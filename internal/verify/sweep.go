package verify

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/springledev/keel/dockerhost"
	"github.com/springledev/keel/runner"
)

// Labels on every throwaway drill container. They say who started it,
// so SweepDrills can tell a drill left behind by a dead process from
// one still running, or from one belonging to another Ballast install
// that shares the Docker host.
const (
	labelDrillOwner = "ballast.drill.owner"
	labelDrillPID   = "ballast.drill.pid"
	labelDrillBoot  = "ballast.drill.boot"
)

// drillNamePrefix starts every throwaway drill container's name.
const drillNamePrefix = "ballast-verify-"

// drillNamePattern is the exact name randomSuffix produces. The
// docker ps name filter matches substrings, so this keeps the sweep off
// any other container that merely contains the prefix.
var drillNamePattern = regexp.MustCompile(`^ballast-verify-[0-9a-f]{6}$`)

// unlabelledDrillMinAge is how old a drill container from a version
// that did not label them must be before the sweep treats it as left
// behind. No label means no way to ask whether its process lives, so
// age stands in: no drill runs this long.
const unlabelledDrillMinAge = 24 * time.Hour

// bootIDPath holds the kernel's identifier for the current boot.
const bootIDPath = "/proc/sys/kernel/random/boot_id"

// OwnerID returns the identifier of this Ballast install, for
// Options.DrillOwner: stable across restarts, and different for another
// machine or another data directory.
func OwnerID(dataDir string) string {
	machine, err := os.ReadFile("/etc/machine-id")
	if err != nil || len(bytes.TrimSpace(machine)) == 0 {
		host, _ := os.Hostname()
		machine = []byte(host)
	}
	if abs, err := filepath.Abs(dataDir); err == nil {
		dataDir = abs
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(string(machine)) + "\x00" + dataDir))
	return hex.EncodeToString(sum[:8])
}

// drillLabelArgs returns the `docker run` label arguments for a drill
// started by this process, or none when no owner is configured.
func drillLabelArgs(owner string) []string {
	if owner == "" {
		return nil
	}
	return []string{
		"--label", labelDrillOwner + "=" + owner,
		"--label", labelDrillPID + "=" + strconv.Itoa(os.Getpid()),
		"--label", labelDrillBoot + "=" + currentBootID(),
	}
}

// currentBootID returns this boot's identifier, or "" where the system
// does not provide one.
func currentBootID() string {
	data, err := os.ReadFile(bootIDPath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// processAlive reports whether pid exists. Permission denied still
// means it exists. A reused pid reads as alive, which only delays the
// removal to a later sweep.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// drillContainer is one row of the sweep's container listing.
type drillContainer struct {
	name, owner, pid, boot, created, networks string
}

// drillSweeper decides and removes; its fields make the decision
// testable without a real reboot, dead process or day-old container.
type drillSweeper struct {
	runner *runner.Runner
	docker dockerhost.Docker
	owner  string
	now    time.Time
	boot   string
	alive  func(pid int) bool
}

// SweepDrills removes throwaway drill containers, and their anonymous
// volumes, that a Ballast process killed outright or lost to a reboot
// could not clean up after itself. It only removes what it can show is
// abandoned, on the Docker host docker points at: a labelled drill must
// belong to owner and have a dead process or an earlier boot, and an
// unlabelled one (from a version before the labels) must have exactly
// the drill name, no network, and be at least a day old. Anything else,
// including a row it cannot read, is left alone.
//
// It returns the names it removed. One failed removal does not stop the
// others, and each failure in the returned error names its container.
func SweepDrills(ctx context.Context, r *runner.Runner, docker dockerhost.Docker, owner string) ([]string, error) {
	s := drillSweeper{runner: r, docker: docker, owner: owner, now: time.Now(), boot: currentBootID(), alive: processAlive}
	return s.sweep(ctx)
}

func (s drillSweeper) sweep(ctx context.Context) ([]string, error) {
	format := strings.Join([]string{
		"{{.Names}}",
		`{{.Label "` + labelDrillOwner + `"}}`,
		`{{.Label "` + labelDrillPID + `"}}`,
		`{{.Label "` + labelDrillBoot + `"}}`,
		"{{.CreatedAt}}",
		"{{.Networks}}",
	}, "\t")
	out, err := s.runner.Run(ctx, nil, nil, s.docker.Command(nil, "ps", "--all", "--filter", "name="+drillNamePrefix, "--format", format))
	if err != nil {
		return nil, fmt.Errorf("listing test containers on Docker host %s: %w", s.docker.Name(), err)
	}
	var (
		removed []string
		errs    []error
	)
	for _, line := range strings.Split(strings.TrimSpace(string(out.Stdout)), "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 6 {
			continue
		}
		c := drillContainer{name: f[0], owner: f[1], pid: f[2], boot: f[3], created: f[4], networks: f[5]}
		if !s.abandoned(c) {
			continue
		}
		if _, err := s.runner.Run(ctx, nil, nil, s.docker.Command(nil, "rm", "--force", "--volumes", c.name)); err != nil {
			errs = append(errs, fmt.Errorf("removing test container %s on Docker host %s (to remove it yourself, run: docker rm --force --volumes %s): %w", c.name, s.docker.Name(), c.name, err))
			continue
		}
		removed = append(removed, c.name)
	}
	return removed, errors.Join(errs...)
}

// abandoned reports whether c is provably a drill nothing is running.
// Every doubt answers false: a leftover is cheap, and removing a live
// drill would fail someone's backup check.
func (s drillSweeper) abandoned(c drillContainer) bool {
	if !drillNamePattern.MatchString(c.name) {
		return false
	}
	if c.owner == "" {
		created, err := time.Parse("2006-01-02 15:04:05 -0700 MST", c.created)
		return err == nil && c.networks == "none" && s.now.Sub(created) >= unlabelledDrillMinAge
	}
	if c.owner != s.owner {
		return false
	}
	if s.boot != "" && c.boot != "" && c.boot != s.boot {
		return true
	}
	pid, err := strconv.Atoi(c.pid)
	return err == nil && pid > 0 && !s.alive(pid)
}

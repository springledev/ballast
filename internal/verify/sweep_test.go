package verify

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/springledev/ballast/internal/strategy"
	"github.com/springledev/keel/dockerhost"
	"github.com/springledev/keel/runner"
)

// TestDrillAbandoned covers every branch of the decision. The ones
// that answer false are the safety of the sweep.
func TestDrillAbandoned(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	s := drillSweeper{owner: "me", now: now, boot: "boot-now", alive: func(pid int) bool { return pid == 100 }}
	const name = "ballast-verify-abc123"
	old, young := "2026-11-04 11:59:00 +0000 UTC", "2026-11-04 12:01:00 +0000 UTC"
	tests := []struct {
		name string
		c    drillContainer
		want bool
	}{
		{"mine, process dead", drillContainer{name: name, owner: "me", pid: "200", boot: "boot-now"}, true},
		{"mine, process alive", drillContainer{name: name, owner: "me", pid: "100", boot: "boot-now"}, false},
		{"mine, earlier boot", drillContainer{name: name, owner: "me", pid: "100", boot: "boot-old"}, true},
		{"mine, no boot label, process alive", drillContainer{name: name, owner: "me", pid: "100"}, false},
		{"mine, no boot label, process dead", drillContainer{name: name, owner: "me", pid: "200"}, true},
		{"mine, unreadable pid", drillContainer{name: name, owner: "me", pid: "x", boot: "boot-now"}, false},
		{"mine, no pid", drillContainer{name: name, owner: "me", boot: "boot-now"}, false},
		{"mine, pid zero", drillContainer{name: name, owner: "me", pid: "0", boot: "boot-now"}, false},
		{"another install, process dead", drillContainer{name: name, owner: "other", pid: "200", boot: "boot-now"}, false},
		{"another install, earlier boot", drillContainer{name: name, owner: "other", pid: "200", boot: "boot-old"}, false},
		{"old version, a day old, no network", drillContainer{name: name, created: old, networks: "none"}, true},
		{"old version, under a day", drillContainer{name: name, created: young, networks: "none"}, false},
		{"old version, has a network", drillContainer{name: name, created: old, networks: "bridge"}, false},
		{"old version, unreadable date", drillContainer{name: name, created: "yesterday", networks: "none"}, false},
		{"name only contains the prefix", drillContainer{name: "my-ballast-verify-abc123", created: old, networks: "none"}, false},
		{"name has a different suffix", drillContainer{name: "ballast-verify-production", created: old, networks: "none"}, false},
		{"labelled, name has a different suffix", drillContainer{name: "ballast-verify-production", owner: "me", pid: "200", boot: "boot-now"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := s.abandoned(tt.c); got != tt.want {
				t.Errorf("abandoned(%+v) = %v, want %v", tt.c, got, tt.want)
			}
		})
	}
}

// sweepStub is a fake docker that lists rows and logs each argv.
func sweepStub(t *testing.T, rows, rmFails string) (*runner.Runner, dockerhost.Docker, string) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "log")
	bin := filepath.Join(dir, "docker")
	script := `#!/bin/sh
printf '%s\n' "$*" >> '` + logPath + `'
case "$1" in
  ps) cat '` + filepath.Join(dir, "rows") + `'; [ -z "$STUB_FAIL_PS" ] || exit 1 ;;
  rm) case "$*" in *'` + rmFails + `'*) [ -z '` + rmFails + `' ] || { echo "rm boom" >&2; exit 1; } ;; esac ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "rows"), []byte(rows), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return &runner.Runner{}, dockerhost.NewLocal(bin), logPath
}

// TestSweepRemovesOnlyAbandoned checks the commands the sweep sends,
// and that one failed removal neither stops the rest nor hides.
func TestSweepRemovesOnlyAbandoned(t *testing.T) {
	const (
		dead  = "ballast-verify-aaaaaa\tme\t200\tb\t2026-11-04 12:00:00 +0000 UTC\tnone\n"
		dead2 = "ballast-verify-bbbbbb\tme\t200\tb\t2026-11-04 12:00:00 +0000 UTC\tnone\n"
		live  = "ballast-verify-cccccc\tme\t100\tb\t2026-11-04 12:00:00 +0000 UTC\tnone\n"
		other = "ballast-verify-dddddd\tyou\t200\tb\t2026-11-04 12:00:00 +0000 UTC\tnone\n"
		bad   = "ballast-verify-eeeeee\tshort row\n"
	)
	tests := []struct {
		name        string
		rows        string
		rmFails     string
		wantRemoved []string
		wantRm      []string
		wantErr     string
	}{
		{name: "nothing listed"},
		{
			name: "only the dead one", rows: live + dead + other + bad,
			wantRemoved: []string{"ballast-verify-aaaaaa"},
			wantRm:      []string{"rm --force --volumes ballast-verify-aaaaaa"},
		},
		{
			name: "a failed removal does not stop the next", rows: dead + dead2, rmFails: "ballast-verify-aaaaaa",
			wantRemoved: []string{"ballast-verify-bbbbbb"},
			wantRm:      []string{"rm --force --volumes ballast-verify-aaaaaa", "rm --force --volumes ballast-verify-bbbbbb"},
			wantErr:     "docker rm --force --volumes ballast-verify-aaaaaa",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, d, logPath := sweepStub(t, tt.rows, tt.rmFails)
			s := drillSweeper{runner: r, docker: d, owner: "me", now: time.Now(), boot: "b", alive: func(pid int) bool { return pid == 100 }}
			removed, err := s.sweep(context.Background())

			if strings.Join(removed, ",") != strings.Join(tt.wantRemoved, ",") {
				t.Errorf("removed = %v, want %v", removed, tt.wantRemoved)
			}
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("error = %v, want none", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Errorf("error = %v, want it to contain %q", err, tt.wantErr)
			}
			log := readStubLog(t, logPath)
			if !strings.HasPrefix(log[0], "ps --all --filter name=ballast-verify- --format ") {
				t.Errorf("first docker invocation = %q, want the listing", log[0])
			}
			if got := log[1:]; strings.Join(got, "|") != strings.Join(tt.wantRm, "|") {
				t.Errorf("removals = %v, want %v", got, tt.wantRm)
			}
		})
	}
}

// TestSweepListingFailure: an unreachable Docker host is an error naming
// the host, and nothing is removed.
func TestSweepListingFailure(t *testing.T) {
	r, d, logPath := sweepStub(t, "ballast-verify-aaaaaa\tme\t200\tb\tx\tnone\n", "")
	r.BaseEnv = append(os.Environ(), "STUB_FAIL_PS=1")
	removed, err := SweepDrills(context.Background(), r, d, "me")
	if err == nil || !strings.Contains(err.Error(), "Docker host local") {
		t.Errorf("error = %v, want it to name the Docker host", err)
	}
	if len(removed) != 0 || len(readStubLog(t, logPath)) != 1 {
		t.Errorf("removed = %v, want nothing removed after a failed listing", removed)
	}
}

// TestOwnerID: the same data directory always gives the same owner, and
// another data directory gives a different one.
func TestOwnerID(t *testing.T) {
	a, b := OwnerID("/var/lib/ballast"), OwnerID("/srv/other")
	if a == "" || a != OwnerID("/var/lib/ballast") {
		t.Errorf("OwnerID is not stable: %q", a)
	}
	if a == b {
		t.Errorf("OwnerID(%q) equals another data directory's", a)
	}
}

// TestContainerRestoreLabelsDrill: with an owner configured the drill
// carries the labels the sweep reads, and without one its argv is plain.
func TestContainerRestoreLabelsDrill(t *testing.T) {
	opts, logPath, _ := stubSetup(t)
	opts.DrillOwner = "me"
	artifact := sqlArtifact(t, "database", "SELECT 1;\n")
	res := runCheck(t, opts, "postgres:16", nil, Request{Artifacts: []strategy.Artifact{artifact}})
	if !res.Passed {
		t.Fatalf("Check() failed: %q", res.Detail)
	}
	run := readStubLog(t, logPath)[0]
	for _, want := range []string{
		"--label ballast.drill.owner=me ",
		"--label ballast.drill.pid=" + strconv.Itoa(os.Getpid()) + " ",
		"--label ballast.drill.boot=" + currentBootID() + " ",
	} {
		if !strings.Contains(run, want) {
			t.Errorf("docker run = %q, want it to contain %q", run, want)
		}
	}
}

// TestSweepRealDocker runs the sweep against real containers. now is
// set to the epoch so nothing counts as a day old: the sweep cannot
// touch an unlabelled container that is not this test's, and the owner
// is unique to this run so no other labelled one matches either.
// Skipped without Docker or in -short mode.
func TestSweepRealDocker(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}
	dockerBin, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("docker not installed; skipping integration test")
	}
	const image = "postgres:16"
	if out, err := exec.Command(dockerBin, "pull", image).CombinedOutput(); err != nil {
		t.Skipf("could not pull %s: %v: %s", image, err, out)
	}
	exited := exec.Command("true")
	if err := exited.Run(); err != nil {
		t.Fatal(err)
	}
	deadPID := strconv.Itoa(exited.Process.Pid)
	owner := "test-" + randomSuffix()
	boot := currentBootID()

	start := func(pid, labelOwner string) (name, volume string) {
		t.Helper()
		name = drillNamePrefix + randomSuffix()
		args := []string{"run", "--detach", "--network", "none", "--name", name, "-e", "POSTGRES_HOST_AUTH_METHOD=trust"}
		if labelOwner != "" {
			args = append(args, "--label", labelDrillOwner+"="+labelOwner, "--label", labelDrillPID+"="+pid, "--label", labelDrillBoot+"="+boot)
		}
		if out, err := exec.Command(dockerBin, append(args, image)...).CombinedOutput(); err != nil {
			t.Fatalf("docker run: %v: %s", err, out)
		}
		t.Cleanup(func() { _ = exec.Command(dockerBin, "rm", "--force", "--volumes", name).Run() })
		out, err := exec.Command(dockerBin, "inspect", "--format", `{{range .Mounts}}{{if eq .Type "volume"}}{{.Name}}{{end}}{{end}}`, name).Output()
		if err != nil || len(strings.TrimSpace(string(out))) == 0 {
			t.Fatalf("finding %s's volume: %v", name, err)
		}
		return name, strings.TrimSpace(string(out))
	}
	exists := func(name string) bool {
		return exec.Command(dockerBin, "inspect", "--type", "container", name).Run() == nil
	}

	dead, deadVolume := start(deadPID, owner)
	live, liveVolume := start(strconv.Itoa(os.Getpid()), owner)
	foreign, _ := start(deadPID, owner+"-other")
	legacy, _ := start("", "")

	s := drillSweeper{runner: &runner.Runner{}, docker: dockerhost.NewLocal(dockerBin), owner: owner, now: time.Unix(0, 0), boot: boot, alive: processAlive}
	removed, err := s.sweep(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if strings.Join(removed, ",") != dead {
		t.Errorf("removed = %v, want only %s", removed, dead)
	}
	if exists(dead) {
		t.Errorf("abandoned container %s is still there", dead)
	}
	if exec.Command(dockerBin, "volume", "inspect", deadVolume).Run() == nil {
		t.Errorf("abandoned container's volume %s is still there", deadVolume)
		_ = exec.Command(dockerBin, "volume", "rm", deadVolume).Run()
	}
	for what, name := range map[string]string{"live drill": live, "another install's drill": foreign, "young unlabelled drill": legacy} {
		if !exists(name) {
			t.Errorf("%s %s was removed", what, name)
		}
	}
	if exec.Command(dockerBin, "volume", "inspect", liveVolume).Run() != nil {
		t.Errorf("live drill's volume %s was removed", liveVolume)
	}

	// The date format the unlabelled rule parses is the one Docker
	// really prints: a real row must read as a day old a day later.
	s.now = time.Now().Add(25 * time.Hour)
	out, err := exec.Command(dockerBin, "ps", "--all", "--filter", "name=^/"+legacy+"$", "--format", "{{.CreatedAt}}\t{{.Networks}}").Output()
	if err != nil {
		t.Fatal(err)
	}
	f := strings.Split(strings.TrimSpace(string(out)), "\t")
	if len(f) != 2 || !s.abandoned(drillContainer{name: legacy, created: f[0], networks: f[1]}) {
		t.Errorf("real row %q did not read as an old unlabelled drill", out)
	}
}

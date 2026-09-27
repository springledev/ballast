// Package detect finds running Docker containers and suggests which
// bundled/user catalog entries they match, for the first-run wizard
// (spec §5.3.6: "detected Docker services → pick one (Immich
// suggested)"). It shells out to `docker` (AGENTS.md rule 4: wrap,
// don't reimplement) and never inspects container internals beyond
// name and image, which is all catalog.DockerDetect.ImagePattern
// needs to match against.
package detect

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/springledev/ballast/internal/catalog"
	"github.com/springledev/ballast/internal/runner"
)

// Container is one running Docker container, as much as detection
// needs to know about it.
type Container struct {
	Name  string `json:"name"`
	Image string `json:"image"`
}

// ListRunning lists every currently running Docker container. An
// error here (commonly: Docker not installed, or the daemon not
// reachable) is the caller's to report — detection is always a
// best-effort convenience, never a hard requirement (a service can
// always be added manually).
func ListRunning(ctx context.Context, r *runner.Runner) ([]Container, error) {
	res, err := r.Run(ctx, nil, nil, runner.Command{
		Name: "docker",
		Args: []string{"ps", "--format", "{{.Names}}\t{{.Image}}"},
	})
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, fmt.Errorf("docker is not installed, or not on $PATH: %w", err)
		}
		return nil, fmt.Errorf("listing running Docker containers: %w", err)
	}
	out := []Container{}
	for _, line := range strings.Split(strings.TrimRight(string(res.Stdout), "\n"), "\n") {
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "\t", 2)
		if len(fields) != 2 {
			continue
		}
		out = append(out, Container{Name: fields[0], Image: fields[1]})
	}
	return out, nil
}

// Suggestion pairs a catalog entry with the running container(s)
// whose image matched its detect.docker.imagePattern.
type Suggestion struct {
	Service    string      `json:"service"`
	Containers []Container `json:"containers"`
}

// Suggest matches every entry with a detect.docker rule against
// containers, by plain case-insensitive substring matching on the
// image name (spec's own bundled examples — "postgres", "mysql", and
// the worked "ghcr.io/immich-app/immich-server" — are all substrings
// of the real image reference, not globs, so no glob engine is
// needed here). Entries with no detect.docker rule (e.g. the generic
// "folder" entry) are never suggested — they are always a manual
// pick, which is correct: nothing about a folder is auto-detectable.
func Suggest(entries []*catalog.Entry, containers []Container) []Suggestion {
	out := []Suggestion{}
	for _, e := range entries {
		if e.Detect.Docker == nil || e.Detect.Docker.ImagePattern == "" {
			continue
		}
		pattern := strings.ToLower(e.Detect.Docker.ImagePattern)
		var matches []Container
		for _, c := range containers {
			if strings.Contains(strings.ToLower(c.Image), pattern) {
				matches = append(matches, c)
			}
		}
		if len(matches) > 0 {
			out = append(out, Suggestion{Service: e.Service, Containers: matches})
		}
	}
	return out
}

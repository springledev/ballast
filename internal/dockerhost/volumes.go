package dockerhost

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/springledev/ballast/internal/runner"
)

// composeProjectLabel is the label Docker Compose sets on every volume
// it creates, naming the compose project the volume belongs to.
const composeProjectLabel = "com.docker.compose.project"

// Volume is one Docker volume, as much as the volume picker needs to
// know about it: enough to show it in a list and let an operator pick
// it by name.
type Volume struct {
	// Name is the volume's name, what a service's mounts.source would
	// reference.
	Name string `json:"name"`
	// Driver is the volume driver ("local" for almost every homelab
	// setup).
	Driver string `json:"driver"`
	// ComposeProject is the Compose project the volume belongs to
	// (from the com.docker.compose.project label), empty for a volume
	// not created by Compose.
	ComposeProject string `json:"compose_project,omitempty"`
}

// dockerVolumeLine is one line of `docker volume ls --format
// '{{json .}}'` output.
type dockerVolumeLine struct {
	Name   string
	Driver string
	Labels string
}

// ListVolumes lists every Docker volume on host d, sorted by name, for
// the volume picker in a strategy's parameter form. An error here
// (commonly: Docker not installed, or the daemon not reachable) is the
// caller's to report — the picker always falls back to a plain text
// box, so listing volumes is a best-effort convenience, never a hard
// requirement.
func ListVolumes(ctx context.Context, r *runner.Runner, d Docker) ([]Volume, error) {
	res, err := r.Run(ctx, nil, nil, d.Command(nil, "volume", "ls", "--format", "{{json .}}"))
	if err != nil {
		if hint := Hint(err, res.Stderr); hint != "" {
			return nil, fmt.Errorf("listing Docker volumes on host %s: %s: %w", d.Name(), hint, err)
		}
		return nil, fmt.Errorf("listing Docker volumes on host %s: %w", d.Name(), err)
	}
	var out []Volume
	for _, line := range strings.Split(strings.TrimRight(string(res.Stdout), "\n"), "\n") {
		if line == "" {
			continue
		}
		var v dockerVolumeLine
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			return nil, fmt.Errorf("listing Docker volumes on host %s: unexpected output: %w", d.Name(), err)
		}
		out = append(out, Volume{
			Name:           v.Name,
			Driver:         v.Driver,
			ComposeProject: parseLabels(v.Labels)[composeProjectLabel],
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// parseLabels parses the docker CLI's "k=v,k=v" label string into a
// map. A label with no "=" is ignored (should not happen in practice,
// but a malformed label must never crash the picker).
func parseLabels(s string) map[string]string {
	out := map[string]string{}
	if s == "" {
		return out
	}
	for _, kv := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		out[k] = v
	}
	return out
}

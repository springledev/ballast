package dockerhost

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/springledev/ballast/internal/runner"
)

// composeConfigFilesLabel is the label Compose sets on every container
// it creates, naming the comma-separated absolute paths of the compose
// files (docker-compose.yml plus any -f/override files) it was started
// from.
const composeConfigFilesLabel = "com.docker.compose.project.config_files"

// composeWorkingDirLabel is the label naming the project's working
// directory — where its compose files (and, by Compose's own
// convention, an unlabelled .env) live.
const composeWorkingDirLabel = "com.docker.compose.project.working_dir"

// composeServiceLabel names the compose service (not container) a
// container implements, e.g. "db" in a project with a "db" and a
// "web" service.
const composeServiceLabel = "com.docker.compose.service"

// ComposeProject is one Docker Compose project discovered on a host,
// for the "compose-project" catalog parameter picker and the compose
// strategy: enough to find its config files and describe its
// containers without ever reading the host filesystem directly.
type ComposeProject struct {
	// Name is the project name (the compose strategy's backup.compose.project).
	Name string `json:"name"`
	// WorkingDir is the project's working directory, as Compose itself
	// recorded it — where docker-compose.yml/.env live.
	WorkingDir string `json:"working_dir"`
	// ConfigFiles are the absolute paths of the compose files the
	// project was last started from.
	ConfigFiles []string `json:"config_files,omitempty"`
	// Services are the compose service names running in the project
	// (e.g. "db", "web"), sorted and deduplicated.
	Services []string `json:"services,omitempty"`
	// Containers are the project's containers, for the compose
	// strategy's labels snapshot. Deliberately excludes environment
	// variables (docker inspect's Config.Env commonly carries secrets
	// such as database passwords) — only name, image and labels, none
	// of which Ballast treats as secret.
	Containers []ComposeContainer `json:"containers,omitempty"`
}

// ComposeContainer is one container belonging to a ComposeProject.
type ComposeContainer struct {
	Name    string            `json:"name"`
	Service string            `json:"service"`
	Image   string            `json:"image"`
	Labels  map[string]string `json:"labels,omitempty"`
}

// dockerContainerLine is one line of `docker ps -a --format
// '{{json .}}'` output.
type dockerContainerLine struct {
	Names  string
	Image  string
	Labels string
}

// ListComposeProjects lists every Docker Compose project on host d —
// found by grouping every container (running or stopped: a stopped
// project's config is still worth snapshotting) by its
// com.docker.compose.project label. A container with no such label
// belongs to no project and is skipped. Listing projects is a
// best-effort convenience like ListVolumes: the caller falls back to a
// plain text box when this fails.
func ListComposeProjects(ctx context.Context, r *runner.Runner, d Docker) ([]ComposeProject, error) {
	res, err := r.Run(ctx, nil, nil, d.Command(nil, "ps", "-a", "--format", "{{json .}}"))
	if err != nil {
		if hint := Hint(err, res.Stderr); hint != "" {
			return nil, fmt.Errorf("listing Docker Compose projects on host %s: %s: %w", d.Name(), hint, err)
		}
		return nil, fmt.Errorf("listing Docker Compose projects on host %s: %w", d.Name(), err)
	}

	byName := map[string]*ComposeProject{}
	for _, line := range strings.Split(strings.TrimRight(string(res.Stdout), "\n"), "\n") {
		if line == "" {
			continue
		}
		var c dockerContainerLine
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			return nil, fmt.Errorf("listing Docker Compose projects on host %s: unexpected output: %w", d.Name(), err)
		}
		labels := parseLabels(c.Labels)
		name := labels[composeProjectLabel]
		if name == "" {
			continue
		}
		p, ok := byName[name]
		if !ok {
			p = &ComposeProject{Name: name, WorkingDir: labels[composeWorkingDirLabel]}
			if cf := labels[composeConfigFilesLabel]; cf != "" {
				p.ConfigFiles = strings.Split(cf, ",")
			}
			byName[name] = p
		}
		svc := labels[composeServiceLabel]
		p.Containers = append(p.Containers, ComposeContainer{
			Name:    c.Names,
			Service: svc,
			Image:   c.Image,
			Labels:  labels,
		})
		if svc != "" && !containsString(p.Services, svc) {
			p.Services = append(p.Services, svc)
		}
	}

	out := make([]ComposeProject, 0, len(byName))
	for _, p := range byName {
		sort.Strings(p.Services)
		sort.Slice(p.Containers, func(i, j int) bool { return p.Containers[i].Name < p.Containers[j].Name })
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// containsString reports whether s is present in list.
func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

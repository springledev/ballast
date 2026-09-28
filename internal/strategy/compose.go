package strategy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/springledev/ballast/internal/catalog"
	"github.com/springledev/ballast/internal/dockerhost"
)

// composeFileNamePattern is the set of file names the compose
// strategy will ever put inside `docker run -v` or a shell argv:
// letters, digits, '.', '_', '-', optionally starting with a single
// leading dot (for ".env"). No slash can appear, so a config file
// path can never smuggle in a path traversal.
var composeFileNamePattern = regexp.MustCompile(`^\.?[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Compose implements the compose strategy (spec §14 step 22): the
// Docker Compose project named in backup.compose.project is looked up
// on the service's Docker host by its containers' compose labels
// (never a host path Ballast is told directly), and its config files
// (docker-compose.yml and any override files it was started from,
// plus .env when present) are archived from the project's working
// directory through a throwaway helper container — the same
// no-host-filesystem-access approach the volume strategy uses (spec
// §17.5), so a remote Docker host needs no other access. Container
// labels (not environment variables, which commonly carry secrets)
// are captured alongside as a second, JSON artifact.
type Compose struct {
	entry *catalog.Entry
	opts  Options
}

// validateComposeEntry checks a compose-strategy entry: a project
// name, and no fields that belong to another strategy.
func validateComposeEntry(entry *catalog.Entry) error {
	b := entry.Backup
	if b.Compose == nil || b.Compose.Project == "" {
		return fmt.Errorf("service %s: compose strategy needs backup.compose.project", entry.Service)
	}
	if len(b.Files) != 0 || len(b.Dumps) != 0 || len(b.Volumes) != 0 || b.StopStart != nil || b.Exec != nil {
		return fmt.Errorf("service %s: compose strategy snapshots one Compose project's config only; other backup.* fields belong to other strategies", entry.Service)
	}
	return nil
}

// Backup finds the named Compose project, archives its config files
// (and .env, when present) into <workdir>/compose-config.tar, and
// writes its containers' names/images/labels into
// <workdir>/compose-labels.json.
func (s *Compose) Backup(ctx context.Context, req Request) (*Result, error) {
	if err := ensureWorkDir(req.WorkDir); err != nil {
		return nil, fmt.Errorf("service %s: %w", s.entry.Service, err)
	}
	project := s.entry.Backup.Compose.Project

	projects, err := dockerhost.ListComposeProjects(ctx, s.opts.Runner, s.opts.Docker)
	if err != nil {
		return nil, fmt.Errorf("service %s: %w", s.entry.Service, err)
	}
	var p *dockerhost.ComposeProject
	for i := range projects {
		if projects[i].Name == project {
			p = &projects[i]
			break
		}
	}
	if p == nil {
		return nil, fmt.Errorf("service %s: compose project %q not found on Docker host %s (check the name with `docker compose ls` or `docker ps`)", s.entry.Service, project, s.opts.Docker.Name())
	}
	if err := validComposeWorkingDir(p.WorkingDir); err != nil {
		return nil, fmt.Errorf("service %s: compose project %s: %w", s.entry.Service, project, err)
	}
	declared, err := composeFileNames(p.ConfigFiles)
	if err != nil {
		return nil, fmt.Errorf("service %s: compose project %s: %w", s.entry.Service, project, err)
	}
	if len(declared) == 0 {
		return nil, fmt.Errorf("service %s: compose project %s has no known config files (its containers carry no compose config-files label)", s.entry.Service, project)
	}
	wanted := append(append([]string(nil), declared...), ".env")

	existing, err := s.probeFiles(ctx, p.WorkingDir, wanted)
	if err != nil {
		return nil, fmt.Errorf("service %s: compose project %s: checking for config files: %w", s.entry.Service, project, err)
	}
	if len(existing) == 0 {
		return nil, fmt.Errorf("service %s: compose project %s: none of its config files (%s) were found under %s on Docker host %s",
			s.entry.Service, project, strings.Join(declared, ", "), p.WorkingDir, s.opts.Docker.Name())
	}

	res := &Result{}
	for _, name := range declared {
		if !slices.Contains(existing, name) {
			res.Skipped = append(res.Skipped, name)
		}
	}

	cfg, err := s.archiveConfig(ctx, req.WorkDir, p.WorkingDir, existing)
	if err != nil {
		return nil, fmt.Errorf("service %s: compose project %s: archiving config: %w", s.entry.Service, project, err)
	}
	res.Artifacts = append(res.Artifacts, cfg)

	labels, err := writeComposeLabels(req.WorkDir, p)
	if err != nil {
		return nil, fmt.Errorf("service %s: compose project %s: writing labels: %w", s.entry.Service, project, err)
	}
	res.Artifacts = append(res.Artifacts, labels)
	return res, nil
}

// probeFiles reports which of names exist as plain files directly
// under workingDir, mounted read-only into a throwaway helper
// container. Names are passed as argv (never interpolated into the
// shell command), so nothing in them — not even a shell metacharacter
// — can execute.
func (s *Compose) probeFiles(ctx context.Context, workingDir string, names []string) ([]string, error) {
	helper := composeHelperName()
	args := append([]string{"run", "--rm", "--name", helper, "--network", "none",
		"-v", workingDir + ":/data:ro", s.opts.volumeHelperImage(),
		"sh", "-c", `cd /data && for f in "$@"; do [ -f "$f" ] && printf '%s\n' "$f"; done`, "sh"}, names...)
	var out bytes.Buffer
	_, err := s.opts.Runner.Run(ctx, nil, &out, s.opts.Docker.Command(nil, args...))
	if err != nil {
		s.opts.removeHelper(helper)
		return nil, err
	}
	var found []string
	for _, line := range strings.Split(strings.TrimRight(out.String(), "\n"), "\n") {
		if line != "" {
			found = append(found, line)
		}
	}
	return found, nil
}

// archiveConfig tars exactly the named files (already confirmed to
// exist by probeFiles) out of workingDir into the artifact file.
func (s *Compose) archiveConfig(ctx context.Context, workDir, workingDir string, names []string) (Artifact, error) {
	path := filepath.Join(workDir, "compose-config.tar")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return Artifact{}, fmt.Errorf("creating archive file: %w", err)
	}
	helper := composeHelperName()
	args := append([]string{"run", "--rm", "--name", helper, "--network", "none",
		"-v", workingDir + ":/data:ro", s.opts.volumeHelperImage(),
		"sh", "-c", `cd /data && tar -cf - "$@"`, "sh"}, names...)
	_, runErr := s.opts.Runner.Run(ctx, nil, f, s.opts.Docker.Command(nil, args...))
	closeErr := f.Close()
	if runErr != nil {
		s.opts.removeHelper(helper)
		return Artifact{}, runErr
	}
	if closeErr != nil {
		return Artifact{}, fmt.Errorf("writing archive file: %w", closeErr)
	}
	return Artifact{Name: "compose-config", Path: path, Format: "tar"}, nil
}

// composeLabelsFile is the JSON shape of compose-labels.json.
type composeLabelsFile struct {
	Project    string                        `json:"project"`
	WorkingDir string                        `json:"working_dir"`
	Containers []dockerhost.ComposeContainer `json:"containers"`
}

// writeComposeLabels captures the project's containers' names, images
// and labels — deliberately never their environment variables, which
// commonly carry secrets such as database passwords (spec rule 2).
func writeComposeLabels(workDir string, p *dockerhost.ComposeProject) (Artifact, error) {
	path := filepath.Join(workDir, "compose-labels.json")
	data, err := json.MarshalIndent(composeLabelsFile{Project: p.Name, WorkingDir: p.WorkingDir, Containers: p.Containers}, "", "  ")
	if err != nil {
		return Artifact{}, fmt.Errorf("encoding labels: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return Artifact{}, fmt.Errorf("writing labels file: %w", err)
	}
	return Artifact{Name: "compose-labels", Path: path, Format: "json"}, nil
}

// composeFileNames returns the deduplicated base names of configFiles
// (a Compose project's config_files label, comma-split by the
// caller), rejecting any name outside composeFileNamePattern — a
// defence in depth against a config-files label Ballast did not
// generate itself ever reaching a shell argv unsafely.
func composeFileNames(configFiles []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, cf := range configFiles {
		name := filepath.Base(strings.TrimSpace(cf))
		if !composeFileNamePattern.MatchString(name) {
			return nil, fmt.Errorf("config file %q has an unsafe name %q", cf, name)
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out, nil
}

// validComposeWorkingDir rejects a working directory the compose
// strategy cannot safely bind-mount: empty (no label found), relative
// (Docker's `-v` requires an absolute host path), or containing a
// colon (which `-v SRC:DST` would otherwise parse as a second field).
func validComposeWorkingDir(dir string) error {
	if dir == "" {
		return fmt.Errorf("has no known working directory (its containers carry no compose working-dir label)")
	}
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("working directory %q is not an absolute path", dir)
	}
	if strings.Contains(dir, ":") {
		return fmt.Errorf("working directory %q contains ':', which a Docker bind mount cannot express", dir)
	}
	return nil
}

// composeHelperName returns a unique helper container name, so a
// failed run can remove exactly the container it started.
func composeHelperName() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return "ballast-compose-" + hex.EncodeToString(b[:])
}

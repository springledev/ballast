package strategy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/springledev/ballast/internal/catalog"
	"github.com/springledev/ballast/internal/dockerhost"
	"github.com/springledev/ballast/internal/runner"
)

// composeEntry builds a compose-strategy entry for the named project.
func composeEntry(project string) *catalog.Entry {
	return &catalog.Entry{Service: "stack", Backup: catalog.Backup{
		Strategy: "compose",
		Compose:  &catalog.ComposeSpec{Project: project},
	}}
}

// composeDockerStub is a fake docker CLI answering `docker ps -a
// --format {{json .}}` with a fixed project (or an empty list) and
// `docker run ... sh -c '...' sh <names>` per the compose strategy's
// two call shapes: the probe (echoes names that are in $STUB_EXISTS)
// and the tar (writes a fixed payload, only when every requested name
// was in $STUB_EXISTS — otherwise it fails loudly, so a test can tell
// the strategy never asked to tar a name the probe didn't confirm).
const composeDockerStub = `#!/bin/sh
printf '%s\n' "$*" >> "$STUB_LOG"
if [ "$1 $2 $3 $4" = "ps -a --format {{json .}}" ]; then
  cat "$STUB_PS_OUTPUT"
  exit 0
fi
if [ "$1" = "run" ]; then
  shift
  while [ "$#" -gt 0 ]; do
    if [ "$1" = "-c" ]; then
      shift
      script="$1"
      shift
      shift # discard the "sh" $0 placeholder
      break
    fi
    shift
  done
  if [ "$script" = 'cd /data && for f in "$@"; do [ -f "$f" ] && printf '"'"'%s\n'"'"' "$f"; done' ]; then
    for f in "$@"; do
      for e in $STUB_EXISTS; do
        [ "$f" = "$e" ] && printf '%s\n' "$f"
      done
    done
    exit 0
  fi
  if [ "$script" = 'cd /data && tar -cf - "$@"' ]; then
    for f in "$@"; do
      ok=0
      for e in $STUB_EXISTS; do
        [ "$f" = "$e" ] && ok=1
      done
      if [ "$ok" = 0 ]; then
        echo "stub: tar asked for $f, which the probe never confirmed" >&2
        exit 1
      fi
    done
    printf '%s' "$STUB_TAR_PAYLOAD"
    exit 0
  fi
  echo "stub: unrecognized run script: $script" >&2
  exit 1
fi
if [ "$1" = "rm" ]; then
  exit 0
fi
echo "stub: unhandled command: $*" >&2
exit 1
`

// composeStub wires a docker stub that lists one project (via psJSON,
// one `docker ps -a --format {{json .}}` line per container) and
// answers probe/tar calls using exists (space-separated file names)
// and payload.
func composeStub(t *testing.T, psJSON []string, exists, payload string) (Options, string) {
	t.Helper()
	dir := t.TempDir()
	bin := writeStub(t, dir, "docker-stub", composeDockerStub)
	logPath := filepath.Join(dir, "docker.log")
	psPath := filepath.Join(dir, "ps.json")
	if err := os.WriteFile(psPath, []byte(strings.Join(psJSON, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &runner.Runner{BaseEnv: append(os.Environ(),
		"STUB_LOG="+logPath,
		"STUB_PS_OUTPUT="+psPath,
		"STUB_EXISTS="+exists,
		"STUB_TAR_PAYLOAD="+payload,
	)}
	return Options{Runner: r, Docker: dockerhost.NewLocal(bin)}, logPath
}

func immichPSLines() []string {
	return []string{
		`{"Names":"immich_server","Image":"immich/server","Labels":"com.docker.compose.project=immich,com.docker.compose.service=server,com.docker.compose.project.working_dir=/opt/immich,com.docker.compose.project.config_files=/opt/immich/docker-compose.yml"}`,
		`{"Names":"immich_db","Image":"immich/postgres","Labels":"com.docker.compose.project=immich,com.docker.compose.service=database,com.docker.compose.project.working_dir=/opt/immich,com.docker.compose.project.config_files=/opt/immich/docker-compose.yml"}`,
	}
}

func TestComposeValidation(t *testing.T) {
	opts := Options{Runner: &runner.Runner{}}
	tests := []struct {
		name    string
		entry   *catalog.Entry
		opts    Options
		wantErr string
	}{
		{"ok", composeEntry("immich"), opts, ""},
		{"no runner", composeEntry("immich"), Options{}, "needs a command runner"},
		{"no project", composeEntry(""), opts, "needs backup.compose.project"},
		{"nil compose", &catalog.Entry{Service: "stack", Backup: catalog.Backup{Strategy: "compose"}}, opts, "needs backup.compose.project"},
		{"files not allowed", func() *catalog.Entry {
			e := composeEntry("immich")
			e.Backup.Files = []catalog.FileSpec{{Path: "/srv"}}
			return e
		}(), opts, "one Compose project's config only"},
		{"volumes not allowed", func() *catalog.Entry {
			e := composeEntry("immich")
			e.Backup.Volumes = []catalog.VolumeSpec{{Name: "data"}}
			return e
		}(), opts, "one Compose project's config only"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := For(tt.entry, tt.opts)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("For: unexpected error %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("For error = %v, want it to contain %q", err, tt.wantErr)
			}
			if !strings.Contains(err.Error(), "stack") {
				t.Errorf("error %q does not name the service", err)
			}
		})
	}
}

func TestComposeBackup(t *testing.T) {
	opts, _ := composeStub(t, immichPSLines(), "docker-compose.yml .env", "TARBYTES")
	s, err := For(composeEntry("immich"), opts)
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	work := t.TempDir()
	res, err := s.Backup(context.Background(), Request{WorkDir: work})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if len(res.Artifacts) != 2 {
		t.Fatalf("artifacts = %+v, want 2", res.Artifacts)
	}
	if len(res.Skipped) != 0 {
		t.Errorf("skipped = %v, want none (every declared file existed)", res.Skipped)
	}

	cfg := res.Artifacts[0]
	if cfg.Name != "compose-config" || cfg.Format != "tar" || cfg.Path != filepath.Join(work, "compose-config.tar") {
		t.Errorf("config artifact = %+v", cfg)
	}
	if body, _ := os.ReadFile(cfg.Path); string(body) != "TARBYTES" {
		t.Errorf("config artifact body = %q, want TARBYTES", body)
	}
	if info, err := os.Stat(cfg.Path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("config artifact mode = %v, %v, want 0600", info, err)
	}

	labels := res.Artifacts[1]
	if labels.Name != "compose-labels" || labels.Format != "json" {
		t.Errorf("labels artifact = %+v", labels)
	}
	var decoded composeLabelsFile
	data, err := os.ReadFile(labels.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("decoding labels json: %v", err)
	}
	if decoded.Project != "immich" || decoded.WorkingDir != "/opt/immich" {
		t.Errorf("labels = %+v", decoded)
	}
	if len(decoded.Containers) != 2 {
		t.Fatalf("containers = %+v, want 2", decoded.Containers)
	}
	for _, c := range decoded.Containers {
		if _, ok := c.Labels["com.docker.compose.project"]; !ok {
			t.Errorf("container %s missing project label", c.Name)
		}
	}
}

func TestComposeBackupMissingEnvIsNotAnError(t *testing.T) {
	// .env is optional: only docker-compose.yml exists this time.
	opts, _ := composeStub(t, immichPSLines(), "docker-compose.yml", "TARBYTES")
	s, err := For(composeEntry("immich"), opts)
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	res, err := s.Backup(context.Background(), Request{WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if len(res.Skipped) != 0 {
		t.Errorf("skipped = %v, want none (.env is optional, not declared)", res.Skipped)
	}
}

func TestComposeBackupProjectNotFound(t *testing.T) {
	opts, _ := composeStub(t, immichPSLines(), "docker-compose.yml", "x")
	s, err := For(composeEntry("nope"), opts)
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	_, err = s.Backup(context.Background(), Request{WorkDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), `compose project "nope" not found`) {
		t.Fatalf("Backup error = %v, want project-not-found", err)
	}
}

func TestComposeBackupNoConfigFilesFound(t *testing.T) {
	// docker-compose.yml is declared but nothing exists on disk.
	opts, _ := composeStub(t, immichPSLines(), "", "x")
	s, err := For(composeEntry("immich"), opts)
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	_, err = s.Backup(context.Background(), Request{WorkDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "none of its config files") {
		t.Fatalf("Backup error = %v, want none-found error", err)
	}
}

func TestComposeFileNames(t *testing.T) {
	tests := []struct {
		name    string
		in      []string
		want    []string
		wantErr string
	}{
		{"basic", []string{"/opt/immich/docker-compose.yml"}, []string{"docker-compose.yml"}, ""},
		{"dedup", []string{"/a/x.yml", "/b/x.yml"}, []string{"x.yml"}, ""},
		{"path traversal is reduced to a base name", []string{"/opt/../../etc/passwd"}, []string{"passwd"}, ""},
		{"unsafe base name", []string{"/opt/$(rm -rf /)"}, nil, "unsafe name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := composeFileNames(tt.in)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("composeFileNames error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("composeFileNames: %v", err)
			}
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("composeFileNames(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

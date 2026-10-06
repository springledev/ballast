package strategy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/springledev/ballast/internal/catalog"
	"github.com/springledev/keel/dockerhost"
	"github.com/springledev/keel/runner"
)

// hostPathStub wires the docker stub with a tar payload and a canned
// `docker inspect` answer.
func hostPathStub(t *testing.T, inspect string) (Options, string) {
	t.Helper()
	bin, logPath := newDockerStub(t)
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "payload.tar")
	inspPath := filepath.Join(dir, "inspect.json")
	for p, c := range map[string]string{tarPath: "TARBYTES", inspPath: inspect} {
		if err := os.WriteFile(p, []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r := &runner.Runner{BaseEnv: append(os.Environ(), "STUB_LOG="+logPath, "STUB_TAR="+tarPath, "STUB_INSPECT="+inspPath)}
	return Options{Runner: r, Docker: dockerhost.NewLocal(bin)}, logPath
}

const mountsJSON = `[{"Type":"bind","Source":"/root/docker/vw/vw-data","Destination":"/data","RW":true},` +
	`{"Type":"volume","Name":"cache","Source":"/var/lib/docker/volumes/cache/_data","Destination":"/cache","RW":true}]`

func hostEntry(strategyName string, containers []string, specs ...catalog.HostPathSpec) *catalog.Entry {
	e := &catalog.Entry{Service: "app", Backup: catalog.Backup{Strategy: strategyName, HostPaths: specs}}
	if len(containers) > 0 {
		e.Backup.StopStart = &catalog.StopStart{Containers: containers}
	}
	return e
}

// TestHostPathValidation covers For's static checks of hostPaths.
func TestHostPathValidation(t *testing.T) {
	opts := Options{Runner: &runner.Runner{}}
	p := func(path string) catalog.HostPathSpec { return catalog.HostPathSpec{Path: path} }
	tests := []struct {
		name    string
		entry   *catalog.Entry
		wantErr string
	}{
		{"absolute path", hostEntry("files", nil, p("/srv/data")), ""},
		{"mount", hostEntry("stop-start", []string{"web"}, catalog.HostPathSpec{Container: "web", Mount: "/data"}), ""},
		{"relative", hostEntry("files", nil, p("data")), "must be absolute"},
		{"whole disk", hostEntry("files", nil, p("/")), "whole disk"},
		{"comma could split --mount", hostEntry("files", nil, p("/srv/a,source=/etc")), "commas"},
		{"empty entry", hostEntry("files", nil, catalog.HostPathSpec{}), "needs either a path"},
		{"path and mount", hostEntry("files", nil, catalog.HostPathSpec{Path: "/a", Mount: "/data", Container: "c"}), "both a path"},
		{"relative mount", hostEntry("files", nil, catalog.HostPathSpec{Container: "c", Mount: "data"}), "absolute path inside the container"},
		{"mixed with files", func() *catalog.Entry {
			e := hostEntry("files", nil, p("/srv"))
			e.Backup.Files = []catalog.FileSpec{{Path: "/x"}}
			return e
		}(), "cannot be mixed"},
		{"volume strategy refuses", hostEntry("volume", nil, p("/srv")), "volume strategy archives Docker volumes only"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.entry.Backup.Strategy == "volume" {
				tt.entry.Backup.Volumes = []catalog.VolumeSpec{{Name: "data"}}
			}
			_, err := For(tt.entry, opts)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("For: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) || !strings.Contains(err.Error(), "app") {
				t.Fatalf("For error = %v, want it to name the service and contain %q", err, tt.wantErr)
			}
		})
	}
}

// TestHostPathBackup runs files and stop-start hostPaths against the
// docker stub: the remote path is never stat'ed locally, validation
// happens before anything is stopped, and the container is always
// started again.
func TestHostPathBackup(t *testing.T) {
	tests := []struct {
		name       string
		strategy   string
		containers []string
		specs      []catalog.HostPathSpec
		wantCmds   []string
		wantErr    string
	}{
		{
			name: "live files", strategy: "files",
			specs:    []catalog.HostPathSpec{{Path: "/remote/only/a"}, {Path: "/remote/only/b"}},
			wantCmds: []string{"run", "run", "run"},
		},
		{
			name: "stop-start checks first, then stop, tar, start", strategy: "stop-start", containers: []string{"web"},
			specs:    []catalog.HostPathSpec{{Path: "/remote/only/a"}},
			wantCmds: []string{"run", "stop web", "run", "start web"},
		},
		{
			name: "missing folder stops nothing", strategy: "stop-start", containers: []string{"web"},
			specs:    []catalog.HostPathSpec{{Path: "/remote/only/fail-run"}},
			wantCmds: []string{"run", "rm"},
			wantErr:  "was not found on Docker host local",
		},
		{
			name: "second folder missing stops nothing", strategy: "stop-start", containers: []string{"web"},
			specs:    []catalog.HostPathSpec{{Path: "/remote/only/ok"}, {Path: "/remote/only/fail-run"}},
			wantCmds: []string{"run", "run", "rm"},
			wantErr:  "was not found",
		},
		{
			name: "mount point resolved from inspect", strategy: "stop-start", containers: []string{"web"},
			specs:    []catalog.HostPathSpec{{Container: "web", Mount: "/data"}},
			wantCmds: []string{"inspect --format", "run", "stop web", "run", "start web"},
		},
		{
			name: "unknown mount point stops nothing", strategy: "stop-start", containers: []string{"web"},
			specs:    []catalog.HostPathSpec{{Container: "web", Mount: "/nope"}},
			wantCmds: []string{"inspect --format"},
			wantErr:  "nothing mounted at /nope",
		},
		{
			name: "mount on a volume is refused", strategy: "files",
			specs:    []catalog.HostPathSpec{{Container: "web", Mount: "/cache"}},
			wantCmds: []string{"inspect --format"},
			wantErr:  "volume strategy",
		},
		{
			name: "duplicate folder names", strategy: "files",
			specs:    []catalog.HostPathSpec{{Path: "/a/data"}, {Path: "/b/data"}},
			wantCmds: nil,
			wantErr:  "share the folder name",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts, logPath := hostPathStub(t, mountsJSON)
			s, err := For(hostEntry(tt.strategy, tt.containers, tt.specs...), opts)
			if err != nil {
				t.Fatalf("For: %v", err)
			}
			work := t.TempDir()
			res, err := s.Backup(context.Background(), Request{WorkDir: work})
			got := commands(readStubLog(t, logPath))
			if strings.Join(got, "|") != strings.Join(tt.wantCmds, "|") {
				t.Errorf("docker commands = %v, want %v", got, tt.wantCmds)
			}
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) || !strings.Contains(err.Error(), "service app") {
					t.Fatalf("Backup error = %v, want service app and %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Backup: %v", err)
			}
			if len(res.Artifacts) != 1 || res.Artifacts[0].Name != "data" || res.Artifacts[0].Format != "tar" {
				t.Fatalf("artifacts = %+v", res.Artifacts)
			}
			data, err := os.ReadFile(res.Artifacts[0].Path)
			if err != nil || string(data) != "TARBYTES" {
				t.Errorf("archive = %q, %v", data, err)
			}
			info, _ := os.Stat(res.Artifacts[0].Path)
			if info.Mode().Perm() != 0o600 {
				t.Errorf("archive mode = %v, want 0600", info.Mode().Perm())
			}
			// Folders are mounted read-only, never with -v (which would
			// create a missing source on the host).
			joined := strings.Join(readStubLog(t, logPath), "\n")
			if strings.Contains(joined, " -v ") || !strings.Contains(joined, ",readonly") {
				t.Errorf("helper must use read-only --mount, got:\n%s", joined)
			}
		})
	}
}

// TestHostPathPreflight proves Preflight reports a bad folder without
// stopping anything.
func TestHostPathPreflight(t *testing.T) {
	opts, logPath := hostPathStub(t, mountsJSON)
	s, err := For(hostEntry("stop-start", []string{"web"}, catalog.HostPathSpec{Path: "/x/fail-run"}), opts)
	if err != nil {
		t.Fatal(err)
	}
	pf, ok := s.(Preflighter)
	if !ok {
		t.Fatal("stop-start strategy does not implement Preflighter")
	}
	if err := pf.Preflight(context.Background()); err == nil || !strings.Contains(err.Error(), "nothing was stopped") {
		t.Fatalf("Preflight error = %v", err)
	}
	for _, c := range commands(readStubLog(t, logPath)) {
		if strings.HasPrefix(c, "stop") {
			t.Errorf("Preflight stopped a container: %s", c)
		}
	}
}

// TestContainerBindMounts lists only bind mounts, for the form's
// auto-fill.
func TestContainerBindMounts(t *testing.T) {
	opts, _ := hostPathStub(t, mountsJSON)
	got, err := ContainerBindMounts(context.Background(), opts, "web")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Source != "/root/docker/vw/vw-data" || got[0].Destination != "/data" || got[0].ReadOnly {
		t.Fatalf("mounts = %+v", got)
	}
	if _, err := ContainerBindMounts(context.Background(), opts, "fail-inspect"); err == nil || !strings.Contains(err.Error(), "fail-inspect") {
		t.Errorf("error = %v, want it to name the container", err)
	}
}

// TestRestoreHostPath checks that restore goes through the helper, to
// a new folder unless overwriting was confirmed.
func TestRestoreHostPath(t *testing.T) {
	tests := []struct {
		name      string
		dest      string
		overwrite bool
		wantCmds  []string
		wantErr   string
	}{
		{"new folder", "/restore/fail-run", false, []string{"run", "rm", "run"}, ""},
		{"existing folder refused", "/restore/there", false, []string{"run"}, "already exists"},
		{"existing folder confirmed", "/restore/there", true, []string{"run", "run"}, ""},
		{"relative refused", "restore", false, nil, "must be absolute"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts, logPath := volumeStub(t, "")
			err := RestoreHostPath(context.Background(), opts, tt.dest, strings.NewReader("tar"), tt.overwrite)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if got := commands(readStubLog(t, logPath)); strings.Join(got, "|") != strings.Join(tt.wantCmds, "|") {
				t.Errorf("commands = %v, want %v", got, tt.wantCmds)
			}
		})
	}
}

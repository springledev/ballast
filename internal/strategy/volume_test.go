package strategy

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/springledev/ballast/internal/catalog"
	"github.com/springledev/keel/dockerhost"
	"github.com/springledev/keel/runner"
)

// volumeEntry builds a volume-strategy entry; with containers it runs
// in stop→tar→start mode, without it live.
func volumeEntry(volumes []string, containers ...string) *catalog.Entry {
	e := &catalog.Entry{Service: "app", Backup: catalog.Backup{Strategy: "volume"}}
	for _, v := range volumes {
		e.Backup.Volumes = append(e.Backup.Volumes, catalog.VolumeSpec{Name: v})
	}
	if len(containers) > 0 {
		e.Backup.StopStart = &catalog.StopStart{Containers: containers}
	}
	return e
}

// volumeStub returns strategy options wired to the docker stub, which
// answers `docker run` with the given tar payload, plus the stub's log.
func volumeStub(t *testing.T, payload string) (Options, string) {
	t.Helper()
	bin, logPath := newDockerStub(t)
	tarPath := filepath.Join(t.TempDir(), "payload.tar")
	if err := os.WriteFile(tarPath, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &runner.Runner{BaseEnv: append(os.Environ(), "STUB_LOG="+logPath, "STUB_TAR="+tarPath)}
	return Options{Runner: r, Docker: dockerhost.NewLocal(bin)}, logPath
}

// commands reduces the stub log to "subcommand first-arg" pairs (e.g.
// "stop web", "run", "volume inspect"), the order the tests assert.
func commands(log []string) []string {
	out := make([]string, 0, len(log))
	for _, l := range log {
		f := strings.Fields(l)
		switch {
		case len(f) == 0:
		case f[0] == "run" || f[0] == "rm":
			out = append(out, f[0])
		case len(f) >= 3 && f[0] == "volume":
			out = append(out, "inspect "+f[2])
		case len(f) >= 2:
			out = append(out, f[0]+" "+f[1])
		}
	}
	return out
}

// TestVolumeValidation covers For's checks for the volume strategy,
// in particular that host paths can never reach `-v`.
func TestVolumeValidation(t *testing.T) {
	opts := Options{Runner: &runner.Runner{}}
	tests := []struct {
		name    string
		entry   *catalog.Entry
		opts    Options
		wantErr string
	}{
		{"live ok", volumeEntry([]string{"app_data"}), opts, ""},
		{"stop ok", volumeEntry([]string{"app_data", "app.db-1"}, "web"), opts, ""},
		{"no runner", volumeEntry([]string{"app_data"}), Options{}, "needs a command runner"},
		{"no volumes", volumeEntry(nil), opts, "at least one backup.volumes"},
		{"host path", volumeEntry([]string{"/etc"}), opts, "not a Docker volume name"},
		{"relative path", volumeEntry([]string{"data/sub"}), opts, "not a Docker volume name"},
		{"option injection", volumeEntry([]string{"-v"}), opts, "not a Docker volume name"},
		{"too short", volumeEntry([]string{"a"}), opts, "not a Docker volume name"},
		{"empty", volumeEntry([]string{""}), opts, "not a Docker volume name"},
		{"duplicate", volumeEntry([]string{"data", "data"}), opts, "listed twice"},
		{"empty stopStart", func() *catalog.Entry {
			e := volumeEntry([]string{"data"})
			e.Backup.StopStart = &catalog.StopStart{}
			return e
		}(), opts, "lists no containers"},
		{"files not allowed", func() *catalog.Entry {
			e := volumeEntry([]string{"data"})
			e.Backup.Files = []catalog.FileSpec{{Path: "/srv"}}
			return e
		}(), opts, "Docker volumes only"},
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
			if !strings.Contains(err.Error(), "app") {
				t.Errorf("error %q does not name the service", err)
			}
		})
	}
}

// TestVolumeBackup runs the strategy against the docker stub in both
// consistency modes and through each failure point, asserting the
// docker command order and, above all, that stopped containers are
// always started again.
func TestVolumeBackup(t *testing.T) {
	tests := []struct {
		name       string
		volumes    []string
		containers []string
		wantCmds   []string
		wantErr    []string
	}{
		{
			name:     "live",
			volumes:  []string{"app_data", "app_media"},
			wantCmds: []string{"inspect app_data", "inspect app_media", "run", "run"},
		},
		{
			name:       "stop then start",
			volumes:    []string{"app_data"},
			containers: []string{"web", "worker"},
			wantCmds:   []string{"inspect app_data", "stop web", "stop worker", "run", "start web", "start worker"},
		},
		{
			name:       "missing volume stops nothing",
			volumes:    []string{"fail-volume"},
			containers: []string{"web"},
			wantCmds:   []string{"inspect fail-volume"},
			wantErr:    []string{"volume fail-volume not found", "Docker host local"},
		},
		{
			name:       "tar failure still restarts and removes the helper",
			volumes:    []string{"fail-run"},
			containers: []string{"web"},
			wantCmds:   []string{"inspect fail-run", "stop web", "run", "rm", "start web"},
			wantErr:    []string{"archiving volume fail-run"},
		},
		{
			name:       "failed stop restarts what was stopped",
			volumes:    []string{"data"},
			containers: []string{"web", "fail-stop"},
			wantCmds:   []string{"inspect data", "stop web", "stop fail-stop", "start web"},
			wantErr:    []string{"stopping container fail-stop"},
		},
		{
			name:       "restart failure is reported",
			volumes:    []string{"data"},
			containers: []string{"fail-start"},
			wantCmds:   []string{"inspect data", "stop fail-start", "run", "start fail-start"},
			wantErr:    []string{"starting containers again"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts, logPath := volumeStub(t, "TARBYTES")
			s, err := For(volumeEntry(tt.volumes, tt.containers...), opts)
			if err != nil {
				t.Fatalf("For: %v", err)
			}
			work := t.TempDir()
			res, err := s.Backup(context.Background(), Request{WorkDir: work})

			if got := commands(readStubLog(t, logPath)); strings.Join(got, "|") != strings.Join(tt.wantCmds, "|") {
				t.Errorf("docker commands = %v, want %v", got, tt.wantCmds)
			}
			if len(tt.wantErr) > 0 {
				if err == nil {
					t.Fatal("Backup succeeded, want an error")
				}
				for _, w := range tt.wantErr {
					if !strings.Contains(err.Error(), w) {
						t.Errorf("error %q does not contain %q", err, w)
					}
				}
				if !strings.Contains(err.Error(), "service app") {
					t.Errorf("error %q does not name the service", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Backup: %v", err)
			}
			if len(res.Artifacts) != len(tt.volumes) || len(res.Paths) != 0 {
				t.Fatalf("result = %+v, want one artifact per volume and no paths", res)
			}
			for i, v := range tt.volumes {
				a := res.Artifacts[i]
				if a.Name != "volume-"+v || a.Format != "tar" || a.Path != filepath.Join(work, "volume-"+v+".tar") {
					t.Errorf("artifact %d = %+v", i, a)
				}
				info, err := os.Stat(a.Path)
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != 0o600 {
					t.Errorf("artifact mode = %04o, want 0600", info.Mode().Perm())
				}
				if body, _ := os.ReadFile(a.Path); string(body) != "TARBYTES" {
					t.Errorf("artifact body = %q, want the helper's stdout", body)
				}
			}
		})
	}
}

// TestVolumeHelperCommand pins the helper container's safety flags:
// read-only mount, no network, removed afterwards, configurable image.
func TestVolumeHelperCommand(t *testing.T) {
	opts, logPath := volumeStub(t, "x")
	opts.VolumeHelperImage = "busybox:1"
	s, err := For(volumeEntry([]string{"app_data"}), opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Backup(context.Background(), Request{WorkDir: t.TempDir()}); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	var run string
	for _, l := range readStubLog(t, logPath) {
		if strings.HasPrefix(l, "run ") {
			run = l
		}
	}
	for _, want := range []string{"--rm", "--network none", "-v app_data:/data:ro", "busybox:1 tar -cf - -C /data ."} {
		if !strings.Contains(run, want) {
			t.Errorf("helper command %q lacks %q", run, want)
		}
	}
}

// TestRestoreVolume checks the restore side: the archive is fed on
// stdin to a writable mount, and names that are not volumes are
// refused before docker is ever called.
func TestRestoreVolume(t *testing.T) {
	tests := []struct {
		name    string
		volume  string
		wantErr string
	}{
		{"ok", "app_data", ""},
		{"host path refused", "/var/lib", "not a Docker volume name"},
		{"docker failure", "fail-run", "restoring volume fail-run on Docker host local"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bin, logPath := newDockerStub(t)
			stdin := filepath.Join(t.TempDir(), "stdin")
			r := &runner.Runner{BaseEnv: append(os.Environ(), "STUB_LOG="+logPath, "STUB_STDIN="+stdin)}
			opts := Options{Runner: r, Docker: dockerhost.NewLocal(bin)}

			err := RestoreVolume(context.Background(), opts, tt.volume, strings.NewReader("ARCHIVE"))
			log := readStubLog(t, logPath)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				if strings.HasPrefix(tt.volume, "/") && len(log) != 0 {
					t.Errorf("docker was called for an invalid name: %v", log)
				}
				return
			}
			if err != nil {
				t.Fatalf("RestoreVolume: %v", err)
			}
			if len(log) != 1 || !strings.Contains(log[0], "-v app_data:/data "+DefaultVolumeHelperImage+" tar -xpf - -C /data") || !strings.Contains(log[0], " -i ") {
				t.Errorf("restore command = %v", log)
			}
			if got, _ := os.ReadFile(stdin); string(got) != "ARCHIVE" {
				t.Errorf("helper stdin = %q, want the archive", got)
			}
		})
	}
}

// TestRemoveHelper: removing a helper by hand bypasses --rm's own volume
// cleanup, so it must ask for the volumes too; a failed removal is
// logged, never returned over the caller's original failure.
func TestRemoveHelper(t *testing.T) {
	tests := []struct {
		name    string
		helper  string
		wantLog string
	}{
		{name: "removes the anonymous volumes too", helper: "ballast-volume-x"},
		{name: "failed removal is logged", helper: "fail-rm", wantLog: "docker rm -f --volumes fail-rm"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts, logPath := volumeStub(t, "TARBYTES")
			var buf bytes.Buffer
			log.SetOutput(&buf)
			defer log.SetOutput(os.Stderr)

			opts.removeHelper(tt.helper)

			want := "rm -f --volumes " + tt.helper
			if stub := readStubLog(t, logPath); len(stub) != 1 || stub[0] != want {
				t.Errorf("docker commands = %v, want %q", stub, want)
			}
			if tt.wantLog == "" && buf.Len() != 0 {
				t.Errorf("log = %q, want nothing on success", buf.String())
			}
			if !strings.Contains(buf.String(), tt.wantLog) {
				t.Errorf("log = %q, want it to contain %q", buf.String(), tt.wantLog)
			}
		})
	}
}

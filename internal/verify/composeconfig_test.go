package verify

import (
	"archive/tar"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/springledev/ballast/internal/catalog"
	"github.com/springledev/ballast/internal/strategy"
	"github.com/springledev/keel/dockerhost"
	"github.com/springledev/keel/runner"
)

// writeComposeSnapshot writes a tar of files plus a manifest naming
// captured and returns the two artifacts.
func writeComposeSnapshot(t *testing.T, files map[string]string, captured []string) []strategy.Artifact {
	t.Helper()
	dir := t.TempDir()
	tp := filepath.Join(dir, "compose-config.tar")
	f, err := os.Create(tp)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	f.Close()
	mp := filepath.Join(dir, "compose-manifest.json")
	manifest := `{"config_files_merge_order":["compose.yaml"],"captured_files":["` + strings.Join(captured, `","`) + `"]}`
	if err := os.WriteFile(mp, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	return []strategy.Artifact{{Name: "compose-config", Path: tp, Format: "tar"}, {Name: "compose-manifest", Path: mp, Format: "json"}}
}

func TestComposeConfigCheck(t *testing.T) {
	stub := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$STUB_LOG\"\nexit \"$STUB_EXIT\"\n"
	tests := []struct {
		name     string
		files    map[string]string
		captured []string
		exit     string
		noArtif  bool
		wantPass bool
		wantText string
	}{
		{"parses", map[string]string{"compose.yaml": "services: {}", "sub/a.env": "A=1"}, []string{"compose.yaml", "sub/a.env"}, "0", false, true, "read all 2"},
		{"compose rejects", map[string]string{"compose.yaml": "x"}, []string{"compose.yaml"}, "1", false, false, "docker compose config"},
		{"file missing from archive", map[string]string{"compose.yaml": "x"}, []string{"compose.yaml", "app.env"}, "0", false, false, "app.env is listed"},
		{"no snapshot", nil, nil, "0", true, false, "no compose snapshot"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "docker-stub")
			if err := os.WriteFile(bin, []byte(stub), 0o755); err != nil {
				t.Fatal(err)
			}
			log := filepath.Join(dir, "log")
			r := &runner.Runner{BaseEnv: append(os.Environ(), "STUB_LOG="+log, "STUB_EXIT="+tt.exit)}
			c, err := newComposeConfig(&catalog.Entry{Service: "stack"}, Options{Runner: r, Docker: dockerhost.NewLocal(bin)})
			if err != nil {
				t.Fatal(err)
			}
			var arts []strategy.Artifact
			if !tt.noArtif {
				arts = writeComposeSnapshot(t, tt.files, tt.captured)
			}
			res := c.Check(context.Background(), Request{Service: "stack", Artifacts: arts})
			if res.Passed != tt.wantPass || !strings.Contains(res.Detail, tt.wantText) {
				t.Errorf("passed=%v detail=%q, want passed=%v containing %q", res.Passed, res.Detail, tt.wantPass, tt.wantText)
			}
		})
	}
}

func TestComposeConfigNeedsRunner(t *testing.T) {
	if _, err := newComposeConfig(&catalog.Entry{Service: "stack"}, Options{}); err == nil {
		t.Fatal("want error without a runner")
	}
}

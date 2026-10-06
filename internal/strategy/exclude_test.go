package strategy

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/springledev/ballast/internal/catalog"
	"github.com/springledev/keel/dockerhost"
	"github.com/springledev/keel/runner"
)

func TestExcluded(t *testing.T) {
	tests := []struct {
		name     string
		patterns []string
		rel      string
		want     bool
	}{
		{"no patterns", nil, "a/b.log", false},
		{"name anywhere", []string{"*.log"}, "a/b/c.log", true},
		{"name at top", []string{"*.log"}, "c.log", true},
		{"not matching", []string{"*.log"}, "c.txt", false},
		{"folder name", []string{"node_modules"}, "node_modules", true},
		{"nested folder name", []string{"node_modules"}, "app/node_modules", true},
		{"slash pattern matches suffix", []string{"cache/thumbs"}, "x/cache/thumbs", true},
		{"slash pattern needs whole components", []string{"cache/thumbs"}, "x/mycache/thumbs", false},
		{"anchored at top", []string{"/tmp"}, "tmp", true},
		{"anchored not deeper", []string{"/tmp"}, "a/tmp", false},
		{"anchored wildcard", []string{"/cache/*"}, "cache/x", true},
		{"trailing slash ignored", []string{"tmp/"}, "a/tmp", true},
		{"root itself never", []string{"*"}, "", false},
		{"second pattern", []string{"x", "y"}, "d/y", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := excluded(tt.patterns, tt.rel); got != tt.want {
				t.Errorf("excluded(%v, %q) = %v, want %v", tt.patterns, tt.rel, got, tt.want)
			}
		})
	}
}

func TestTarExcludeArgs(t *testing.T) {
	got := tarExcludeArgs("data", []string{"*.log", "/tmp"})
	want := []string{
		"--exclude", "./data/*.log", "--exclude", "./data/*.log/*", "--exclude", "./data/*/*.log", "--exclude", "./data/*/*.log/*",
		"--exclude", "./data/tmp", "--exclude", "./data/tmp/*",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tarExcludeArgs = %v, want %v", got, want)
	}
	if tarExcludeArgs("data", nil) != nil {
		t.Error("no patterns must give no arguments")
	}
}

// excludeFixture builds a folder with files to keep and to skip.
func excludeFixture(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "app")
	for p, body := range map[string]string{
		"keep.txt":              "keep",
		"debug.log":             "0123456789",
		"sub/keep2.txt":         "kept too",
		"sub/trace.log":         "abc",
		"node_modules/pkg/a.js": "12345",
		"node_modules/pkg/b.js": "123",
		"cache/thumbs/t1.jpg":   "jjjj",
		"cache/other.dat":       "dd",
	} {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestTarOneHonoursExcludes(t *testing.T) {
	root := excludeFixture(t)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	var skipped []string
	if err := tarOne(context.Background(), tw, root, []string{"*.log", "node_modules", "cache/thumbs"}, &skipped); err != nil {
		t.Fatalf("tarOne: %v", err)
	}
	tw.Close()
	var names []string
	tr := tar.NewReader(&buf)
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		names = append(names, h.Name)
	}
	sort.Strings(names)
	want := []string{"app/", "app/cache/", "app/cache/other.dat", "app/keep.txt", "app/sub/", "app/sub/keep2.txt"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("archive holds %v, want %v", names, want)
	}
}

func TestPreviewExcludes(t *testing.T) {
	root := excludeFixture(t)
	entry := &catalog.Entry{Service: "docs", Backup: catalog.Backup{
		Strategy: "files",
		Files:    []catalog.FileSpec{{Path: root, Exclude: []string{"*.log", "node_modules"}}},
	}}
	pv, err := PreviewExcludes(entry)
	if err != nil {
		t.Fatalf("PreviewExcludes: %v", err)
	}
	if pv.SkippedCount != 3 || pv.SkippedBytes != 10+3+8 {
		t.Errorf("skipped = %d items / %d bytes, want 3 / 21", pv.SkippedCount, pv.SkippedBytes)
	}
	if pv.KeptBytes != 4+8+4+2 {
		t.Errorf("kept = %d bytes, want 18", pv.KeptBytes)
	}
	if len(pv.Skipped) != 3 || !strings.HasSuffix(pv.Skipped[0].Path, "debug.log") && !strings.HasSuffix(pv.Skipped[0].Path, "node_modules") {
		t.Errorf("skipped list = %+v", pv.Skipped)
	}
	if pv.Skipped[0].Bytes < pv.Skipped[1].Bytes {
		t.Errorf("biggest must come first: %+v", pv.Skipped)
	}

	if _, err := PreviewExcludes(&catalog.Entry{Service: "x", Backup: catalog.Backup{Files: []catalog.FileSpec{{Path: "rel"}}}}); err == nil {
		t.Error("a relative path must be rejected")
	}
	withHost := &catalog.Entry{Service: "x", Backup: catalog.Backup{HostPaths: []catalog.HostPathSpec{{Path: "/a"}}}}
	pv, err = PreviewExcludes(withHost)
	if err != nil || pv.Note == "" {
		t.Errorf("host paths must be noted as not previewable: %+v, %v", pv, err)
	}
}

func TestFilesStrategyReturnsExcludes(t *testing.T) {
	root := excludeFixture(t)
	e := &catalog.Entry{Service: "docs", Backup: catalog.Backup{Strategy: "files", Files: []catalog.FileSpec{
		{Path: root, Exclude: []string{"*.log"}}, {Path: root, Exclude: []string{"cache"}},
	}}}
	s, err := For(e, Options{})
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	res, err := s.Backup(context.Background(), Request{WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if !reflect.DeepEqual(res.Excludes, []string{"*.log", "cache"}) {
		t.Errorf("Excludes = %v", res.Excludes)
	}
}

func TestHostPathBackupPassesExcludes(t *testing.T) {
	opts, logPath := hostPathStub(t, mountsJSON)
	e := hostEntry("files", nil, catalog.HostPathSpec{Path: "/remote/only/a", Exclude: []string{"*.log"}})
	s, err := For(e, opts)
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	if _, err := s.Backup(context.Background(), Request{WorkDir: t.TempDir()}); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	joined := strings.Join(readStubLog(t, logPath), "\n")
	if !strings.Contains(joined, "--exclude ./a/*.log") || !strings.Contains(joined, "tar -cf - --exclude") {
		t.Errorf("helper tar must carry the excludes before -C, got:\n%s", joined)
	}
}

// TestHostPathExcludesRealDocker proves the helper's own tar skips what
// the patterns say, on a real Docker daemon.
func TestHostPathExcludesRealDocker(t *testing.T) {
	if testing.Short() {
		t.Skip("needs Docker")
	}
	if err := exec.Command("docker", "image", "inspect", DefaultVolumeHelperImage).Run(); err != nil {
		t.Skipf("Docker or %s not available: %v", DefaultVolumeHelperImage, err)
	}
	root := excludeFixture(t)
	opts := Options{Runner: &runner.Runner{BaseEnv: os.Environ()}, Docker: dockerhost.NewLocal("docker")}
	srcs := []hostSource{{Source: root, Name: "app", Exclude: []string{"*.log", "node_modules", "cache/thumbs"}}}
	art, err := opts.archiveHostPaths(context.Background(), t.TempDir(), srcs)
	if err != nil {
		t.Fatalf("archiveHostPaths: %v", err)
	}
	f, err := os.Open(art.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var names []string
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		if !h.FileInfo().IsDir() {
			names = append(names, strings.TrimPrefix(h.Name, "./"))
		}
	}
	sort.Strings(names)
	want := []string{"app/cache/other.dat", "app/keep.txt", "app/sub/keep2.txt"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("helper archive holds %v, want %v", names, want)
	}
}

package api

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestSelfBackupStagesAndVerifies(t *testing.T) {
	h := newHarness(t)
	h.addDestination("local")
	h.ops.WorkRoot = filepath.Join(t.TempDir(), "work")

	src := t.TempDir()
	cfg := filepath.Join(src, "ballast.yaml")
	cat := filepath.Join(src, "catalog.d")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(cfg, []byte("max_parallel_jobs: 2\n"), 0o600))
	must(os.MkdirAll(filepath.Join(cat, "sub"), 0o700))
	must(os.WriteFile(filepath.Join(cat, "mine.yaml"), []byte("x: 1\n"), 0o600))
	must(os.WriteFile(filepath.Join(cat, "sub", "deep.yaml"), []byte("y: 2\n"), 0o600))
	h.ops.SelfBackupFiles = SelfBackupSources{ConfigFile: cfg, CatalogDirs: []string{cat, filepath.Join(src, "missing")}}

	res, err := h.ops.SelfBackup(context.Background(), "local")
	if err != nil {
		t.Fatalf("SelfBackup: %v", err)
	}
	if !res.Verified || res.SnapshotID == "" {
		t.Fatalf("result = %+v, want verified with a snapshot id", res)
	}
	sort.Strings(res.Files)
	want := []string{"ballast.db", "ballast.yaml", "catalog.d/0/mine.yaml", "catalog.d/0/sub/deep.yaml"}
	if strings.Join(res.Files, ",") != strings.Join(want, ",") {
		t.Fatalf("files = %v, want %v", res.Files, want)
	}
	log := strings.Join(h.resticInvocations(t), "\n")
	if !strings.Contains(log, "backup") || !strings.Contains(log, "--tag "+SelfBackupTag) {
		t.Fatalf("restic calls missing tagged backup:\n%s", log)
	}
	if _, err := os.Stat(filepath.Join(h.ops.WorkRoot, "_self")); !os.IsNotExist(err) {
		t.Fatalf("staging directory left behind: %v", err)
	}
}

func TestSelfBackupErrors(t *testing.T) {
	h := newHarness(t)
	if _, err := h.ops.SelfBackup(context.Background(), "local"); err == nil {
		t.Fatal("no work root: want error")
	}
	h.ops.WorkRoot = t.TempDir()
	if _, err := h.ops.SelfBackup(context.Background(), "ghost"); err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("unknown destination error = %v", err)
	}
}

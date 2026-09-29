package strategy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/springledev/ballast/internal/catalog"
	"github.com/springledev/keel/runner"
)

// execEntry wraps a stub script in a minimal exec catalog entry.
func execEntry(script string, args ...string) *catalog.Entry {
	return &catalog.Entry{
		Service: "phone",
		Backup: catalog.Backup{
			Strategy: "exec",
			Exec:     &catalog.ExecBackup{Script: script, Args: args},
		},
	}
}

// TestExecContract runs the Phase-2 reserved contract with a stub
// script: the script learns the artifact dir via BALLAST_ARTIFACT_DIR,
// drops its payload there, and every produced file becomes an
// artifact. Request env and catalog args reach the script.
func TestExecContract(t *testing.T) {
	dir := t.TempDir()
	stub := writeStub(t, dir, "phone-backup", `#!/bin/sh
printf 'payload for %s with %s\n' "$1" "$MARKER" > "$BALLAST_ARTIFACT_DIR/backup.bin"
printf 'meta\n' > "$BALLAST_ARTIFACT_DIR/backup.info"
`)
	s, err := For(execEntry(stub, "arg-one"), Options{Runner: &runner.Runner{}})
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	workDir := t.TempDir()
	res, err := s.Backup(context.Background(), Request{
		WorkDir: workDir,
		Env:     []string{"MARKER=from-request-env"},
	})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if len(res.Artifacts) != 2 {
		t.Fatalf("Artifacts = %v, want the two files the script produced", res.Artifacts)
	}
	byName := map[string]string{}
	for _, a := range res.Artifacts {
		byName[a.Name] = a.Path
	}
	content, err := os.ReadFile(byName["backup.bin"])
	if err != nil {
		t.Fatalf("reading artifact: %v", err)
	}
	if string(content) != "payload for arg-one with from-request-env\n" {
		t.Errorf("artifact content = %q, want script args and request env visible", content)
	}
	if filepath.Dir(byName["backup.bin"]) != workDir {
		t.Errorf("artifact path = %s, want it inside the work dir %s", byName["backup.bin"], workDir)
	}
}

// TestExecScriptFailure fails the stub script and checks the error
// names the service and the script.
func TestExecScriptFailure(t *testing.T) {
	dir := t.TempDir()
	stub := writeStub(t, dir, "broken", "#!/bin/sh\necho nope >&2\nexit 1\n")
	s, err := For(execEntry(stub), Options{Runner: &runner.Runner{}})
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	_, err = s.Backup(context.Background(), Request{WorkDir: t.TempDir()})
	if err == nil {
		t.Fatal("Backup = nil error, want the script failure")
	}
	if !strings.Contains(err.Error(), "phone") || !strings.Contains(err.Error(), stub) {
		t.Errorf("error = %q, want it to name the service and the script", err)
	}
}

// TestExecNoArtifactsIsAFailure enforces the contract: exit 0 means
// an artifact exists. A script that produces nothing must fail
// loudly, or Ballast would report a backup of nothing.
func TestExecNoArtifactsIsAFailure(t *testing.T) {
	dir := t.TempDir()
	stub := writeStub(t, dir, "silent", "#!/bin/sh\nexit 0\n")
	s, err := For(execEntry(stub), Options{Runner: &runner.Runner{}})
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	_, err = s.Backup(context.Background(), Request{WorkDir: t.TempDir()})
	if err == nil {
		t.Fatal("Backup = nil error, want the empty-artifact contract violation")
	}
	if !strings.Contains(err.Error(), "left no artifacts") {
		t.Errorf("error = %q, want it to explain the contract violation", err)
	}
}

// TestExecRejectsEmptyWorkDir covers the work-dir validation branch.
func TestExecRejectsEmptyWorkDir(t *testing.T) {
	dir := t.TempDir()
	stub := writeStub(t, dir, "noop", "#!/bin/sh\nexit 0\n")
	s, err := For(execEntry(stub), Options{Runner: &runner.Runner{}})
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	if _, err := s.Backup(context.Background(), Request{}); err == nil {
		t.Error("Backup with empty work dir = nil error, want failure")
	}
}

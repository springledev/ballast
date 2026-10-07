package verify

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/springledev/ballast/internal/catalog"
	"github.com/springledev/ballast/internal/strategy"
	"github.com/springledev/keel/runner"
)

func execScriptEntry(verifyScript string) *catalog.Entry {
	return &catalog.Entry{
		Service: "phone",
		Backup:  catalog.Backup{Strategy: "exec", Exec: &catalog.ExecBackup{Script: "/bin/true", VerifyScript: verifyScript}},
		Verify:  catalog.Verify{Method: MethodExecScript},
	}
}

func TestExecScript(t *testing.T) {
	dir := t.TempDir()
	artifact := filepath.Join(dir, "backup.bin")
	if err := os.WriteFile(artifact, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	okScript := filepath.Join(t.TempDir(), "ok.sh")
	failScript := filepath.Join(t.TempDir(), "fail.sh")
	if err := os.WriteFile(okScript, []byte("#!/bin/sh\ntest -f \"$BALLAST_ARTIFACT_DIR/backup.bin\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(failScript, []byte("#!/bin/sh\necho 'table missing' >&2\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	arts := []strategy.Artifact{{Name: "backup.bin", Path: artifact}}

	tests := []struct {
		name         string
		script       string
		artifacts    []strategy.Artifact
		wantPassed   bool
		wantUnverif  bool
		wantInDetail string
	}{
		{"exit 0 verifies and sees the artifact dir", okScript, arts, true, false, "exited 0"},
		{"non-zero exit fails", failScript, arts, false, false, "did not succeed"},
		{"no verify script is amber", "", arts, false, true, "no verify script"},
		{"no artifacts fails", okScript, nil, false, false, "no files"},
		{"missing script fails", filepath.Join(dir, "nope.sh"), arts, false, false, "did not succeed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkers, err := For(execScriptEntry(tt.script), Options{Runner: &runner.Runner{}})
			if err != nil {
				t.Fatalf("For: %v", err)
			}
			res := checkers[0].Check(context.Background(), Request{Service: "phone", Artifacts: tt.artifacts})
			if res.Passed != tt.wantPassed || res.Unverifiable != tt.wantUnverif {
				t.Errorf("Passed=%v Unverifiable=%v, want %v/%v (%s)", res.Passed, res.Unverifiable, tt.wantPassed, tt.wantUnverif, res.Detail)
			}
			if !strings.Contains(res.Detail, tt.wantInDetail) {
				t.Errorf("Detail = %q, want it to contain %q", res.Detail, tt.wantInDetail)
			}
		})
	}
}

func TestExecScriptNeedsExecStrategy(t *testing.T) {
	_, err := For(&catalog.Entry{Service: "x", Verify: catalog.Verify{Method: MethodExecScript}}, Options{Runner: &runner.Runner{}})
	if err == nil {
		t.Fatal("want an error for exec-script on a non-exec entry")
	}
}

package protondrive

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/springledev/keel/runner"
)

// fakeCLI is a stand-in for Proton's command-line tool that keeps a
// "drive" in a local folder. Its output shapes follow the plan's
// assumptions about the real tool (see the step-49 notes); CI needs no
// Proton account. Behaviour switches come from environment variables:
// FAKE_SIGNED_OUT, FAKE_SHORT (info reports one byte too few) and
// FAKE_CORRUPT (downloads differ from the upload).
const fakeCLI = "testdata/fake-proton-drive.sh"

// newFake writes the fake CLI and returns a Client using it plus the
// folder that stands in for Proton Drive.
func newFake(t *testing.T) (*Client, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "drive"), 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "proton-drive")
	script, err := os.ReadFile(fakeCLI)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, script, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_ROOT", root)
	return &Client{Bin: bin, StateDir: filepath.Join(t.TempDir(), "proton-drive", "p"), Runner: &runner.Runner{}}, root
}

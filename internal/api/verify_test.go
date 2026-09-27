package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/springledev/ballast/internal/catalog"
	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/restic"
	"github.com/springledev/ballast/internal/runner"
)

func TestOpsVerifyServiceZeroArtifactFoldsIntoReadBack(t *testing.T) {
	h := newHarness(t)
	h.addDestination("local")
	if err := inventory.PutService(h.ops.DB, inventory.Service{Name: "svc", CatalogEntry: "folder", Destination: "local", Params: map[string]string{"path": t.TempDir()}}); err != nil {
		t.Fatalf("PutService: %v", err)
	}

	rep, err := h.ops.VerifyService(context.Background(), "svc")
	if err != nil {
		t.Fatalf("VerifyService: %v", err)
	}
	if rep.Light != "green" {
		t.Fatalf("Light = %q, want green (report: %+v)", rep.Light, rep)
	}
	if rep.SnapshotID != "11111111" {
		t.Errorf("SnapshotID = %q, want the stub snapshot's short id", rep.SnapshotID)
	}
	var sawReadBack, sawArchiveIntegrity bool
	for _, c := range rep.Checks {
		if c.Method == "restic-read-back" {
			sawReadBack = true
			if !c.Passed {
				t.Errorf("restic-read-back check: Passed = false, want true")
			}
		}
		if c.Method == "archive-integrity" {
			sawArchiveIntegrity = true
		}
	}
	if !sawReadBack {
		t.Errorf("checks = %+v, want a restic-read-back result", rep.Checks)
	}
	if !sawArchiveIntegrity {
		t.Errorf("checks = %+v, want archive-integrity folded into the read-back result (spec §14 step 11 user decision)", rep.Checks)
	}
}

// TestOpsVerifyServiceReadBackFailureAmber pins the "unverifiable, not
// green" half of the traffic light (spec §14 step 11 user decision,
// mirroring pipeline.TestRunReadBackFailureAmber): a zero-artifact
// service whose restic read-back cannot be performed must come back
// amber, and the folded archive-integrity result must report the same
// failure, never a false pass.
func TestOpsVerifyServiceReadBackFailureAmber(t *testing.T) {
	h := newHarness(t)
	h.addDestination("local")
	if err := inventory.PutService(h.ops.DB, inventory.Service{Name: "svc", CatalogEntry: "folder", Destination: "local", Params: map[string]string{"path": t.TempDir()}}); err != nil {
		t.Fatalf("PutService: %v", err)
	}
	t.Setenv("STUB_FAIL_CMD", "ls")

	rep, err := h.ops.VerifyService(context.Background(), "svc")
	if err != nil {
		t.Fatalf("VerifyService: %v", err)
	}
	if rep.Light != "amber" {
		t.Fatalf("Light = %q, want amber (report: %+v)", rep.Light, rep)
	}
	for _, c := range rep.Checks {
		if c.Passed {
			t.Errorf("check %+v: Passed = true, want false (ls was made to fail)", c)
		}
	}
}

// verifyRedResticStub is a dedicated fake restic CLI for
// TestOpsVerifyServiceFailedCheckerRed: unlike the shared
// resticStubScript, its "restore" writes corrupt content to the
// requested artifact path so archive-integrity's declared check has
// something real (not zero artifacts) to fail against, while the
// read-back (ls/dump) still succeeds — isolating a genuine checker
// failure from a read-back failure.
const verifyRedResticStub = `#!/bin/sh
printf '%s\n' "$*" >> "$STUB_LOG"
cmd="$1"
case "$cmd" in
  snapshots)
    printf '[{"time":"2024-01-01T00:00:00Z","id":"3333333333333333333333333333333333333333333333333333333333333333","short_id":"33333333","hostname":"test","tags":["ballast","ballast-service:svc"],"paths":["__ARTIFACT_PATH__"]}]\n'
    ;;
  ls)
    printf '{"message_type":"snapshot","time":"2024-01-01T00:00:00Z","id":"3333333333333333333333333333333333333333333333333333333333333333","short_id":"33333333","hostname":"test","paths":["__ARTIFACT_PATH__"]}\n'
    printf '{"message_type":"node","name":"dump.tar","type":"file","path":"__ARTIFACT_PATH__","size":11}\n'
    ;;
  dump) printf 'sample-content' ;;
  restore)
    shift
    target=""
    prev=""
    for arg in "$@"; do
      if [ "$prev" = "--target" ]; then target="$arg"; fi
      prev="$arg"
    done
    dest="$target__ARTIFACT_PATH__"
    mkdir -p "$(dirname "$dest")"
    printf 'not a tar file, just garbage bytes' > "$dest"
    ;;
  *) echo "unknown stub restic command: $cmd" >&2; exit 2 ;;
esac
exit 0
`

// TestOpsVerifyServiceFailedCheckerRed pins the "a real failed check
// is red, not amber" half of the traffic light: the declared
// archive-integrity checker runs against a real restored artifact
// (spec §14 step 11 folds the check into read-back only when there
// are zero artifacts — this one has one), and a corrupt one must fail
// the whole service red even though restic's own read-back succeeds.
func TestOpsVerifyServiceFailedCheckerRed(t *testing.T) {
	h := newHarness(t)
	h.addDestination("local")

	workRoot := t.TempDir()
	artifactPath := filepath.Join(workRoot, "svc", "dump.tar")

	script := strings.ReplaceAll(verifyRedResticStub, "__ARTIFACT_PATH__", artifactPath)
	stubDir := t.TempDir()
	stubBin := filepath.Join(stubDir, "restic-stub")
	if err := os.WriteFile(stubBin, []byte(script), 0o755); err != nil {
		t.Fatalf("writing restic stub: %v", err)
	}
	h.ops.Repo = func(dest inventory.Destination) (*restic.Repository, error) {
		return restic.Open(dest.Location, restic.Options{
			Runner:       &runner.Runner{BaseEnv: append(os.Environ(), "STUB_LOG="+filepath.Join(h.credDir, "restic.log"))},
			PasswordFile: filepath.Join(h.credDir, dest.PasswordCredential),
			ResticBin:    stubBin,
		})
	}
	h.ops.WorkRoot = workRoot

	entryYAML := `
apiVersion: ballast.dev/v1
service: svc
version: 1
backup:
  strategy: dump
  dumps:
    - name: dump
      container: stubcontainer
      command: ["true"]
      format: tar
verify:
  method: archive-integrity
`
	fsys := fstest.MapFS{"svc.yaml": &fstest.MapFile{Data: []byte(entryYAML)}}
	reg, err := catalog.LoadRegistry(fsys, nil)
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	h.ops.Catalog = reg

	if err := inventory.PutService(h.ops.DB, inventory.Service{Name: "svc", CatalogEntry: "svc", Destination: "local"}); err != nil {
		t.Fatalf("PutService: %v", err)
	}

	rep, err := h.ops.VerifyService(context.Background(), "svc")
	if err != nil {
		t.Fatalf("VerifyService: %v", err)
	}
	if rep.Light != "red" {
		t.Fatalf("Light = %q, want red (report: %+v)", rep.Light, rep)
	}
	var sawFailedArchiveIntegrity, sawPassedReadBack bool
	for _, c := range rep.Checks {
		if c.Method == "archive-integrity" && !c.Passed {
			sawFailedArchiveIntegrity = true
		}
		if c.Method == "restic-read-back" && c.Passed {
			sawPassedReadBack = true
		}
	}
	if !sawFailedArchiveIntegrity {
		t.Errorf("checks = %+v, want a failed archive-integrity result (the restored file is corrupt)", rep.Checks)
	}
	if !sawPassedReadBack {
		t.Errorf("checks = %+v, want restic-read-back to still pass (a checker failure must not mask read-back succeeding)", rep.Checks)
	}
}

func TestOpsVerifyServiceNoSnapshots(t *testing.T) {
	h := newHarness(t)
	h.addDestination("local")
	h.runner.state = "" // irrelevant: VerifyService never runs a backup
	// Make the stub report no snapshots at all for this destination.
	h.ops.Repo = func(dest inventory.Destination) (*restic.Repository, error) {
		return nil, errors.New("no repository builder configured for this test")
	}
	if err := inventory.PutService(h.ops.DB, inventory.Service{Name: "svc", CatalogEntry: "folder", Destination: "local", Params: map[string]string{"path": t.TempDir()}}); err != nil {
		t.Fatalf("PutService: %v", err)
	}

	if _, err := h.ops.VerifyService(context.Background(), "svc"); err == nil {
		t.Fatal("VerifyService with a broken repository: want an error")
	}
}

func TestOpsVerifyServiceUnknownService(t *testing.T) {
	h := newHarness(t)
	if _, err := h.ops.VerifyService(context.Background(), "ghost"); err == nil {
		t.Fatal("VerifyService for an unconfigured service: want an error")
	}
}

func TestOpsVerifyAllServicesIsolatesFailures(t *testing.T) {
	h := newHarness(t)
	h.addDestination("local")
	if err := inventory.PutService(h.ops.DB, inventory.Service{Name: "good", CatalogEntry: "folder", Destination: "local", Params: map[string]string{"path": t.TempDir()}}); err != nil {
		t.Fatalf("PutService: %v", err)
	}
	if err := inventory.PutService(h.ops.DB, inventory.Service{Name: "bad", CatalogEntry: "no-such-entry", Destination: "local"}); err != nil {
		t.Fatalf("PutService: %v", err)
	}

	reports, err := h.ops.VerifyAllServices(context.Background())
	if err == nil {
		t.Fatal("VerifyAllServices with one broken service: want a non-nil error")
	}
	if !strings.Contains(err.Error(), "bad") {
		t.Errorf("error = %v, want it to name the broken service", err)
	}
	if len(reports) != 2 {
		t.Fatalf("reports = %+v, want 2 (one per configured service, isolated from each other)", reports)
	}
	var goodLight, badLight string
	for _, r := range reports {
		switch r.Service {
		case "good":
			goodLight = r.Light
		case "bad":
			badLight = r.Light
		}
	}
	if goodLight != "green" {
		t.Errorf("good service light = %q, want green (one broken service must not affect it)", goodLight)
	}
	if badLight != "red" {
		t.Errorf("bad service light = %q, want red", badLight)
	}
}

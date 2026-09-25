package api

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/springledev/ballast/internal/catalog"
	"github.com/springledev/ballast/internal/engine"
	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/restic"
	"github.com/springledev/ballast/internal/runner"
	"github.com/springledev/ballast/internal/scheduler"
	"github.com/springledev/ballast/internal/store"
)

// resticStubScript is a minimal, hermetic fake restic CLI shared by
// every Ops test that needs a repository: init/backup/snapshots/ls/
// dump/forget all succeed against one fixed fake snapshot.
const resticStubScript = `#!/bin/sh
printf '%s\n' "$*" >> "$STUB_LOG"
cmd="$1"
if [ -n "$STUB_FAIL_CMD" ] && [ "$cmd" = "$STUB_FAIL_CMD" ]; then
  echo "stub: simulated restic $cmd failure" >&2
  exit 1
fi
case "$cmd" in
  init) exit 0 ;;
  backup)
    printf '{"message_type":"summary","snapshot_id":"1111111111111111111111111111111111111111111111111111111111111111","files_new":1,"total_files_processed":1}\n'
    ;;
  snapshots)
    printf '[{"time":"2024-01-01T00:00:00Z","id":"1111111111111111111111111111111111111111111111111111111111111111","short_id":"11111111","hostname":"test","tags":["ballast","ballast-service:svc"],"paths":["/stub/data"]}]\n'
    ;;
  ls)
    printf '{"message_type":"snapshot","time":"2024-01-01T00:00:00Z","id":"1111111111111111111111111111111111111111111111111111111111111111","short_id":"11111111","hostname":"test","paths":["/stub/data"]}\n'
    printf '{"message_type":"node","name":"sample.txt","type":"file","path":"/stub/data/sample.txt","size":11}\n'
    ;;
  dump) printf 'sample-content' ;;
  restore) exit 0 ;;
  forget) printf '[]' ;;
  *) echo "unknown stub restic command: $cmd" >&2; exit 2 ;;
esac
exit 0
`

func writeResticStub(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "restic-stub")
	if err := os.WriteFile(path, []byte(resticStubScript), 0o755); err != nil {
		t.Fatalf("writing restic stub: %v", err)
	}
	return path
}

// spyRunner is a stub scheduler.Runner recording every service run.
type spyRunner struct {
	mu    sync.Mutex
	ran   []string
	state engine.State
	err   error
	block chan struct{} // if set, Run blocks until this is closed
}

func (r *spyRunner) Run(_ context.Context, service string) (engine.State, error) {
	r.mu.Lock()
	r.ran = append(r.ran, service)
	block := r.block
	r.mu.Unlock()
	if block != nil {
		<-block
	}
	state := r.state
	if state == "" {
		state = engine.SUCCESS
	}
	return state, r.err
}

const folderEntryYAML = `
apiVersion: ballast.dev/v1
service: folder
version: 1
parameters:
  - name: path
    required: true
backup:
  strategy: files
  files:
    - path: "{{ .Params.path }}"
verify:
  method: archive-integrity
`

// harness bundles a fresh Ops with a stub restic binary and stub
// scheduler.Runner for table-driven Ops tests.
type harness struct {
	t            *testing.T
	ops          *Ops
	runner       *spyRunner
	resticBin    string
	credDir      string
	locks        scheduler.FileLocker
	triggerToken string // scratch field http_test.go's golden cases stash a generated token in
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "ballast.db"))
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	resticBin := writeResticStub(t)
	credDir := t.TempDir()

	fsys := fstest.MapFS{"folder.yaml": &fstest.MapFile{Data: []byte(folderEntryYAML)}}
	reg, err := catalog.LoadRegistry(fsys, nil)
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}

	r := &spyRunner{}
	locks := scheduler.FileLocker{Dir: filepath.Join(t.TempDir(), "locks")}
	coord := &scheduler.Coordinator{DB: s.DB(), Locks: locks, Runner: r, LockWait: -1}

	ops := &Ops{
		DB:      s.DB(),
		Catalog: reg,
		Coord:   coord,
		Repo: func(dest inventory.Destination) (*restic.Repository, error) {
			return restic.Open(dest.Location, restic.Options{
				Runner:       &runner.Runner{BaseEnv: append(os.Environ(), "STUB_LOG="+filepath.Join(credDir, "restic.log"))},
				PasswordFile: filepath.Join(credDir, dest.PasswordCredential),
				ResticBin:    resticBin,
			})
		},
	}
	return &harness{t: t, ops: ops, runner: r, resticBin: resticBin, credDir: credDir, locks: locks}
}

// holdRunLock simulates a live run of service, so a Start/Backup call
// refuses with scheduler.ErrAlreadyRunning.
func (h *harness) holdRunLock(t *testing.T, service string) {
	t.Helper()
	release, ok, err := h.locks.TryLock(service)
	if err != nil || !ok {
		t.Fatalf("holding run lock for %s: %v, %v", service, ok, err)
	}
	t.Cleanup(release)
}

// addDestination configures a destination "local" with a working
// password credential file already in place.
func (h *harness) addDestination(name string) {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(h.credDir, "ballast-restic-password."+name), []byte("pw\n"), 0o600); err != nil {
		h.t.Fatal(err)
	}
	if err := inventory.PutDestination(h.ops.DB, inventory.Destination{
		Name: name, Location: "/stub-repo", KeepDaily: 7, KeepWeekly: 4, KeepMonthly: 6,
	}); err != nil {
		h.t.Fatalf("PutDestination: %v", err)
	}
}

func TestOpsPutServiceValidatesCatalog(t *testing.T) {
	h := newHarness(t)
	h.addDestination("local")

	tests := []struct {
		name    string
		svc     inventory.Service
		wantErr string
	}{
		{"unknown catalog entry", inventory.Service{Name: "svc", CatalogEntry: "ghost", Destination: "local"}, "catalog entry"},
		{"missing required param", inventory.Service{Name: "svc", CatalogEntry: "folder", Destination: "local"}, "missing required parameter"},
		{"unknown destination", inventory.Service{Name: "svc", CatalogEntry: "folder", Destination: "nope", Params: map[string]string{"path": "/x"}}, ""},
		{"ok", inventory.Service{Name: "svc", CatalogEntry: "folder", Destination: "local", Params: map[string]string{"path": "/x"}}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := h.ops.PutService(context.Background(), tt.svc)
			if tt.name == "ok" {
				if err != nil {
					t.Fatalf("PutService: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("PutService: want an error")
			}
			if tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestOpsServiceCRUD(t *testing.T) {
	h := newHarness(t)
	h.addDestination("local")
	svc := inventory.Service{Name: "svc", CatalogEntry: "folder", Destination: "local", Params: map[string]string{"path": "/x"}}
	if err := h.ops.PutService(context.Background(), svc); err != nil {
		t.Fatalf("PutService: %v", err)
	}
	list, err := h.ops.ListServices(context.Background())
	if err != nil || len(list) != 1 {
		t.Fatalf("ListServices: %v, %v", list, err)
	}
	got, err := h.ops.GetService(context.Background(), "svc")
	if err != nil || got.Name != "svc" {
		t.Fatalf("GetService: %v, %v", got, err)
	}
	if err := h.ops.DeleteService(context.Background(), "svc"); err != nil {
		t.Fatalf("DeleteService: %v", err)
	}
	if _, err := h.ops.GetService(context.Background(), "svc"); !errors.Is(err, inventory.ErrNotFound) {
		t.Errorf("GetService after delete = %v, want ErrNotFound", err)
	}
}

func TestOpsDestinationCRUDAndInit(t *testing.T) {
	h := newHarness(t)
	if err := os.WriteFile(filepath.Join(h.credDir, "ballast-restic-password.local"), []byte("pw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := h.ops.PutDestination(context.Background(), inventory.Destination{
		Name: "local", Location: "/stub-repo", KeepDaily: 7,
	}, true /* init */)
	if err != nil {
		t.Fatalf("PutDestination with init: %v", err)
	}

	if err := h.ops.TestDestination(context.Background(), "local"); err != nil {
		t.Fatalf("TestDestination: %v", err)
	}

	list, err := h.ops.ListDestinations(context.Background())
	if err != nil || len(list) != 1 {
		t.Fatalf("ListDestinations: %v, %v", list, err)
	}

	if err := h.ops.DeleteDestination(context.Background(), "local"); err != nil {
		t.Fatalf("DeleteDestination: %v", err)
	}
}

func TestOpsDeleteDestinationInUse(t *testing.T) {
	h := newHarness(t)
	h.addDestination("local")
	if err := h.ops.PutService(context.Background(), inventory.Service{
		Name: "svc", CatalogEntry: "folder", Destination: "local", Params: map[string]string{"path": "/x"},
	}); err != nil {
		t.Fatalf("PutService: %v", err)
	}
	if err := h.ops.DeleteDestination(context.Background(), "local"); !errors.Is(err, inventory.ErrInUse) {
		t.Errorf("DeleteDestination = %v, want ErrInUse", err)
	}
}

func TestOpsBackupAndStartBackup(t *testing.T) {
	h := newHarness(t)
	if err := h.ops.Backup(context.Background(), "svc"); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if len(h.runner.ran) != 1 || h.runner.ran[0] != "svc" {
		t.Errorf("ran = %v, want [svc]", h.runner.ran)
	}

	interrupted, err := h.ops.StartBackup(context.Background(), "other")
	if err != nil {
		t.Fatalf("StartBackup: %v", err)
	}
	if interrupted {
		t.Error("interrupted = true, want false")
	}
	h.ops.Wait()
	if len(h.runner.ran) != 2 {
		t.Errorf("ran = %v, want two runs after Wait", h.runner.ran)
	}
}

// TestOpsRedactErrPreservesErrorType: redacting a secret out of an
// error's text must not lose the original error underneath — callers
// (h.writeErr's status mapping, `errors.Is`) still need to recognize
// it, exactly like scheduler.Coordinator's own redactedError.
func TestOpsRedactErrPreservesErrorType(t *testing.T) {
	h := newHarness(t)
	const secret = "s3cret-token"
	h.ops.Redact = []string{secret}

	original := fmt.Errorf("service svc: %w: saw token %s", scheduler.ErrAlreadyRunning, secret)
	got := h.ops.redactErr(original)

	if !errors.Is(got, scheduler.ErrAlreadyRunning) {
		t.Errorf("redactErr(%v) = %v, lost errors.Is(scheduler.ErrAlreadyRunning)", original, got)
	}
	if strings.Contains(got.Error(), secret) {
		t.Errorf("redactErr(%v).Error() = %q, still leaks the secret", original, got.Error())
	}
}

func TestOpsStartBackupAlreadyRunning(t *testing.T) {
	h := newHarness(t)
	h.runner.block = make(chan struct{})
	defer close(h.runner.block)

	_, err := h.ops.StartBackup(context.Background(), "svc")
	if err != nil {
		t.Fatalf("first StartBackup: %v", err)
	}
	// Give the goroutine a moment to actually take the lock.
	deadline := time.Now().Add(2 * time.Second)
	for {
		h.runner.mu.Lock()
		n := len(h.runner.ran)
		h.runner.mu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	_, err = h.ops.StartBackup(context.Background(), "svc")
	if !errors.Is(err, scheduler.ErrAlreadyRunning) {
		t.Errorf("second StartBackup = %v, want ErrAlreadyRunning", err)
	}
}

func TestOpsSnapshotsAndRestore(t *testing.T) {
	h := newHarness(t)
	h.addDestination("local")
	if err := inventory.PutService(h.ops.DB, inventory.Service{Name: "svc", CatalogEntry: "folder", Destination: "local"}); err != nil {
		t.Fatalf("PutService: %v", err)
	}

	snaps, err := h.ops.Snapshots(context.Background(), "svc")
	if err != nil {
		t.Fatalf("Snapshots: %v", err)
	}
	if len(snaps) != 1 {
		t.Fatalf("snapshots = %v, want 1", snaps)
	}

	to := filepath.Join(t.TempDir(), "restore-out")
	if err := h.ops.Restore(context.Background(), RestoreRequest{Service: "svc", To: to}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
}

func TestOpsRestoreRejectsNonEmptyTarget(t *testing.T) {
	h := newHarness(t)
	h.addDestination("local")
	if err := inventory.PutService(h.ops.DB, inventory.Service{Name: "svc", CatalogEntry: "folder", Destination: "local"}); err != nil {
		t.Fatalf("PutService: %v", err)
	}
	to := t.TempDir()
	if err := os.WriteFile(filepath.Join(to, "existing"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := h.ops.Restore(context.Background(), RestoreRequest{Service: "svc", To: to})
	if err == nil || !errors.Is(err, ErrValidation) {
		t.Fatalf("Restore into non-empty dir = %v, want ErrValidation", err)
	}
}

func TestOpsRestoreRequiresTo(t *testing.T) {
	h := newHarness(t)
	err := h.ops.Restore(context.Background(), RestoreRequest{Service: "svc"})
	if !errors.Is(err, ErrValidation) {
		t.Errorf("Restore without --to = %v, want ErrValidation", err)
	}
}

func TestOpsRestoreCrossServiceSnapshotRejected(t *testing.T) {
	h := newHarness(t)
	h.addDestination("local")
	if err := inventory.PutService(h.ops.DB, inventory.Service{Name: "svc", CatalogEntry: "folder", Destination: "local"}); err != nil {
		t.Fatalf("PutService: %v", err)
	}
	to := filepath.Join(t.TempDir(), "out")
	err := h.ops.Restore(context.Background(), RestoreRequest{Service: "svc", Snapshot: "deadbeef", To: to})
	if err == nil || !errors.Is(err, ErrValidation) {
		t.Fatalf("Restore with an unknown snapshot id = %v, want ErrValidation", err)
	}
}

func TestOpsSchedules(t *testing.T) {
	h := newHarness(t)
	if err := h.ops.SetSchedule(context.Background(), "svc", "not a cron"); !errors.Is(err, ErrValidation) {
		t.Errorf("SetSchedule with a bad cron = %v, want ErrValidation", err)
	}
	if err := h.ops.SetSchedule(context.Background(), "svc", "0 3 * * *"); err != nil {
		t.Fatalf("SetSchedule: %v", err)
	}
	list, err := h.ops.ListSchedules(context.Background())
	if err != nil || len(list) != 1 || list[0].Service != "svc" {
		t.Fatalf("ListSchedules: %v, %v", list, err)
	}
	if err := h.ops.SetScheduleEnabled(context.Background(), "svc", false); err != nil {
		t.Fatalf("SetScheduleEnabled: %v", err)
	}
	if err := h.ops.SetScheduleEnabled(context.Background(), "nope", false); !errors.Is(err, ErrValidation) {
		t.Errorf("SetScheduleEnabled for unknown service = %v, want ErrValidation", err)
	}
}

func TestOpsTriggerTokenGenerateAndTrigger(t *testing.T) {
	h := newHarness(t)
	token, err := h.ops.GenerateTriggerToken(context.Background(), "svc")
	if err != nil {
		t.Fatalf("GenerateTriggerToken: %v", err)
	}
	if token == "" {
		t.Fatal("token is empty")
	}
	if err := h.ops.Trigger(context.Background(), "svc", "wrong"); !errors.Is(err, scheduler.ErrInvalidTriggerToken) {
		t.Errorf("Trigger with wrong token = %v, want ErrInvalidTriggerToken", err)
	}
	if err := h.ops.Trigger(context.Background(), "svc", token); err != nil {
		t.Fatalf("Trigger with valid token: %v", err)
	}
}

// TestOpsStartTrigger: StartTrigger authenticates like Trigger but
// runs the backup in the background, tracked by Wait, exactly like
// StartBackup (HTTP use: the trigger endpoint must not hold the
// request open, nor let the request's cancellation kill the run).
func TestOpsStartTrigger(t *testing.T) {
	h := newHarness(t)
	token, err := h.ops.GenerateTriggerToken(context.Background(), "svc")
	if err != nil {
		t.Fatalf("GenerateTriggerToken: %v", err)
	}

	if _, err := h.ops.StartTrigger(context.Background(), "svc", "wrong"); !errors.Is(err, scheduler.ErrInvalidTriggerToken) {
		t.Errorf("StartTrigger with wrong token = %v, want ErrInvalidTriggerToken", err)
	}
	if len(h.runner.ran) != 0 {
		t.Fatalf("ran = %v, want no runs for a rejected token", h.runner.ran)
	}

	interrupted, err := h.ops.StartTrigger(context.Background(), "svc", token)
	if err != nil {
		t.Fatalf("StartTrigger with valid token: %v", err)
	}
	if interrupted {
		t.Error("interrupted = true, want false (no prior run)")
	}
	h.ops.Wait()
	if len(h.runner.ran) != 1 || h.runner.ran[0] != "svc" {
		t.Errorf("ran = %v, want [svc]", h.runner.ran)
	}
}

func TestOpsListCatalog(t *testing.T) {
	h := newHarness(t)
	list := h.ops.ListCatalog()
	if len(list) != 1 || list[0].Service != "folder" {
		t.Errorf("ListCatalog = %+v, want [folder]", list)
	}
}

func TestOpsStatusLightsAndFreshness(t *testing.T) {
	h := newHarness(t)
	h.addDestination("local")
	if err := inventory.PutService(h.ops.DB, inventory.Service{Name: "svc", CatalogEntry: "folder", Destination: "local"}); err != nil {
		t.Fatalf("PutService: %v", err)
	}

	// No run yet: "none".
	st, err := h.ops.Status(context.Background(), "svc", false)
	if err != nil || len(st) != 1 {
		t.Fatalf("Status: %v, %v", st, err)
	}
	if st[0].Tiers[0].Light != "none" {
		t.Errorf("light = %s, want none", st[0].Tiers[0].Light)
	}
	if st[0].FreshnessAgeSeconds != nil {
		t.Errorf("freshness = %v, want nil (never run)", st[0].FreshnessAgeSeconds)
	}

	// A verified SUCCESS run: "green", freshness set, verify records
	// present when requested.
	m, err := engine.StartRun(h.ops.DB, "svc")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	for _, s := range []engine.State{engine.PREPARING, engine.DUMPING, engine.UPLOADING, engine.VERIFYING} {
		if err := m.Transition(s, "step"); err != nil {
			t.Fatalf("Transition %s: %v", s, err)
		}
	}
	if err := m.RecordVerification(engine.VerificationRecord{Method: "archive-integrity", Passed: true}); err != nil {
		t.Fatalf("RecordVerification: %v", err)
	}
	if err := m.Transition(engine.SUCCESS, "verified"); err != nil {
		t.Fatalf("Transition SUCCESS: %v", err)
	}

	st, err = h.ops.Status(context.Background(), "svc", true)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st[0].Tiers[0].Light != "green" {
		t.Errorf("light = %s, want green", st[0].Tiers[0].Light)
	}
	if st[0].FreshnessAgeSeconds == nil {
		t.Error("freshness = nil, want set after a SUCCESS")
	}
	if len(st[0].Verification) != 1 || st[0].Verification[0].Method != "archive-integrity" {
		t.Errorf("verification = %+v, want the recorded archive-integrity check", st[0].Verification)
	}
}

func TestOpsStatusUnknownServiceErrors(t *testing.T) {
	h := newHarness(t)
	if _, err := h.ops.Status(context.Background(), "nope", false); !errors.Is(err, inventory.ErrNotFound) {
		t.Errorf("Status = %v, want ErrNotFound", err)
	}
}

package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/springledev/ballast/internal/engine"
	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/repohealth"
)

const stopStartEntryYAML = `
apiVersion: ballast.dev/v1
service: stopper
version: 1
backup:
  strategy: stop-start
  stopStart:
    containers: ["app"]
  files:
    - path: %q
verify:
  method: archive-integrity
`

// waitForLine polls path until it holds a line starting with prefix.
func waitForLine(t *testing.T, path, prefix string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(path)
		for _, l := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(l, prefix) {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s never logged %q", path, prefix)
}

// runAndStop runs service and, once waitPrefix appears in logPath,
// stops it the way the Coordinator does when someone asks.
func runAndStop(t *testing.T, p *Pipeline, service, logPath, waitPrefix string) (engine.State, error) {
	t.Helper()
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	type out struct {
		state engine.State
		err   error
	}
	done := make(chan out, 1)
	go func() {
		st, err := p.Run(ctx, service)
		done <- out{st, err}
	}()
	waitForLine(t, logPath, waitPrefix)
	cancel(engine.ErrCancelledByUser)
	select {
	case o := <-done:
		return o.state, o.err
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not return after being stopped")
	}
	return "", nil
}

// lastDetail returns the detail of run's final transition.
func lastDetail(t *testing.T, h *testHarness, runID int64) string {
	t.Helper()
	var d string
	if err := h.db.QueryRow(`SELECT detail FROM run_transitions WHERE run_id = ? ORDER BY id DESC LIMIT 1`, runID).Scan(&d); err != nil {
		t.Fatal(err)
	}
	return d
}

// checker is a repository-health checker that treats every lock the
// restic stub reports as this machine's, with its process gone.
func checker(h *testHarness) *repohealth.Checker {
	return &repohealth.Checker{
		DB:           h.db,
		Hostname:     func() (string, error) { return "this-host", nil },
		ProcessAlive: func(int) bool { return false },
	}
}

// TestPipeline_CancelMidDump_RestartsStoppedContainers stops a
// stop-start backup while `docker stop` is still going: the container
// is started again, the run is CANCELLED "stopped on request", and the
// run's own work folder is removed.
func TestPipeline_CancelMidDump_RestartsStoppedContainers(t *testing.T) {
	h := newHarness(t)
	data := t.TempDir()
	if err := os.WriteFile(filepath.Join(data, "db.sqlite3"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := inventory.PutService(h.db, inventory.Service{Name: "stopper", CatalogEntry: "stopper", Destination: "local"}); err != nil {
		t.Fatal(err)
	}
	// A cancel lets the `docker stop` under way finish (cutting it short
	// could leave the container down), then acts at once.
	p := h.pipeline("DOCKER_STUB_STOP_SLEEP=1")
	p.Catalog = registryWith(t, map[string]string{"stopper.yaml": yamlf(stopStartEntryYAML, data)})

	start := time.Now()
	state, err := runAndStop(t, p, "stopper", h.dockerLog, "stop app")
	if time.Since(start) > 6*time.Second {
		t.Errorf("stopping took %s, want it right after the stop under way", time.Since(start))
	}
	if state != engine.CANCELLED || !errors.Is(err, engine.ErrCancelledByUser) {
		t.Fatalf("Run = %s, %v; want CANCELLED stopped on request", state, err)
	}
	logged, _ := os.ReadFile(h.dockerLog)
	if !strings.Contains(string(logged), "start app") {
		t.Errorf("docker calls = %q, want the stopped container started again", logged)
	}
	runs, _ := engine.RecentRuns(h.db, "stopper", 1)
	if len(runs) != 1 || runs[0].State != engine.CANCELLED {
		t.Fatalf("runs = %+v, want one CANCELLED", runs)
	}
	if d := lastDetail(t, h, runs[0].ID); d != engine.CancelDetailRequested {
		t.Errorf("detail = %q, want %q", d, engine.CancelDetailRequested)
	}
	if _, err := os.Stat(filepath.Join(h.workRoot, "stopper")); !os.IsNotExist(err) {
		t.Errorf("work folder still there after the stop (stat err %v)", err)
	}
}

// TestPipeline_CancelMidUpload_RepoUnlocked stops a run while restic
// is uploading: restic is killed, the lock it left is removed with a
// plain unlock, the destination is recorded CANCELLED and nothing is
// pruned.
func TestPipeline_CancelMidUpload_RepoUnlocked(t *testing.T) {
	h := newHarness(t)
	data := t.TempDir()
	if err := inventory.PutService(h.db, inventory.Service{Name: "folder", CatalogEntry: "folder", Destination: "local"}); err != nil {
		t.Fatal(err)
	}
	p := h.pipeline("RESTIC_STUB_BACKUP_SLEEP=10", "RESTIC_STUB_LOCK_IDS=abc", "RESTIC_STUB_LOCK_HOST=this-host")
	p.Catalog = registryWith(t, map[string]string{"folder.yaml": yamlf(folderEntryYAML, data)})
	p.RepoHealth = checker(h)

	state, err := runAndStop(t, p, "folder", h.resticLog, "backup")
	if state != engine.CANCELLED || !errors.Is(err, engine.ErrCancelledByUser) {
		t.Fatalf("Run = %s, %v; want CANCELLED stopped on request", state, err)
	}
	unlocked, _ := os.ReadFile(h.resticLog + ".unlock")
	if !strings.Contains(string(unlocked), "/stub-repo") {
		t.Errorf("unlock calls = %q, want the repository unlocked", unlocked)
	}
	logged, _ := os.ReadFile(h.resticLog)
	for _, bad := range []string{"--remove-all", "forget", "prune"} {
		if strings.Contains(string(logged), bad) {
			t.Errorf("restic calls = %q, must not contain %q", logged, bad)
		}
	}
	runs, _ := engine.RecentRuns(h.db, "folder", 1)
	targets, err := engine.RunTargets(h.db, runs[0].ID)
	if err != nil || len(targets) != 1 || targets[0].State != engine.CANCELLED {
		t.Fatalf("targets = %+v, %v; want one CANCELLED", targets, err)
	}
	if !strings.Contains(targets[0].Detail, "Stopped on request") {
		t.Errorf("target detail = %q", targets[0].Detail)
	}
}

// TestCancel_OneTargetUnlockFails_OthersRecorded: two destinations
// upload at once and the run is stopped; one cannot be unlocked. Each
// is still recorded on its own, and only the failing one mentions the
// lock (rule 3).
func TestCancel_OneTargetUnlockFails_OthersRecorded(t *testing.T) {
	h := newHarness(t)
	if err := inventory.PutDestination(h.db, inventory.Destination{Name: "second", Location: "/stub-repo2", KeepDaily: 7}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.credDir, "ballast-restic-password.second"), []byte("pw2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	data := t.TempDir()
	svc := inventory.Service{Name: "folder", CatalogEntry: "folder", Destination: "local",
		Targets: []inventory.Target{{Destination: "local"}, {Destination: "second"}}}
	if err := inventory.PutService(h.db, svc); err != nil {
		t.Fatal(err)
	}
	p := h.pipeline("RESTIC_STUB_BACKUP_SLEEP=10", "RESTIC_STUB_LOCK_IDS=abc", "RESTIC_STUB_LOCK_HOST=this-host",
		"RESTIC_STUB_UNLOCK_FAIL_REPO=/stub-repo2")
	p.Catalog = registryWith(t, map[string]string{"folder.yaml": yamlf(folderEntryYAML, data)})
	p.RepoHealth = checker(h)
	p.TargetParallel = 2

	state, _ := runAndStop(t, p, "folder", h.resticLog, "backup")
	if state != engine.CANCELLED {
		t.Fatalf("state = %s, want CANCELLED", state)
	}
	runs, _ := engine.RecentRuns(h.db, "folder", 1)
	targets, err := engine.RunTargets(h.db, runs[0].ID)
	if err != nil || len(targets) != 2 {
		t.Fatalf("targets = %+v, %v; want both recorded", targets, err)
	}
	for _, tr := range targets {
		if tr.State != engine.CANCELLED {
			t.Errorf("%s: state = %s, want CANCELLED", tr.Destination, tr.State)
		}
		mentionsLock := strings.Contains(tr.Detail, "lock")
		if mentionsLock != (tr.Destination == "second") {
			t.Errorf("%s: detail = %q (only the destination that could not be unlocked should mention the lock)", tr.Destination, tr.Detail)
		}
	}
}

// TestCancelNeverSuccess_Pipeline: a run asked to stop just as it
// finishes is refused its green result and ends CANCELLED, with no
// retention run.
func TestCancelNeverSuccess_Pipeline(t *testing.T) {
	h := newHarness(t)
	data := t.TempDir()
	if err := inventory.PutService(h.db, inventory.Service{Name: "folder", CatalogEntry: "folder", Destination: "local"}); err != nil {
		t.Fatal(err)
	}
	p := h.pipeline("RESTIC_STUB_DUMP_SLEEP=1")
	p.Catalog = registryWith(t, map[string]string{"folder.yaml": yamlf(folderEntryYAML, data)})
	// The stop request lands once the read-back has started, before the
	// run's final transition; the context itself is never cancelled,
	// as when the owning process has not polled yet.
	go func() {
		waitForLine(t, h.resticLog, "dump")
		runs, _ := engine.RecentRuns(h.db, "folder", 1)
		if len(runs) == 1 {
			_, _ = engine.RequestCancel(h.db, runs[0].ID)
		}
	}()
	p.Notifier = &stubNotifier{}
	state, err := p.Run(context.Background(), "folder")
	if state != engine.CANCELLED || !errors.Is(err, engine.ErrCancelledByUser) {
		t.Fatalf("Run = %s, %v; want CANCELLED stopped on request", state, err)
	}
	logged, _ := os.ReadFile(h.resticLog)
	if strings.Contains(string(logged), "forget") {
		t.Errorf("a cancelled run applied retention: %q", logged)
	}
	n := p.Notifier.(*stubNotifier)
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.jobFailures+n.verificationFails+n.successHeartbeats != 0 {
		t.Errorf("a cancelled run sent a notification: %+v", n)
	}
}

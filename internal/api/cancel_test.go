package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/springledev/ballast/internal/engine"
	"github.com/springledev/ballast/internal/inventory"
)

// startSlow starts a backup of svc whose fake runner records a real
// run and then waits until it is stopped, ending CANCELLED with the
// recorded reason. It returns the run ID and a channel closed when the
// run has finished.
func startSlow(t *testing.T, h *harness) (int64, <-chan struct{}) {
	t.Helper()
	ids := make(chan int64, 1)
	h.ops.Coord.Runner = runnerFunc(func(ctx context.Context, service string) (engine.State, error) {
		m, err := engine.StartRun(h.ops.DB, service)
		if err != nil {
			return "", err
		}
		if err := m.Transition(engine.PREPARING, "preparing"); err != nil {
			return "", err
		}
		ids <- m.Run().ID
		<-ctx.Done()
		if err := m.Transition(engine.CANCELLED, engine.CancelDetail(ctx, nil)); err != nil {
			return "", err
		}
		return engine.CANCELLED, ctx.Err()
	})
	h.ops.Coord.CancelPoll = time.Hour // in-process cancel must be immediate
	if _, err := h.ops.StartBackup(context.Background(), "svc"); err != nil {
		t.Fatalf("StartBackup: %v", err)
	}
	done := make(chan struct{})
	go func() { h.ops.Wait(); close(done) }()
	select {
	case id := <-ids:
		return id, done
	case <-time.After(5 * time.Second):
		t.Fatal("the backup never started")
	}
	return 0, nil
}

func waitDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the backup did not stop")
	}
}

func TestAPI_CancelRun(t *testing.T) {
	h := newHarness(t)
	withService(t, h)
	finished := seedRun(t, h, "svc", engine.SUCCESS)

	if rec := serveAs(t, h, http.MethodPost, "/api/v1/runs/9999/cancel", nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown run = %d %s, want 404", rec.Code, rec.Body.String())
	}
	if rec := serveAs(t, h, http.MethodPost, "/api/v1/runs/abc/cancel", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("bad id = %d, want 400", rec.Code)
	}
	rec := serveAs(t, h, http.MethodPost, fmt.Sprintf("/api/v1/runs/%d/cancel", finished), nil)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "already finished") {
		t.Errorf("finished run = %d %s, want 409 already finished", rec.Code, rec.Body.String())
	}

	id, done := startSlow(t, h)
	rec = serveAs(t, h, http.MethodPost, fmt.Sprintf("/api/v1/runs/%d/cancel", id), nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("running run = %d %s, want 202", rec.Code, rec.Body.String())
	}
	var res CancelResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.RunID != id || res.Service != "svc" || res.AlreadyRequested || !strings.Contains(res.Message, "last good backup is kept") {
		t.Errorf("result = %+v", res)
	}
	waitDone(t, done)
	m, err := engine.LoadRun(h.ops.DB, id)
	if err != nil {
		t.Fatal(err)
	}
	if m.Run().State != engine.CANCELLED {
		t.Errorf("state = %s, want CANCELLED", m.Run().State)
	}
	ts, _ := engine.RecentTransitions(h.ops.DB, id, 1)
	if len(ts) != 1 || ts[0].Detail != engine.CancelDetailRequested {
		t.Errorf("last transition = %+v, want %q", ts, engine.CancelDetailRequested)
	}
	if rec := serveAs(t, h, http.MethodPost, fmt.Sprintf("/api/v1/runs/%d/cancel", id), nil); rec.Code != http.StatusConflict {
		t.Errorf("cancel after the stop finished = %d, want 409", rec.Code)
	}
}

func TestCancel_NothingRunning(t *testing.T) {
	h := newHarness(t)
	withService(t, h)
	seedRun(t, h, "svc", engine.FAILED)
	_, err := h.ops.CancelService(context.Background(), "svc")
	if !errors.Is(err, ErrNothingRunning) {
		t.Fatalf("CancelService idle = %v, want ErrNothingRunning", err)
	}
	if rec := serveAs(t, h, http.MethodPost, "/api/v1/services/svc/cancel", nil); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "nothing to stop") {
		t.Errorf("HTTP idle = %d %s, want 409", rec.Code, rec.Body.String())
	}
	if _, err := h.ops.CancelService(context.Background(), "nope"); !errors.Is(err, inventory.ErrNotFound) {
		t.Errorf("unknown service = %v, want ErrNotFound", err)
	}
	// A run left unfinished by a process that died has nobody to stop it.
	m, err := engine.StartRun(h.ops.DB, "svc")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.ops.CancelRun(context.Background(), m.Run().ID); !errors.Is(err, ErrNothingRunning) {
		t.Errorf("orphaned run = %v, want ErrNothingRunning", err)
	}
	// Ballast's own backup is not stopped from here.
	if _, err := h.ops.CancelService(context.Background(), SelfBackupService); !errors.Is(err, ErrNotCancellable) {
		t.Errorf("self-backup = %v, want ErrNotCancellable", err)
	}
}

// TestCancel_Double: a second stop request is accepted as a repeat,
// and status reads "cancelling" while the run tidies up.
func TestCancel_Double(t *testing.T) {
	h := newHarness(t)
	withService(t, h)
	// The run ignores the in-process stop so it stays "cancelling".
	release := make(chan struct{})
	ids := make(chan int64, 1)
	h.ops.Coord.Runner = runnerFunc(func(ctx context.Context, service string) (engine.State, error) {
		m, err := engine.StartRun(h.ops.DB, service)
		if err != nil {
			return "", err
		}
		ids <- m.Run().ID
		<-release
		_ = m.Transition(engine.CANCELLED, engine.CancelDetail(ctx, nil))
		return engine.CANCELLED, ctx.Err()
	})
	if _, err := h.ops.StartBackup(context.Background(), "svc"); err != nil {
		t.Fatal(err)
	}
	defer h.ops.Wait()
	defer close(release)
	id := <-ids
	first, err := h.ops.CancelService(context.Background(), "svc")
	if err != nil || first.AlreadyRequested || first.RunID != id {
		t.Fatalf("first cancel = %+v, %v", first, err)
	}
	rec := serveAs(t, h, http.MethodPost, "/api/v1/services/svc/cancel", nil)
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"already_requested":true`) {
		t.Fatalf("second cancel = %d %s, want 202 already_requested", rec.Code, rec.Body.String())
	}
	st, err := h.ops.Status(context.Background(), "svc", false)
	if err != nil {
		t.Fatal(err)
	}
	if tier := st[0].Tiers[0]; tier.Light != "running" || tier.Phase != PhaseCancelling || !tier.Cancelling {
		t.Errorf("tier = %+v, want running and cancelling", tier)
	}
}

// TestCancel_NoNotificationNoFreshnessChange: a stopped backup sends
// no notification, and the freshness clock still counts from the last
// good backup.
func TestCancel_NoNotificationNoFreshnessChange(t *testing.T) {
	h := newHarness(t)
	withService(t, h)
	good := seedRun(t, h, "svc", engine.SUCCESS)
	before, err := h.ops.Status(context.Background(), "svc", false)
	if err != nil {
		t.Fatal(err)
	}
	n := &countingNotifier{}
	m, err := engine.StartRun(h.ops.DB, "svc")
	if err != nil {
		t.Fatal(err)
	}
	m.SetNotifier(context.Background(), n)
	if err := m.Transition(engine.PREPARING, ""); err != nil {
		t.Fatal(err)
	}
	if err := m.Transition(engine.CANCELLED, engine.CancelDetailRequested); err != nil {
		t.Fatal(err)
	}
	if n.calls != 0 {
		t.Errorf("a stopped backup sent %d notification(s)", n.calls)
	}
	after, err := h.ops.Status(context.Background(), "svc", false)
	if err != nil {
		t.Fatal(err)
	}
	b, a := before[0].Tiers[0], after[0].Tiers[0]
	if b.LastVerifiedAt == nil || a.LastVerifiedAt == nil || !a.LastVerifiedAt.Equal(*b.LastVerifiedAt) {
		t.Errorf("last verified backup moved: before %v, after %v (run %d)", b.LastVerifiedAt, a.LastVerifiedAt, good)
	}
	if a.LastState != string(engine.CANCELLED) {
		t.Errorf("last state = %s, want CANCELLED", a.LastState)
	}
}

// countingNotifier counts every notification it is asked to send.
type countingNotifier struct{ calls int }

func (n *countingNotifier) NotifyJobFailure(context.Context, string, int64, string) error {
	n.calls++
	return nil
}
func (n *countingNotifier) NotifyVerificationFailure(context.Context, string, int64, string) error {
	n.calls++
	return nil
}
func (n *countingNotifier) NotifySuccessHeartbeat(context.Context, string, int64) error {
	n.calls++
	return nil
}

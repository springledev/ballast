package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/springledev/ballast/internal/engine"
)

// TestStartBackupSurvivesRequestCompletion is a regression test for a
// real bug: net/http cancels an incoming request's Context() the
// instant its handler returns (documented behavior), which for a
// 202-Accepted async endpoint happens almost immediately — long
// before a real backup finishes. httptest.NewRecorder +
// httptest.NewRequest (used by the golden-file tests) does NOT
// reproduce this: that request's context is context.Background() and
// is never auto-canceled, so the bug was invisible there. This test
// uses a real httptest.NewServer (a real net/http.Server) so the
// request's context really is canceled on handler return, and
// asserts the background run's context is NOT canceled as a result —
// api.Ops.Ctx (set by wiring to the server's own long-lived context)
// is what makes that true in production.
func TestStartBackupSurvivesRequestCompletion(t *testing.T) {
	h := newHarness(t)
	ctxErrAtCompletion := make(chan error, 1)
	proceed := make(chan struct{})
	started := make(chan struct{})
	h.ops.Coord.Runner = runnerFunc(func(ctx context.Context, service string) (engine.State, error) {
		close(started)
		<-proceed
		// Recorded once the handler has already returned (see below),
		// which is exactly when a request-scoped context would have
		// been canceled if StartBackup used it for the background run.
		ctxErrAtCompletion <- ctx.Err()
		return engine.SUCCESS, nil
	})
	// Simulate wiring: give Ops a long-lived context distinct from any
	// single request's, as `ballast serve` does.
	h.ops.Ctx = context.Background()

	srv := httptest.NewServer(NewHandler(h.ops, testAPIToken))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/services/svc/backup", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testAPIToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST backup: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}

	// The HTTP handler has now returned and the response is fully
	// received: a request-scoped context would be canceled by now.
	<-started
	close(proceed)

	select {
	case gotErr := <-ctxErrAtCompletion:
		if gotErr != nil {
			t.Errorf("background run's ctx.Err() = %v, want nil (the run must outlive the request that started it)", gotErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("background run never completed")
	}
}

// TestTriggerSurvivesRequestCompletion is TestStartBackupSurvivesRequestCompletion's
// counterpart for POST .../trigger: it too must start its run in the
// background on Ops.Ctx rather than the request's own context, and
// return 202 immediately, so a client disconnect or timeout (a phone
// shortcut, curl -m) cannot cancel a run it kicked off.
func TestTriggerSurvivesRequestCompletion(t *testing.T) {
	h := newHarness(t)
	token, err := h.ops.GenerateTriggerToken(context.Background(), "svc")
	if err != nil {
		t.Fatalf("GenerateTriggerToken: %v", err)
	}

	ctxErrAtCompletion := make(chan error, 1)
	proceed := make(chan struct{})
	started := make(chan struct{})
	h.ops.Coord.Runner = runnerFunc(func(ctx context.Context, service string) (engine.State, error) {
		close(started)
		<-proceed
		ctxErrAtCompletion <- ctx.Err()
		return engine.SUCCESS, nil
	})
	h.ops.Ctx = context.Background()

	srv := httptest.NewServer(NewHandler(h.ops, testAPIToken))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/services/svc/trigger", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Ballast-Trigger-Token", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST trigger: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}

	<-started
	close(proceed)

	select {
	case gotErr := <-ctxErrAtCompletion:
		if gotErr != nil {
			t.Errorf("background run's ctx.Err() = %v, want nil (the run must outlive the request that started it)", gotErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("background run never completed")
	}
}

// runnerFunc adapts a plain function to scheduler.Runner without
// importing internal/scheduler's own RunnerFunc (keeps this test
// self-contained).
type runnerFunc func(ctx context.Context, service string) (engine.State, error)

func (f runnerFunc) Run(ctx context.Context, service string) (engine.State, error) {
	return f(ctx, service)
}

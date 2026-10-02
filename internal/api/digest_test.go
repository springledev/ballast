package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/springledev/ballast/internal/engine"
	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/notify"
)

func TestWeeklyDigestSummarisesServices(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.addDestination("local")
	for _, n := range []string{"photos", "vault"} {
		if err := h.ops.PutService(ctx, inventory.Service{Name: n, CatalogEntry: "folder", Destination: "local", Params: map[string]string{"path": "/" + n}}); err != nil {
			t.Fatal(err)
		}
	}
	h.ops.WorkRoot = t.TempDir()
	ev, err := h.ops.WeeklyDigest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Type != notify.EventWeeklyDigest {
		t.Errorf("type = %q", ev.Type)
	}
	if !strings.Contains(ev.Message, "2 never backed up") {
		t.Errorf("message = %q", ev.Message)
	}
	for _, want := range []string{"photos: none, no verified backup yet", "vault:", "Disk space:"} {
		if !strings.Contains(ev.Detail, want) {
			t.Errorf("detail %q missing %q", ev.Detail, want)
		}
	}
}

func TestSendDigestOnlyToOptedInChannels(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	var mu sync.Mutex
	got := map[string]string{} // path -> title
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body) //nolint:errcheck
		mu.Lock()
		got[r.URL.Path] = r.Header.Get("Title")
		mu.Unlock()
	}))
	defer srv.Close()

	if _, err := h.ops.SendDigest(ctx); !errors.Is(err, ErrNoDigestChannels) {
		t.Fatalf("no channels: err = %v, want ErrNoDigestChannels", err)
	}

	on, off := true, false
	// A channel limited to failure events still gets the digest it asked for.
	if _, err := h.ops.PutChannel(ctx, ChannelInput{Name: "weekly", URL: srv.URL + "/weekly", ApplyToAll: true, Events: []string{"job_failure"}, Digest: &on}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ops.PutChannel(ctx, ChannelInput{Name: "quiet", URL: srv.URL + "/quiet", ApplyToAll: true, Digest: &off}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ops.PutChannel(ctx, ChannelInput{Name: "broken", URL: "http://127.0.0.1:1/x", ApplyToAll: true, Digest: &on}); err != nil {
		t.Fatal(err)
	}

	res, err := h.ops.SendDigest(ctx)
	if err == nil || !strings.Contains(err.Error(), "channel broken") {
		t.Errorf("err = %v, want the broken channel named", err)
	}
	if strings.Join(res.Sent, ",") != "weekly" {
		t.Errorf("sent = %v, want [weekly] (a broken channel never blocks another)", res.Sent)
	}
	if got["/weekly"] != "Ballast weekly digest" {
		t.Errorf("titles = %v", got)
	}
	if _, ok := got["/quiet"]; ok {
		t.Error("digest sent to a channel that did not opt in")
	}

	// Editing without the digest field keeps the opt-in.
	if v, err := h.ops.PutChannel(ctx, ChannelInput{Name: "weekly", ApplyToAll: true}); err != nil || !v.Digest {
		t.Errorf("edit lost digest opt-in: %+v, %v", v, err)
	}
}

// TestWeeklyDigestReportsVerifiedBackup drives a run through the real
// state machine to VERIFYING -> SUCCESS and checks the digest counts the
// service green with a last verified restore age; an unverified run
// stays amber and is never reported as verified.
func TestWeeklyDigestReportsVerifiedBackup(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.addDestination("local")
	for _, n := range []string{"photos", "vault"} {
		if err := h.ops.PutService(ctx, inventory.Service{Name: n, CatalogEntry: "folder", Destination: "local", Params: map[string]string{"path": "/" + n}}); err != nil {
			t.Fatal(err)
		}
	}
	finish := func(service string, final engine.State) {
		m, err := engine.StartRun(h.ops.DB, service)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range []engine.State{engine.PREPARING, engine.DUMPING, engine.UPLOADING, engine.VERIFYING, final} {
			if err := m.Transition(s, "test"); err != nil {
				t.Fatalf("%s -> %s: %v", service, s, err)
			}
		}
	}
	finish("photos", engine.SUCCESS)
	finish("vault", engine.SUCCESS_UNVERIFIED)

	ev, err := h.ops.WeeklyDigest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Message != "Weekly digest: 1 green, 1 amber, 0 red" {
		t.Errorf("message = %q", ev.Message)
	}
	for _, want := range []string{"photos: green, last verified restore", "vault: amber, no verified backup yet"} {
		if !strings.Contains(ev.Detail, want) {
			t.Errorf("detail %q missing %q", ev.Detail, want)
		}
	}
}

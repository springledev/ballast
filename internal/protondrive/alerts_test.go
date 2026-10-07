package protondrive

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/springledev/ballast/internal/notify"
	"github.com/springledev/ballast/internal/store"
)

type recordingChannel struct{ events []notify.Event }

func (c *recordingChannel) Type() string { return "fake" }

func (c *recordingChannel) Send(_ context.Context, e notify.Event) error {
	c.events = append(c.events, e)
	return nil
}

func TestNotifyAlerterSendsOnceThenRecovers(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	n := notify.NewNotifier()
	ch := &recordingChannel{}
	n.RegisterChannel("vault", ch)
	a := &NotifyAlerter{DB: st.DB(), Notifier: n}
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if err := a.Alert(ctx, "vault", "proton", "sign in again"); err != nil {
			t.Fatal(err)
		}
	}
	if len(ch.events) != 1 || ch.events[0].Service != "vault" || !strings.Contains(ch.events[0].Message, "Proton Drive") || !strings.Contains(ch.events[0].Message, "sign in again") {
		t.Fatalf("events = %+v, want one alert naming Proton Drive", ch.events)
	}
	if ch.events[0].Severity == "error" {
		t.Error("a copy problem is a warning, never an error")
	}
	if err := a.Recovered(ctx, "vault", "proton"); err != nil || len(ch.events) != 2 || ch.events[1].Type != notify.EventRecovery {
		t.Fatalf("recovery: %v %+v", err, ch.events)
	}
	if err := a.Recovered(ctx, "vault", "proton"); err != nil || len(ch.events) != 2 {
		t.Errorf("recovery was sent twice")
	}
}

func TestNotifyAlerterWithoutChannelsIsQuiet(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	a := &NotifyAlerter{DB: st.DB(), Notifier: notify.NewNotifier()}
	if err := a.Alert(context.Background(), "vault", "proton", "x"); err != nil {
		t.Errorf("err = %v, want nil when no channel is set up", err)
	}
}

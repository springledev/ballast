package repohealth

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/springledev/ballast/internal/backend"
	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/notify"
	"github.com/springledev/ballast/internal/restic"
	"github.com/springledev/ballast/internal/store"
	"github.com/springledev/keel/runner"
)

// stub restic: records argv, serves lock info from env.
const stub = `#!/bin/sh
printf '%s\n' "$*" >> "$STUB_LOG"
case "$1" in
  list) printf '%s\n' $STUB_LOCK_IDS ;;
  cat) printf '{"time":"%s","exclusive":true,"hostname":"%s","username":"root","pid":4242}\n' "$STUB_LOCK_TIME" "$STUB_LOCK_HOST" ;;
  check) [ -n "$STUB_CHECK_FAIL" ] && { echo "Fatal: pack abc damaged" >&2; exit 1; }; exit 0 ;;
  unlock) exit 0 ;;
esac
exit 0
`

type fixture struct {
	db  *sql.DB
	log string
	env []string
	c   *Checker
	now time.Time
}

func newFixture(t *testing.T, env ...string) *fixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	dir := t.TempDir()
	bin := filepath.Join(dir, "restic")
	if err := os.WriteFile(bin, []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	pw := filepath.Join(dir, "pw")
	os.WriteFile(pw, []byte("x\n"), 0o600)
	f := &fixture{db: st.DB(), log: filepath.Join(dir, "log"), now: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)}
	f.env = append(os.Environ(), "STUB_LOG="+f.log)
	f.env = append(f.env, env...)
	d := inventory.Destination{Name: "nas", Type: backend.Local, Settings: backend.Settings{Path: "/srv/b"}, PasswordCredential: "pw", KeepDaily: 7}
	if err := inventory.PutDestination(f.db, d); err != nil {
		t.Fatal(err)
	}
	f.c = &Checker{
		DB: f.db,
		Repo: func(inventory.Destination) (*restic.Repository, error) {
			return restic.Open("/srv/b", restic.Options{Runner: &runner.Runner{BaseEnv: f.env}, PasswordFile: pw, ResticBin: bin})
		},
		Hostname:     func() (string, error) { return "nas-host", nil },
		ProcessAlive: func(int) bool { return false },
		Now:          func() time.Time { return f.now },
	}
	return f
}

func (f *fixture) calls(t *testing.T) string {
	b, _ := os.ReadFile(f.log)
	return string(b)
}

func lockEnv(host string, age time.Duration, now time.Time) []string {
	return []string{"STUB_LOCK_IDS=abc", "STUB_LOCK_HOST=" + host, "STUB_LOCK_TIME=" + now.Add(-age).Format(time.RFC3339)}
}

func TestCheckGreenAndRed(t *testing.T) {
	f := newFixture(t)
	st, err := f.c.Check(context.Background(), "nas")
	if err != nil || st.State != StateOK || st.Light != "green" || st.LastOK.IsZero() {
		t.Fatalf("green check = %+v, %v", st, err)
	}
	if !strings.Contains(f.calls(t), "check --read-data-subset=5%") {
		t.Errorf("default subset not used: %q", f.calls(t))
	}
	f.env = append(f.env, "STUB_CHECK_FAIL=1")
	st, err = f.c.Check(context.Background(), "nas")
	if err != nil || st.State != StateFailed || st.Light != "red" || !strings.Contains(st.Detail, "damaged") {
		t.Fatalf("red check = %+v, %v", st, err)
	}
	if st.LastOK.IsZero() {
		t.Error("a failed check must keep the earlier last-OK time")
	}
}

func TestHandleLocks(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name       string
		host       string
		age        time.Duration
		alive      bool
		wantUnlock bool
		wantNote   string
	}{
		{"stale own lock removed", "nas-host", 7 * time.Hour, false, true, ""},
		{"recent own lock kept quietly", "nas-host", time.Hour, false, false, ""},
		{"other machine amber", "other-box", 9 * time.Hour, false, false, "another machine"},
		{"live process amber", "nas-host", 9 * time.Hour, true, false, "still running"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, lockEnv(tc.host, tc.age, now)...)
			f.c.ProcessAlive = func(int) bool { return tc.alive }
			repo, _ := f.c.Repo(inventory.Destination{})
			out := f.c.HandleLocks(context.Background(), repo, "nas")
			if out.Removed != tc.wantUnlock || strings.Contains(f.calls(t), "unlock") != tc.wantUnlock {
				t.Errorf("removed=%v calls=%q", out.Removed, f.calls(t))
			}
			if (tc.wantNote == "") != (out.Note == "") || !strings.Contains(out.Note, tc.wantNote) {
				t.Errorf("note = %q, want containing %q", out.Note, tc.wantNote)
			}
			if strings.Contains(f.calls(t), "--remove-all") {
				t.Error("--remove-all must never be used")
			}
		})
	}
}

// TestClearOwnLocks: after a stopped backup a fresh lock from this
// machine whose process is gone is removed at once (no minimum age),
// while another machine's lock or a live process's lock is left alone.
func TestClearOwnLocks(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name       string
		host       string
		alive      bool
		wantUnlock bool
		wantNote   string
	}{
		{"fresh own lock of a dead process removed", "nas-host", false, true, ""},
		{"other machine left alone", "other-box", false, false, "another machine"},
		{"live process left alone", "nas-host", true, false, "still running"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, lockEnv(tc.host, time.Second, now)...)
			f.c.ProcessAlive = func(int) bool { return tc.alive }
			repo, _ := f.c.Repo(inventory.Destination{})
			out, err := f.c.ClearOwnLocks(context.Background(), repo, "nas")
			if err != nil {
				t.Fatalf("ClearOwnLocks: %v", err)
			}
			if out.Removed != tc.wantUnlock || strings.Contains(f.calls(t), "unlock") != tc.wantUnlock {
				t.Errorf("removed=%v calls=%q", out.Removed, f.calls(t))
			}
			if !strings.Contains(out.Note, tc.wantNote) {
				t.Errorf("note = %q, want containing %q", out.Note, tc.wantNote)
			}
			if strings.Contains(f.calls(t), "--remove-all") {
				t.Error("--remove-all must never be used")
			}
		})
	}
}

func TestCheckAmberWhenForeignLockBlocks(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	f := newFixture(t, append(lockEnv("other-box", 10*time.Hour, now), "STUB_CHECK_FAIL=1")...)
	st, err := f.c.Check(context.Background(), "nas")
	if err != nil || st.State != StateAmber || !strings.Contains(st.Detail, "another machine") {
		t.Fatalf("got %+v, %v", st, err)
	}
}

func TestConfigureAndSweepDue(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.c.Configure("nas", true, 0, "5%"); err == nil {
		t.Error("zero interval must be refused")
	}
	if err := f.c.Configure("nas", true, 24, "bogus"); err == nil {
		t.Error("bad subset must be refused")
	}
	if err := f.c.Configure("ghost", true, 24, "5%"); err == nil {
		t.Error("unknown destination must be refused")
	}
	if err := f.c.Configure("nas", true, 24, ""); err != nil {
		t.Fatal(err)
	}
	if err := f.c.SweepDue(ctx); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(f.calls(t), "check"); got != 1 || strings.Contains(f.calls(t), "read-data-subset") {
		t.Fatalf("first sweep: %q", f.calls(t))
	}
	f.now = f.now.Add(23 * time.Hour)
	f.c.SweepDue(ctx)
	if got := strings.Count(f.calls(t), "check"); got != 1 {
		t.Errorf("not due yet, but ran again: %q", f.calls(t))
	}
	f.now = f.now.Add(2 * time.Hour)
	f.c.SweepDue(ctx)
	if got := strings.Count(f.calls(t), "check"); got != 2 {
		t.Errorf("due again, should run: %q", f.calls(t))
	}
	if err := f.c.Configure("nas", false, 24, ""); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(100 * time.Hour)
	f.c.SweepDue(ctx)
	if got := strings.Count(f.calls(t), "check"); got != 2 {
		t.Errorf("disabled destination was checked: %q", f.calls(t))
	}
}

type recAlerter struct{ alerts, recovered int }

func (r *recAlerter) Alert(context.Context, string, string, string) error { r.alerts++; return nil }
func (r *recAlerter) Recovered(context.Context, string) error             { r.recovered++; return nil }

func TestCheckAlerts(t *testing.T) {
	f := newFixture(t, "STUB_CHECK_FAIL=1")
	rec := &recAlerter{}
	f.c.Alerts = rec
	f.c.Check(context.Background(), "nas")
	f.env = f.env[:len(f.env)-1]
	f.c.Check(context.Background(), "nas")
	if rec.alerts != 1 || rec.recovered != 1 {
		t.Errorf("alerts=%d recovered=%d", rec.alerts, rec.recovered)
	}
}

type fakeChannel struct{ events []notify.Event }

func (c *fakeChannel) Type() string { return "fake" }
func (c *fakeChannel) Send(_ context.Context, e notify.Event) error {
	c.events = append(c.events, e)
	return nil
}

func TestNotifyAlerter(t *testing.T) {
	f := newFixture(t)
	if _, err := f.db.Exec(`INSERT INTO services (name, catalog_entry, destination, created_at, updated_at) VALUES ('vault','x','nas','t','t')`); err != nil {
		t.Fatal(err)
	}
	n := notify.NewNotifier()
	ch := &fakeChannel{}
	n.RegisterChannel("vault", ch)
	a := &NotifyAlerter{DB: f.db, Notifier: n}
	ctx := context.Background()
	if err := a.Alert(ctx, "nas", StateFailed, "bad pack"); err != nil {
		t.Fatal(err)
	}
	if err := a.Alert(ctx, "nas", StateFailed, "bad pack"); err != nil {
		t.Fatal(err)
	}
	if len(ch.events) != 1 || ch.events[0].Service != "vault" || !strings.Contains(ch.events[0].Message, "bad pack") {
		t.Fatalf("events = %+v", ch.events)
	}
	if err := a.Recovered(ctx, "nas"); err != nil || len(ch.events) != 2 || ch.events[1].Type != notify.EventRecovery {
		t.Fatalf("recovery: %v %+v", err, ch.events)
	}
	if err := a.Recovered(ctx, "nas"); err != nil || len(ch.events) != 2 {
		t.Errorf("recovery sent twice")
	}
	// No channels for any dependent service surfaces ErrNoChannels.
	b := &NotifyAlerter{DB: f.db, Notifier: notify.NewNotifier()}
	f.db.Exec(`DELETE FROM alert_state`)
	if err := b.Alert(ctx, "nas", StateFailed, "x"); !errors.Is(err, notify.ErrNoChannels) {
		t.Errorf("err = %v, want ErrNoChannels", err)
	}
}

func TestCheckBusy(t *testing.T) {
	f := newFixture(t)
	end, err := f.c.begin("nas")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.Check(context.Background(), "nas"); !errors.Is(err, ErrBusy) {
		t.Errorf("err = %v, want ErrBusy", err)
	}
	if err := f.c.SweepDue(context.Background()); err != nil {
		t.Errorf("a busy destination must not fail the sweep: %v", err)
	}
	end()
	if _, err := f.c.Check(context.Background(), "nas"); err != nil {
		t.Errorf("after finishing: %v", err)
	}
}

func TestListSkipsProtonDriveCopies(t *testing.T) {
	f := newFixture(t)
	if err := inventory.PutDestination(f.db, inventory.Destination{Name: "proton", Type: backend.ProtonDrive}); err != nil {
		t.Fatal(err)
	}
	list, err := f.c.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range list {
		if st.Destination == "proton" {
			t.Errorf("a Proton Drive copy has no repository to check but was listed: %+v", st)
		}
	}
	if len(list) != 1 {
		t.Errorf("list = %+v, want only the restic destination", list)
	}
}

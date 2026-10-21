package notify

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/springledev/ballast/internal/scheduler"
	"github.com/springledev/ballast/internal/store"
)

func TestRoutedChannelFilters(t *testing.T) {
	tests := []struct {
		name   string
		events []string
		tiers  []string
		ev     Event
		want   int
	}{
		{"no filters sends all", nil, nil, Event{Type: EventJobFailure}, 1},
		{"event allowed", []string{"job_failure"}, nil, Event{Type: EventJobFailure}, 1},
		{"event filtered", []string{"job_failure"}, nil, Event{Type: EventSuccessHeartbeat}, 0},
		{"tier allowed", nil, []string{"restic"}, Event{Type: EventJobFailure}, 1},
		{"tier filtered", nil, []string{"restic"}, Event{Type: EventJobFailure, Tier: "rclone"}, 0},
		{"test always passes", []string{"recovery"}, []string{"rclone"}, Event{Type: EventTest}, 1},
		{"digest skips filters", []string{"job_failure"}, []string{"rclone"}, Event{Type: EventWeeklyDigest}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeChannel{}
			newRouted(f, tt.events, tt.tiers).Send(context.Background(), tt.ev) //nolint:errcheck
			if len(f.events) != tt.want {
				t.Fatalf("delivered %d, want %d", len(f.events), tt.want)
			}
		})
	}
}

func TestValidateRouting(t *testing.T) {
	if _, _, err := ValidateRouting([]string{"nonsense"}, nil); err == nil {
		t.Error("unknown event accepted")
	}
	if _, _, err := ValidateRouting(nil, []string{"a b"}); err == nil {
		t.Error("bad tier accepted")
	}
	ev, ti, err := ValidateRouting([]string{"recovery", "recovery", "job_failure"}, []string{"restic"})
	if err != nil || len(ev) != 2 || len(ti) != 1 {
		t.Fatalf("got %v %v %v", ev, ti, err)
	}
}

func TestAlertGateCooldown(t *testing.T) {
	db := testDB(t)
	defer db.Close()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	g := &AlertGate{DB: db.DB(), Now: func() time.Time { return now }}
	if ok, _ := g.ShouldAlert("svc"); !ok {
		t.Fatal("first failure must alert")
	}
	if err := g.RecordAlert("svc"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(23 * time.Hour)
	if ok, _ := g.ShouldAlert("svc"); ok {
		t.Error("alert repeated inside cool-down")
	}
	now = now.Add(2 * time.Hour)
	if ok, _ := g.ShouldAlert("svc"); !ok {
		t.Error("no reminder after cool-down")
	}
	if a, _ := g.IsAlerting("svc"); !a {
		t.Error("should be alerting")
	}
	if err := g.Clear("svc"); err != nil {
		t.Fatal(err)
	}
	if a, _ := g.IsAlerting("svc"); a {
		t.Error("still alerting after Clear")
	}
}

// A daily job failing at 05:00:15 and then 05:00:10 the next day must
// remind, and the decision log must say so.
func TestEngineAdapter_DailyFailureRemindsEveryDay(t *testing.T) {
	db := testDB(t)
	defer db.Close()
	if err := scheduler.SetSchedule(db.DB(), "svc", "0 5 * * *"); err != nil {
		t.Fatal(err)
	}
	notifier := NewNotifier()
	ch := &fakeChannel{}
	notifier.RegisterChannel("svc", ch)
	now := time.Date(2026, 10, 8, 5, 0, 15, 0, time.UTC)
	a := &EngineAdapter{DB: db.DB(), Notifier: notifier, Now: func() time.Time { return now }}
	ctx := context.Background()
	for i, at := range []time.Time{
		now, now.Add(24*time.Hour - 5*time.Second), now.Add(48*time.Hour - 12*time.Second),
	} {
		now = at
		if err := a.NotifyJobFailure(ctx, "svc", int64(i), "boom"); err != nil {
			t.Fatal(err)
		}
	}
	if len(ch.events) != 3 {
		t.Fatalf("sent %d alerts for 3 nightly failures, want 3", len(ch.events))
	}
}

// failAt starts a run at started (a real runs row, so the gate can see
// when it began) and reports its failure at the adapter's current time.
func failAt(t *testing.T, a *EngineAdapter, db *store.Store, started time.Time) {
	t.Helper()
	d := db.DB()
	if _, err := d.Exec(`INSERT OR IGNORE INTO jobs (service, created_at) VALUES ('svc', ?)`, started.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	res, err := d.Exec(`INSERT INTO runs (job_id, state, started_at) SELECT id, 'FAILED', ? FROM jobs WHERE service = 'svc'`, started.Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	if err := a.NotifyJobFailure(context.Background(), "svc", id, "boom"); err != nil {
		t.Fatal(err)
	}
}

func TestEngineAdapter_ReminderPerScheduledRun(t *testing.T) {
	day := func(d, h, m, s int) time.Time { return time.Date(2026, 10, d, h, m, s, 0, time.UTC) }
	type fail struct{ started, finished time.Time }
	tests := []struct {
		name  string
		cron  string // empty means unscheduled
		fails []fail
		sent  []bool // whether each failure should alert
	}{
		{"daily 05:00:15 then 05:00:10", "0 5 * * *",
			[]fail{{day(8, 5, 0, 15), day(8, 5, 0, 15)}, {day(9, 5, 0, 10), day(9, 5, 0, 10)}},
			[]bool{true, true}},
		{"daily 02:00, first takes 3h, second fails after 5 min", "0 2 * * *",
			[]fail{{day(8, 2, 0, 0), day(8, 5, 0, 0)}, {day(9, 2, 0, 0), day(9, 2, 5, 0)}},
			[]bool{true, true}},
		{"mon and thu: thu then mon", "0 5 * * 1,4",
			[]fail{{day(8, 5, 0, 0), day(8, 5, 10, 0)}, {day(12, 5, 0, 0), day(12, 5, 10, 0)}},
			[]bool{true, true}},
		{"mon and thu: manual retry on the same day is held", "0 5 * * 1,4",
			[]fail{{day(8, 5, 0, 0), day(8, 5, 10, 0)}, {day(8, 14, 0, 0), day(8, 14, 10, 0)}, {day(9, 14, 0, 0), day(9, 14, 10, 0)}, {day(12, 5, 0, 0), day(12, 5, 10, 0)}},
			[]bool{true, false, false, true}},
		{"0,12,13: each firing reminds, retries between do not", "0 0,12,13 * * *",
			[]fail{{day(8, 0, 0, 0), day(8, 0, 5, 0)}, {day(8, 0, 30, 0), day(8, 0, 35, 0)}, {day(8, 12, 0, 0), day(8, 12, 5, 0)}, {day(8, 13, 0, 0), day(8, 13, 5, 0)}},
			[]bool{true, false, true, true}},
		{"unscheduled keeps 24h", "",
			[]fail{{day(8, 5, 0, 0), day(8, 5, 0, 0)}, {day(9, 4, 0, 0), day(9, 4, 0, 0)}, {day(9, 5, 0, 0), day(9, 5, 0, 0)}},
			[]bool{true, false, true}},
		{"unscheduled manual retries do not spam", "",
			[]fail{{day(8, 5, 0, 0), day(8, 5, 0, 0)}, {day(8, 5, 10, 0), day(8, 5, 10, 0)}, {day(8, 6, 0, 0), day(8, 6, 0, 0)}},
			[]bool{true, false, false}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := testDB(t)
			defer db.Close()
			if tt.cron != "" {
				if err := scheduler.SetSchedule(db.DB(), "svc", tt.cron); err != nil {
					t.Fatal(err)
				}
			}
			notifier := NewNotifier()
			ch := &fakeChannel{}
			notifier.RegisterChannel("svc", ch)
			var now time.Time
			a := &EngineAdapter{DB: db.DB(), Notifier: notifier, Location: time.UTC, Now: func() time.Time { return now }}
			for i, f := range tt.fails {
				now = f.finished
				before := len(ch.events)
				failAt(t, a, db, f.started)
				if got := len(ch.events) > before; got != tt.sent[i] {
					ds, _ := ListAlertDecisions(db.DB(), "svc", 10)
					t.Errorf("failure %d (started %v): alerted = %v, want %v; decisions %+v", i, f.started, got, tt.sent[i], ds)
				}
			}
		})
	}
}

func TestEngineAdapter_HeldReasonForSameScheduledRun(t *testing.T) {
	db := testDB(t)
	defer db.Close()
	if err := scheduler.SetSchedule(db.DB(), "svc", "0 5 * * *"); err != nil {
		t.Fatal(err)
	}
	notifier := NewNotifier()
	notifier.RegisterChannel("svc", &fakeChannel{})
	var now time.Time
	a := &EngineAdapter{DB: db.DB(), Notifier: notifier, Location: time.UTC, Now: func() time.Time { return now }}
	start := time.Date(2026, 10, 8, 5, 0, 0, 0, time.UTC)
	now = start.Add(time.Minute)
	failAt(t, a, db, start)
	now = start.Add(time.Hour)
	failAt(t, a, db, start.Add(50*time.Minute))
	ds, _ := ListAlertDecisions(db.DB(), "svc", 10)
	if len(ds) != 2 || ds[0].Outcome != OutcomeHeld || !strings.Contains(ds[0].Reason, "already alerted for this scheduled run") {
		t.Fatalf("decisions = %+v", ds)
	}
}

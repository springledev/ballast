package notify

import (
	"context"
	"testing"
	"time"

	"github.com/springledev/ballast/internal/scheduler"
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

func TestAlertGateCooldownFollowsSchedule(t *testing.T) {
	tests := []struct {
		name     string
		cron     string // empty means no schedule
		disabled bool
		cooldown time.Duration
		elapsed  time.Duration
		want     bool
	}{
		{"daily job, next failure 5s early reminds", "0 5 * * *", false, 0, 24*time.Hour - 5*time.Second, true},
		{"daily job, 20h later still held", "0 5 * * *", false, 0, 20 * time.Hour, false},
		{"unscheduled keeps 24h: 23h held", "", false, 0, 23 * time.Hour, false},
		{"unscheduled keeps 24h: 24h reminds", "", false, 0, 24 * time.Hour, true},
		{"disabled schedule counts as unscheduled", "0 5 * * *", true, 0, 23 * time.Hour, false},
		{"hourly job reminds hourly", "0 * * * *", false, 0, 59 * time.Minute, true},
		{"weekly job is capped by the 24h default", "0 5 * * 1", false, 0, 23 * time.Hour, false},
		{"explicit shorter cool-down is kept", "0 5 * * *", false, time.Hour, 2 * time.Hour, true},
		{"explicit longer cool-down is capped", "0 5 * * *", false, 72 * time.Hour, 24*time.Hour - 5*time.Second, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := testDB(t)
			defer db.Close()
			if tt.cron != "" {
				if err := scheduler.SetSchedule(db.DB(), "svc", tt.cron); err != nil {
					t.Fatal(err)
				}
				if tt.disabled {
					if err := scheduler.SetEnabled(db.DB(), "svc", false); err != nil {
						t.Fatal(err)
					}
				}
			}
			now := time.Date(2026, 10, 9, 5, 0, 15, 0, time.UTC)
			g := &AlertGate{DB: db.DB(), Cooldown: tt.cooldown, Now: func() time.Time { return now }}
			if err := g.RecordAlert("svc"); err != nil {
				t.Fatal(err)
			}
			now = now.Add(tt.elapsed)
			ok, why, err := g.Decide("svc")
			if err != nil || ok != tt.want {
				t.Fatalf("Decide = %v, %q, %v; want %v", ok, why, err, tt.want)
			}
		})
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

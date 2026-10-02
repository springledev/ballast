package notify

import (
	"context"
	"testing"
	"time"
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

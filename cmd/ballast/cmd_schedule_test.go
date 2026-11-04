package main

import (
	"strings"
	"testing"
	"time"

	"github.com/springledev/ballast/internal/api"
	"github.com/springledev/ballast/internal/scheduler"
)

func TestScheduleSetEnableDisableList(t *testing.T) {
	cliEnv(t, &spyRunner{})

	out, err := execCLI(t, "schedule", "set", "immich", "0 3 * * *")
	if err != nil {
		t.Fatalf("schedule set: %v (output %q)", err, out)
	}
	if !strings.Contains(out, "immich") || !strings.Contains(out, "0 3 * * *") {
		t.Errorf("output = %q, want it to name the service and cron expression", out)
	}

	out, err = execCLI(t, "schedule", "list")
	if err != nil {
		t.Fatalf("schedule list: %v", err)
	}
	if !strings.Contains(out, "immich") || !strings.Contains(out, "enabled") {
		t.Errorf("list output = %q, want immich listed and enabled", out)
	}

	if _, err := execCLI(t, "schedule", "disable", "immich"); err != nil {
		t.Fatalf("schedule disable: %v", err)
	}
	out, err = execCLI(t, "schedule", "list")
	if err != nil {
		t.Fatalf("schedule list: %v", err)
	}
	if !strings.Contains(out, "disabled") {
		t.Errorf("list output after disable = %q, want disabled", out)
	}

	if _, err := execCLI(t, "schedule", "enable", "immich"); err != nil {
		t.Fatalf("schedule enable: %v", err)
	}
	out, err = execCLI(t, "schedule", "list")
	if err != nil {
		t.Fatalf("schedule list: %v", err)
	}
	if !strings.Contains(out, "enabled") || strings.Contains(out, "disabled") {
		t.Errorf("list output after re-enable = %q, want enabled only", out)
	}
}

func TestScheduleSetInvalidCron(t *testing.T) {
	cliEnv(t, &spyRunner{})
	_, err := execCLI(t, "schedule", "set", "immich", "not a cron expression")
	if err == nil {
		t.Error("schedule set with an invalid cron expression: want an error")
	}
}

func TestScheduleEnableUnknownService(t *testing.T) {
	cliEnv(t, &spyRunner{})
	_, err := execCLI(t, "schedule", "enable", "ghost")
	if err == nil {
		t.Error("schedule enable for a service with no schedule: want an error")
	}
}

func TestScheduleSetKeepsPaused(t *testing.T) {
	tests := []struct {
		name       string
		pause      bool
		wantOut    string
		wantListed string
	}{
		{"enabled shows the next run", false, "Runs at the next check, then", "enabled"},
		{"paused stays paused", true, "paused", "disabled"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cliEnv(t, &spyRunner{})
			if _, err := execCLI(t, "schedule", "set", "immich", "0 3 * * *"); err != nil {
				t.Fatalf("schedule set: %v", err)
			}
			if tc.pause {
				if _, err := execCLI(t, "schedule", "disable", "immich"); err != nil {
					t.Fatalf("schedule disable: %v", err)
				}
			}
			out, err := execCLI(t, "schedule", "set", "immich", "30 2 * * *")
			if err != nil {
				t.Fatalf("schedule set: %v", err)
			}
			if !strings.Contains(out, tc.wantOut) {
				t.Errorf("set output = %q, want it to contain %q", out, tc.wantOut)
			}
			list, err := execCLI(t, "schedule", "list")
			if err != nil {
				t.Fatalf("schedule list: %v", err)
			}
			if !strings.Contains(list, tc.wantListed) || !strings.Contains(list, "30 2 * * *") {
				t.Errorf("list = %q, want %q and the new cron", list, tc.wantListed)
			}
			if tc.pause && strings.Contains(list, "enabled") {
				t.Errorf("list = %q, a paused schedule must not be re-enabled by saving", list)
			}
		})
	}
}

func TestScheduleTextDueNow(t *testing.T) {
	// Tuesday 2026-11-03 02:00 UTC; a "0 1 * * *" schedule last fired Monday.
	next := time.Date(2026, 11, 4, 1, 0, 0, 0, time.UTC)
	mk := func(enabled, due bool, n *time.Time) api.ScheduleInfo {
		return api.ScheduleInfo{ScheduledJob: scheduler.ScheduledJob{Service: "svc", Cron: "0 1 * * *", Enabled: enabled}, NextRun: n, DueNow: due}
	}
	tests := []struct {
		name       string
		info       api.ScheduleInfo
		wantText   string
		wantNotice string
	}{
		{"due now", mk(true, true, &next), "Runs at the next check, then Wed 2026-11-04 01:00", "straight away"},
		{"not due", mk(true, false, &next), "Next run: Wed 2026-11-04 01:00", "never starts a backup by surprise"},
		{"paused", mk(false, false, nil), "paused", "never starts a backup by surprise"},
		{"paused never reports due", mk(false, true, nil), "paused", "never starts a backup by surprise"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextRunText(tc.info, time.UTC); got != tc.wantText {
				t.Errorf("nextRunText = %q, want %q", got, tc.wantText)
			}
			if got := surpriseNotice(tc.info); !strings.Contains(got, tc.wantNotice) {
				t.Errorf("surpriseNotice = %q, want it to contain %q", got, tc.wantNotice)
			}
		})
	}
}

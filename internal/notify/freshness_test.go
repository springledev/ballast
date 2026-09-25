package notify

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/springledev/ballast/internal/engine"
	"github.com/springledev/ballast/internal/store"
)

func TestFreshnessChecker_NoSuccessfulRun(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	m, err := engine.StartRun(db.DB(), "test-service")
	if err != nil {
		t.Fatalf("StartRun error: %v", err)
	}
	if err := m.Transition(engine.PREPARING, ""); err != nil {
		t.Fatalf("Transition error: %v", err)
	}

	checker := NewFreshnessChecker(db.DB())
	stale, age, err := checker.IsStale("test-service", 48*time.Hour)
	if err != nil {
		t.Errorf("IsStale error: %v", err)
	}

	if !stale {
		t.Errorf("IsStale() = %v, want true (no successful run)", stale)
	}

	if age != neverBackedUpAge {
		t.Errorf("age = %v, want %v for never-backed-up", age, neverBackedUpAge)
	}
}

func TestFreshnessChecker_WithinMaxAge(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	startSuccessfulRun(t, db, "test-service")

	checker := NewFreshnessChecker(db.DB())

	stale, age, err := checker.IsStale("test-service", 48*time.Hour)
	if err != nil {
		t.Errorf("IsStale error: %v", err)
	}

	if stale {
		t.Errorf("IsStale() = %v, want false (recent successful run)", stale)
	}

	if age > 1*time.Minute {
		t.Errorf("age = %v, want close to 0", age)
	}
}

func TestFreshnessChecker_ExceedsMaxAge(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	startSuccessfulRun(t, db, "test-service")

	checker := NewFreshnessChecker(db.DB())

	// Fake clock set to 3 days later
	fakeNow := time.Now().UTC().Add(72 * time.Hour)
	checker.SetClock(func() time.Time { return fakeNow })

	stale, age, err := checker.IsStale("test-service", 48*time.Hour)
	if err != nil {
		t.Errorf("IsStale error: %v", err)
	}

	if !stale {
		t.Errorf("IsStale() = %v, want true (exceeded max age)", stale)
	}

	if age < 70*time.Hour || age > 74*time.Hour {
		t.Errorf("age = %v, want ~72h", age)
	}
}

func TestFreshnessChecker_SuccessUnverifiedNotCounted(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	// Create a run as SUCCESS_UNVERIFIED (amber)
	m, err := engine.StartRun(db.DB(), "test-service")
	if err != nil {
		t.Fatalf("StartRun error: %v", err)
	}
	for _, state := range []engine.State{engine.PREPARING, engine.DUMPING, engine.UPLOADING, engine.VERIFYING} {
		if err := m.Transition(state, ""); err != nil {
			t.Fatalf("Transition error: %v", err)
		}
	}
	if err := m.Transition(engine.SUCCESS_UNVERIFIED, ""); err != nil {
		t.Fatalf("Transition error: %v", err)
	}

	checker := NewFreshnessChecker(db.DB())

	// Amber backup should NOT count as fresh
	stale, age, err := checker.IsStale("test-service", 48*time.Hour)
	if err != nil {
		t.Errorf("IsStale error: %v", err)
	}

	if !stale {
		t.Errorf("IsStale() = %v, want true (amber doesn't count as fresh)", stale)
	}

	if age != neverBackedUpAge {
		t.Errorf("age = %v, want %v (no successful run)", age, neverBackedUpAge)
	}
}

func TestFreshnessChecker_LastSuccessfulRun(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	startSuccessfulRun(t, db, "test-service")

	checker := NewFreshnessChecker(db.DB())

	lastRun, err := checker.LastSuccessfulRun("test-service")
	if err != nil {
		t.Errorf("LastSuccessfulRun error: %v", err)
	}

	if lastRun.IsZero() {
		t.Errorf("LastSuccessfulRun() is zero, want a valid time")
	}

	now := time.Now().UTC()
	if lastRun.Before(now.Add(-1*time.Minute)) || lastRun.After(now.Add(1*time.Minute)) {
		t.Errorf("LastSuccessfulRun() = %v, expected recent time near %v", lastRun, now)
	}
}

func TestFreshnessChecker_MultipleServices(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	for _, svc := range []string{"immich", "vaultwarden"} {
		startSuccessfulRun(t, db, svc)
	}

	checker := NewFreshnessChecker(db.DB())

	stale1, _, _ := checker.IsStale("immich", 48*time.Hour)
	stale2, _, _ := checker.IsStale("vaultwarden", 48*time.Hour)

	if stale1 || stale2 {
		t.Errorf("IsStale() = immich:%v, vaultwarden:%v, both want false", stale1, stale2)
	}

	stale3, _, _ := checker.IsStale("nonexistent", 48*time.Hour)
	if !stale3 {
		t.Errorf("IsStale(nonexistent) = %v, want true", stale3)
	}
}

func TestFreshnessChecker_NoRunAtAll(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	checker := NewFreshnessChecker(db.DB())

	lastRun, err := checker.LastSuccessfulRun("nonexistent-service")
	if err != nil {
		t.Errorf("LastSuccessfulRun error: %v", err)
	}

	if !lastRun.IsZero() {
		t.Errorf("LastSuccessfulRun() = %v, want zero time for nonexistent service", lastRun)
	}
}

// fakeChannel records events for testing.
type fakeChannel struct {
	events []Event
}

func (f *fakeChannel) Send(ctx context.Context, e Event) error {
	f.events = append(f.events, e)
	return nil
}

func (f *fakeChannel) Type() string { return "fake" }

// failingChannel fails every call.
type failingChannel struct{}

func (f *failingChannel) Send(ctx context.Context, e Event) error {
	return fmt.Errorf("intentional failure for testing")
}

func (f *failingChannel) Type() string { return "failing" }

// sweepStep is one sweep in a TestSweepFreshness scenario: optional
// setup mutations, then a sweep at a given clock offset, then
// assertions on that sweep's outcome.
type sweepStep struct {
	label string

	clock       time.Duration // now, as an offset from the scenario's base time
	resucceed   bool          // record a fresh SUCCESS run before sweeping
	manualClear bool          // simulate an external clear: UPDATE is_stale = 0
	channel     Channel       // swap the registered channel before sweeping (nil = keep current)

	wantErr    bool
	wantEvents int // only checked when channel is a *fakeChannel

	checkDB func(t *testing.T, db *sql.DB, service string)
}

// TestSweepFreshness exercises SweepFreshness's stale/fresh dedup
// state machine as a table of scenarios, each a sequence of sweeps
// against one service (spec §11 "freshness breach (nightly sweep)").
func TestSweepFreshness(t *testing.T) {
	tests := []struct {
		name        string
		maxAgeHours int
		steps       []sweepStep
	}{
		{
			name:        "fresh service sends no alert",
			maxAgeHours: 48,
			steps: []sweepStep{
				{label: "sweep while fresh", clock: 0, wantEvents: 0},
			},
		},
		{
			name:        "newly stale sends exactly one alert",
			maxAgeHours: 24,
			steps: []sweepStep{
				{label: "sweep past max age", clock: 48 * time.Hour, wantEvents: 1},
			},
		},
		{
			name:        "already-reported stale service is not re-alerted",
			maxAgeHours: 24,
			steps: []sweepStep{
				{label: "first stale sweep", clock: 48 * time.Hour, wantEvents: 1},
				{label: "second sweep, still stale", clock: 48 * time.Hour, wantEvents: 0},
			},
		},
		{
			name:        "alert clears once the service is fresh again",
			maxAgeHours: 24,
			steps: []sweepStep{
				{label: "first stale sweep", clock: 48 * time.Hour, wantEvents: 1},
				{
					label: "fresh again after a new backup", clock: 50 * time.Hour, resucceed: true, wantEvents: 0,
					checkDB: func(t *testing.T, db *sql.DB, service string) {
						var isStale int
						var reportedAt sql.NullString
						if err := db.QueryRow(`SELECT is_stale, reported_at FROM freshness_alerts WHERE service = ?`, service).Scan(&isStale, &reportedAt); err != nil {
							t.Fatalf("querying freshness_alerts: %v", err)
						}
						if isStale != 0 {
							t.Errorf("is_stale = %d, want 0 (cleared now that the service is fresh again)", isStale)
						}
						if reportedAt.Valid {
							t.Errorf("reported_at = %q, want NULL (cleared now that the service is fresh again)", reportedAt.String)
						}
					},
				},
			},
		},
		{
			name:        "externally cleared but still stale alerts again",
			maxAgeHours: 24,
			steps: []sweepStep{
				{label: "first stale sweep", clock: 48 * time.Hour, wantEvents: 1},
				{label: "still stale after manual clear", clock: 96 * time.Hour, manualClear: true, wantEvents: 1},
			},
		},
		{
			name:        "send failure is not recorded and is retried next sweep",
			maxAgeHours: 24,
			steps: []sweepStep{
				{
					label:   "stale, channel fails",
					clock:   48 * time.Hour,
					channel: &failingChannel{},
					wantErr: true,
					checkDB: func(t *testing.T, db *sql.DB, service string) {
						var isStale int
						if err := db.QueryRow(`SELECT is_stale FROM freshness_alerts WHERE service = ?`, service).Scan(&isStale); err != nil {
							t.Fatalf("freshness_alerts row must exist (checked, even though Send failed): %v", err)
						}
						if isStale != 0 {
							t.Error("alert must not be recorded as reported when Send failed")
						}
					},
				},
				{label: "retry with a working channel", clock: 48 * time.Hour, channel: &fakeChannel{}, wantEvents: 1},
			},
		},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := testDB(t)
			defer db.Close()
			service := fmt.Sprintf("svc-sweep-%d", i)

			startSuccessfulRun(t, db, service)
			if _, err := db.DB().Exec(
				`INSERT INTO notification_settings (service, enabled, max_age_hours, updated_at) VALUES (?, 1, ?, ?)`,
				service, tt.maxAgeHours, time.Now().UTC().Format(time.RFC3339Nano),
			); err != nil {
				t.Fatalf("inserting notification_settings: %v", err)
			}

			checker := NewFreshnessChecker(db.DB())
			baseTime := time.Now().UTC()

			notifier := NewNotifier()
			current := &fakeChannel{}
			notifier.RegisterChannel(service, current)

			for _, st := range tt.steps {
				checkClock := baseTime.Add(st.clock)

				if st.resucceed {
					m := startSuccessfulRun(t, db, service)
					// Stamp the resucceeded run's finished_at with the
					// sweep's injected clock time, not the real wall
					// clock startSuccessfulRun used: the fake clock in
					// these scenarios is far ahead of real time, so a
					// run genuinely finished "now" (by the real clock)
					// would still read as stale under it, and the
					// stale -> fresh clear branch would never run.
					if _, err := db.DB().Exec(`UPDATE runs SET finished_at = ? WHERE id = ?`, checkClock.Format(time.RFC3339Nano), m.Run().ID); err != nil {
						t.Fatalf("%s: stamping resucceeded run's finished_at: %v", st.label, err)
					}
				}
				if st.manualClear {
					if _, err := db.DB().Exec(`UPDATE freshness_alerts SET is_stale = 0 WHERE service = ?`, service); err != nil {
						t.Fatalf("%s: manual clear: %v", st.label, err)
					}
				}
				if st.channel != nil {
					notifier = NewNotifier()
					notifier.RegisterChannel(service, st.channel)
					if fc, ok := st.channel.(*fakeChannel); ok {
						current = fc
					} else {
						current = nil
					}
				}

				if current != nil {
					current.events = nil
				}

				checker.SetClock(func() time.Time { return checkClock })

				err := checker.SweepFreshness(context.Background(), notifier)
				if (err != nil) != st.wantErr {
					t.Fatalf("%s: SweepFreshness() error = %v, wantErr %v", st.label, err, st.wantErr)
				}
				if current != nil && len(current.events) != st.wantEvents {
					t.Errorf("%s: events = %d, want %d", st.label, len(current.events), st.wantEvents)
				}
				if st.checkDB != nil {
					st.checkDB(t, db.DB(), service)
				}
			}
		})
	}
}

// TestSweepFreshness_WeeklyScheduleDerivesDefaultMaxAge pins the
// weekly max-age default (spec §11: weekly → 9 days) being derived
// from the service's schedule when notification_settings has no
// explicit max_age_hours override (NULL).
func TestSweepFreshness_WeeklyScheduleDerivesDefaultMaxAge(t *testing.T) {
	db := testDB(t)
	defer db.Close()
	service := "weekly-service"

	startSuccessfulRun(t, db, service)

	if _, err := db.DB().Exec(
		`INSERT INTO schedules (service, cron_expr, enabled, updated_at) VALUES (?, ?, 1, ?)`,
		service, "0 3 * * 1", time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatalf("inserting schedule: %v", err)
	}
	// No explicit max_age_hours: NULL means "derive from the schedule".
	if _, err := db.DB().Exec(
		`INSERT INTO notification_settings (service, enabled, max_age_hours, updated_at) VALUES (?, 1, NULL, ?)`,
		service, time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatalf("inserting notification_settings: %v", err)
	}

	checker := NewFreshnessChecker(db.DB())
	notifier := NewNotifier()
	ch := &fakeChannel{}
	notifier.RegisterChannel(service, ch)

	baseTime := time.Now().UTC()

	// 3 days later: within the weekly 9-day default, must not be stale.
	checker.SetClock(func() time.Time { return baseTime.Add(3 * 24 * time.Hour) })
	if err := checker.SweepFreshness(context.Background(), notifier); err != nil {
		t.Fatalf("sweep at 3 days: %v", err)
	}
	if len(ch.events) != 0 {
		t.Errorf("at 3 days: got %d events, want 0 (within the weekly 9-day default)", len(ch.events))
	}

	// 10 days later: past the weekly 9-day default, must be stale.
	checker.SetClock(func() time.Time { return baseTime.Add(10 * 24 * time.Hour) })
	if err := checker.SweepFreshness(context.Background(), notifier); err != nil {
		t.Fatalf("sweep at 10 days: %v", err)
	}
	if len(ch.events) != 1 {
		t.Errorf("at 10 days: got %d events, want 1 (past the weekly 9-day default)", len(ch.events))
	}
}

// TestSweepFreshness_NoScheduleFallsBackToNightlyDefault covers a
// service with no explicit max_age_hours and no schedules row at all:
// it must fall back to the nightly default rather than error out.
func TestSweepFreshness_NoScheduleFallsBackToNightlyDefault(t *testing.T) {
	db := testDB(t)
	defer db.Close()
	service := "no-schedule-service"

	startSuccessfulRun(t, db, service)
	if _, err := db.DB().Exec(
		`INSERT INTO notification_settings (service, enabled, max_age_hours, updated_at) VALUES (?, 1, NULL, ?)`,
		service, time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatalf("inserting notification_settings: %v", err)
	}

	checker := NewFreshnessChecker(db.DB())
	notifier := NewNotifier()
	ch := &fakeChannel{}
	notifier.RegisterChannel(service, ch)

	baseTime := time.Now().UTC()

	// 72h later: past the nightly 48h default, must be stale.
	checker.SetClock(func() time.Time { return baseTime.Add(72 * time.Hour) })
	if err := checker.SweepFreshness(context.Background(), notifier); err != nil {
		t.Fatalf("sweep at 72h: %v", err)
	}
	if len(ch.events) != 1 {
		t.Errorf("at 72h: got %d events, want 1 (past the nightly 48h fallback default)", len(ch.events))
	}
}

// TestSweepFreshness_TouchesLastCheckedOnEveryCheck pins the
// last_checked_at behavior: it must advance on every sweep that looks
// at a service, not only on a sweep that raises or clears an alert.
func TestSweepFreshness_TouchesLastCheckedOnEveryCheck(t *testing.T) {
	db := testDB(t)
	defer db.Close()
	service := "touch-service"

	startSuccessfulRun(t, db, service)
	if _, err := db.DB().Exec(
		`INSERT INTO notification_settings (service, enabled, max_age_hours, updated_at) VALUES (?, 1, ?, ?)`,
		service, 48, time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatalf("inserting notification_settings: %v", err)
	}

	checker := NewFreshnessChecker(db.DB())
	notifier := NewNotifier()
	notifier.RegisterChannel(service, &fakeChannel{})

	baseTime := time.Now().UTC()

	// First sweep: still fresh, no alert raised or cleared.
	checker.SetClock(func() time.Time { return baseTime.Add(1 * time.Hour) })
	if err := checker.SweepFreshness(context.Background(), notifier); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	first := readLastCheckedAt(t, db.DB(), service)
	if first.IsZero() {
		t.Fatal("last_checked_at must be recorded on a sweep that neither raises nor clears an alert")
	}

	// Second sweep, still fresh, at a later clock: last_checked_at must
	// advance even though nothing about the alert state changed.
	checker.SetClock(func() time.Time { return baseTime.Add(2 * time.Hour) })
	if err := checker.SweepFreshness(context.Background(), notifier); err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	second := readLastCheckedAt(t, db.DB(), service)
	if !second.After(first) {
		t.Errorf("last_checked_at = %v, want later than %v after a second, still-fresh sweep", second, first)
	}
}

// TestSweepFreshness_ServiceWithNoNotificationSettingsRowIsStillSwept
// pins finding 5: a service that has run (so it has a jobs row) but
// has never had a notification_settings row created for it must still
// be swept with default settings (enabled, nightly default max-age) —
// not silently skipped just because nobody has visited the
// notifications page for it yet.
func TestSweepFreshness_ServiceWithNoNotificationSettingsRowIsStillSwept(t *testing.T) {
	db := testDB(t)
	defer db.Close()
	service := "no-settings-row-service"

	startSuccessfulRun(t, db, service)
	// Deliberately no notification_settings row at all.

	checker := NewFreshnessChecker(db.DB())
	notifier := NewNotifier()
	ch := &fakeChannel{}
	notifier.RegisterChannel(service, ch)

	baseTime := time.Now().UTC()
	// 72h later: past the nightly 48h fallback default.
	checker.SetClock(func() time.Time { return baseTime.Add(72 * time.Hour) })
	if err := checker.SweepFreshness(context.Background(), notifier); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(ch.events) != 1 {
		t.Errorf("events = %d, want 1 (a service with no notification_settings row must still be swept)", len(ch.events))
	}
}

// TestSweepFreshness_ScheduledButNeverRunIsSwept pins finding 5's
// "union of jobs and schedules" requirement: a service that has a
// schedule but has never actually run (no jobs row at all) must still
// be included in the sweep, and — having no successful run — reports
// as stale immediately.
func TestSweepFreshness_ScheduledButNeverRunIsSwept(t *testing.T) {
	db := testDB(t)
	defer db.Close()
	service := "scheduled-never-run-service"

	if _, err := db.DB().Exec(
		`INSERT INTO schedules (service, cron_expr, enabled, updated_at) VALUES (?, ?, 1, ?)`,
		service, "0 3 * * *", time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatalf("inserting schedule: %v", err)
	}
	// No jobs row (never run) and no notification_settings row.

	checker := NewFreshnessChecker(db.DB())
	notifier := NewNotifier()
	ch := &fakeChannel{}
	notifier.RegisterChannel(service, ch)

	if err := checker.SweepFreshness(context.Background(), notifier); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(ch.events) != 1 {
		t.Errorf("events = %d, want 1 (a scheduled-but-never-run service must be swept and reported stale)", len(ch.events))
	}
}

// TestSweepFreshness_OptedOutServiceIsSkipped pins finding 5's
// opt-out behavior: notification_settings.enabled = 0 removes a
// service from the sweep entirely — no staleness check, no
// notification, no freshness_alerts bookkeeping — even though it
// would otherwise be stale and has no registered channel (which would
// otherwise surface as an ErrNoChannels error).
func TestSweepFreshness_OptedOutServiceIsSkipped(t *testing.T) {
	db := testDB(t)
	defer db.Close()
	service := "opted-out-service"

	startSuccessfulRun(t, db, service)
	if _, err := db.DB().Exec(
		`INSERT INTO notification_settings (service, enabled, updated_at) VALUES (?, 0, ?)`,
		service, time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatalf("inserting notification_settings: %v", err)
	}

	checker := NewFreshnessChecker(db.DB())
	notifier := NewNotifier() // no channel registered for this service at all

	baseTime := time.Now().UTC()
	checker.SetClock(func() time.Time { return baseTime.Add(30 * 24 * time.Hour) })
	if err := checker.SweepFreshness(context.Background(), notifier); err != nil {
		t.Fatalf("sweep of an opted-out service must not error even with no channel registered: %v", err)
	}

	var count int
	if err := db.DB().QueryRow(`SELECT COUNT(*) FROM freshness_alerts WHERE service = ?`, service).Scan(&count); err != nil {
		t.Fatalf("querying freshness_alerts: %v", err)
	}
	if count != 0 {
		t.Errorf("freshness_alerts rows for opted-out service = %d, want 0 (never even checked)", count)
	}
}

// TestSweepFreshness_DisabledScheduleIsSkipped covers review finding
// 2: a service whose schedule is disabled (scheduler.SetEnabled(false),
// so run-due — see internal/scheduler/coordinator.go — skips it) must
// not generate freshness-breach alerts even though it would otherwise
// be well past its max age. Like the notification_settings opt-out
// case, no channel is registered at all, to prove the service is
// excluded before ever reaching Send (which would otherwise surface
// ErrNoChannels).
func TestSweepFreshness_DisabledScheduleIsSkipped(t *testing.T) {
	db := testDB(t)
	defer db.Close()
	service := "disabled-schedule-service"

	startSuccessfulRun(t, db, service)
	if _, err := db.DB().Exec(
		`INSERT INTO schedules (service, cron_expr, enabled, updated_at) VALUES (?, ?, 0, ?)`,
		service, "0 3 * * *", time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatalf("inserting disabled schedule: %v", err)
	}

	checker := NewFreshnessChecker(db.DB())
	notifier := NewNotifier() // no channel registered for this service at all

	baseTime := time.Now().UTC()
	checker.SetClock(func() time.Time { return baseTime.Add(30 * 24 * time.Hour) })
	if err := checker.SweepFreshness(context.Background(), notifier); err != nil {
		t.Fatalf("sweep of a disabled-schedule service must not error even with no channel registered: %v", err)
	}

	var count int
	if err := db.DB().QueryRow(`SELECT COUNT(*) FROM freshness_alerts WHERE service = ?`, service).Scan(&count); err != nil {
		t.Fatalf("querying freshness_alerts: %v", err)
	}
	if count != 0 {
		t.Errorf("freshness_alerts rows for disabled-schedule service = %d, want 0 (never even checked)", count)
	}
}

// TestSweepFreshness_EnabledScheduleIsStillSwept is the control for
// TestSweepFreshness_DisabledScheduleIsSkipped: a service whose
// schedule row explicitly has enabled = 1 must still be swept
// normally, so the exclusion really is keyed on the disabled state and
// not on the mere presence of a schedules row.
func TestSweepFreshness_EnabledScheduleIsStillSwept(t *testing.T) {
	db := testDB(t)
	defer db.Close()
	service := "enabled-schedule-service"

	startSuccessfulRun(t, db, service)
	if _, err := db.DB().Exec(
		`INSERT INTO schedules (service, cron_expr, enabled, updated_at) VALUES (?, ?, 1, ?)`,
		service, "0 3 * * *", time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatalf("inserting enabled schedule: %v", err)
	}

	checker := NewFreshnessChecker(db.DB())
	notifier := NewNotifier()
	ch := &fakeChannel{}
	notifier.RegisterChannel(service, ch)

	baseTime := time.Now().UTC()
	checker.SetClock(func() time.Time { return baseTime.Add(30 * 24 * time.Hour) })
	if err := checker.SweepFreshness(context.Background(), notifier); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(ch.events) != 1 {
		t.Errorf("events = %d, want 1 (an enabled schedule must still be swept)", len(ch.events))
	}
}

// TestSweepFreshness_NoChannelSurfacesErrNoChannelsAndDoesNotRecord
// pins finding 6 for the sweep path specifically: a stale service
// with a notification_settings row (so it is not opted out) but no
// registered channel must have SweepFreshness surface ErrNoChannels
// as a real error, and must not record the breach as reported, so the
// next sweep retries it instead of staying silent forever.
func TestSweepFreshness_NoChannelSurfacesErrNoChannelsAndDoesNotRecord(t *testing.T) {
	db := testDB(t)
	defer db.Close()
	service := "no-channel-service"

	startSuccessfulRun(t, db, service)
	if _, err := db.DB().Exec(
		`INSERT INTO notification_settings (service, enabled, max_age_hours, updated_at) VALUES (?, 1, 24, ?)`,
		service, time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatalf("inserting notification_settings: %v", err)
	}

	checker := NewFreshnessChecker(db.DB())
	notifier := NewNotifier() // no channel registered for this service

	baseTime := time.Now().UTC()
	checker.SetClock(func() time.Time { return baseTime.Add(48 * time.Hour) })
	err := checker.SweepFreshness(context.Background(), notifier)
	if !errors.Is(err, ErrNoChannels) {
		t.Fatalf("SweepFreshness() error = %v, want it to wrap ErrNoChannels", err)
	}

	var isStale int
	if err := db.DB().QueryRow(`SELECT is_stale FROM freshness_alerts WHERE service = ?`, service).Scan(&isStale); err != nil {
		t.Fatalf("freshness_alerts row must exist (checked, even though Send failed): %v", err)
	}
	if isStale != 0 {
		t.Error("alert must not be recorded as reported when Send fails with ErrNoChannels")
	}
}

// TestClearFreshnessAlert_ClearsReportedAt is a direct unit test of
// clearFreshnessAlert (finding 12): both is_stale and reported_at must
// be reset, not is_stale alone.
func TestClearFreshnessAlert_ClearsReportedAt(t *testing.T) {
	db := testDB(t)
	defer db.Close()
	service := "clear-alert-service"

	checker := NewFreshnessChecker(db.DB())
	now := time.Now().UTC()
	if err := checker.recordFreshnessAlert(service, now); err != nil {
		t.Fatalf("recordFreshnessAlert: %v", err)
	}

	// Sanity check: recording actually set both columns, so clearing
	// them is a meaningful assertion below.
	var isStale int
	var reportedAt sql.NullString
	if err := db.DB().QueryRow(`SELECT is_stale, reported_at FROM freshness_alerts WHERE service = ?`, service).Scan(&isStale, &reportedAt); err != nil {
		t.Fatalf("querying freshness_alerts after recording: %v", err)
	}
	if isStale != 1 || !reportedAt.Valid {
		t.Fatalf("freshness_alerts after recording = is_stale:%d reported_at:%v, want is_stale:1 reported_at:set", isStale, reportedAt)
	}

	if err := checker.clearFreshnessAlert(service); err != nil {
		t.Fatalf("clearFreshnessAlert: %v", err)
	}

	if err := db.DB().QueryRow(`SELECT is_stale, reported_at FROM freshness_alerts WHERE service = ?`, service).Scan(&isStale, &reportedAt); err != nil {
		t.Fatalf("querying freshness_alerts after clearing: %v", err)
	}
	if isStale != 0 {
		t.Errorf("is_stale = %d, want 0 after clearing", isStale)
	}
	if reportedAt.Valid {
		t.Errorf("reported_at = %q, want NULL after clearing", reportedAt.String)
	}
}

func readLastCheckedAt(t *testing.T, db *sql.DB, service string) time.Time {
	t.Helper()
	var raw string
	if err := db.QueryRow(`SELECT last_checked_at FROM freshness_alerts WHERE service = ?`, service).Scan(&raw); err != nil {
		t.Fatalf("reading last_checked_at for %s: %v", service, err)
	}
	ts, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		t.Fatalf("parsing last_checked_at %q: %v", raw, err)
	}
	return ts
}

// startSuccessfulRun drives a fresh run for service all the way
// through to SUCCESS, the boilerplate shared by most freshness tests.
func startSuccessfulRun(t *testing.T, db *store.Store, service string) *engine.Machine {
	t.Helper()
	m, err := engine.StartRun(db.DB(), service)
	if err != nil {
		t.Fatalf("StartRun(%s): %v", service, err)
	}
	for _, s := range []engine.State{engine.PREPARING, engine.DUMPING, engine.UPLOADING, engine.VERIFYING, engine.SUCCESS} {
		if err := m.Transition(s, ""); err != nil {
			t.Fatalf("Transition(%s, %s): %v", service, s, err)
		}
	}
	return m
}

// testDB creates a temporary test database with migrations.
func testDB(t *testing.T) *store.Store {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	return db
}

package scheduler

import (
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/springledev/ballast/internal/store"
)

func openTestDB(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "ballast.db"))
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestSetScheduleRejectsInvalidCron(t *testing.T) {
	s := openTestDB(t)
	if err := SetSchedule(s.DB(), "immich", "not a cron"); err == nil {
		t.Error("SetSchedule with an invalid cron expression should error")
	}
}

func TestSetScheduleRejectsEmptyService(t *testing.T) {
	s := openTestDB(t)
	if err := SetSchedule(s.DB(), "", "0 3 * * *"); err == nil {
		t.Error("SetSchedule with an empty service should error")
	}
}

func TestSetAndListSchedule(t *testing.T) {
	s := openTestDB(t)
	if err := SetSchedule(s.DB(), "immich", "0 3 * * *"); err != nil {
		t.Fatalf("SetSchedule: %v", err)
	}
	if err := SetSchedule(s.DB(), "vaultwarden", "0 3 * * 0"); err != nil {
		t.Fatalf("SetSchedule: %v", err)
	}

	jobs, err := ListSchedules(s.DB())
	if err != nil {
		t.Fatalf("ListSchedules: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("len(jobs) = %d, want 2", len(jobs))
	}
	byService := map[string]ScheduledJob{}
	for _, j := range jobs {
		byService[j.Service] = j
	}
	if got := byService["immich"].Cron; got != "0 3 * * *" {
		t.Errorf("immich cron = %q, want %q", got, "0 3 * * *")
	}
	if !byService["immich"].Enabled {
		t.Error("a newly set schedule should be enabled")
	}
}

func TestListSchedulesOrderedByService(t *testing.T) {
	s := openTestDB(t)
	for _, svc := range []string{"vaultwarden", "immich", "paperless"} {
		if err := SetSchedule(s.DB(), svc, "0 3 * * *"); err != nil {
			t.Fatalf("SetSchedule %s: %v", svc, err)
		}
	}
	jobs, err := ListSchedules(s.DB())
	if err != nil {
		t.Fatalf("ListSchedules: %v", err)
	}
	var got []string
	for _, j := range jobs {
		got = append(got, j.Service)
	}
	want := []string{"immich", "paperless", "vaultwarden"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("ListSchedules order = %v, want %v", got, want)
	}
}

func TestSetScheduleUpsertsExisting(t *testing.T) {
	s := openTestDB(t)
	if err := SetSchedule(s.DB(), "immich", "0 3 * * *"); err != nil {
		t.Fatalf("SetSchedule: %v", err)
	}
	if err := SetSchedule(s.DB(), "immich", "30 2 * * *"); err != nil {
		t.Fatalf("SetSchedule (update): %v", err)
	}
	jobs, err := ListSchedules(s.DB())
	if err != nil {
		t.Fatalf("ListSchedules: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("len(jobs) = %d, want 1 (upsert, not duplicate)", len(jobs))
	}
	if jobs[0].Cron != "30 2 * * *" {
		t.Errorf("cron = %q, want updated value", jobs[0].Cron)
	}
}

func TestSetScheduleKeepsLastFiring(t *testing.T) {
	s := openTestDB(t)
	if err := SetSchedule(s.DB(), "immich", "0 3 * * *"); err != nil {
		t.Fatalf("SetSchedule: %v", err)
	}
	fired := time.Date(2026, 3, 5, 3, 0, 0, 0, time.UTC)
	if err := MarkFired(s.DB(), "immich", fired); err != nil {
		t.Fatalf("MarkFired: %v", err)
	}
	if err := SetSchedule(s.DB(), "immich", "30 2 * * *"); err != nil {
		t.Fatalf("SetSchedule (update): %v", err)
	}
	got, err := LastFired(s.DB(), "immich")
	if err != nil {
		t.Fatalf("LastFired: %v", err)
	}
	if !got.Equal(fired) {
		t.Errorf("LastFired = %v, want %v kept: changing a schedule must not cause an extra immediate run", got, fired)
	}
}

func TestSetEnabledTogglesWithoutLosingCron(t *testing.T) {
	s := openTestDB(t)
	if err := SetSchedule(s.DB(), "immich", "0 3 * * *"); err != nil {
		t.Fatalf("SetSchedule: %v", err)
	}
	if err := SetEnabled(s.DB(), "immich", false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	jobs, err := ListSchedules(s.DB())
	if err != nil {
		t.Fatalf("ListSchedules: %v", err)
	}
	if jobs[0].Enabled {
		t.Error("expected schedule to be disabled")
	}
	if jobs[0].Cron != "0 3 * * *" {
		t.Errorf("cron changed after SetEnabled: got %q", jobs[0].Cron)
	}
}

func TestSetEnabledUnknownServiceErrors(t *testing.T) {
	s := openTestDB(t)
	if err := SetEnabled(s.DB(), "nope", false); err == nil {
		t.Error("SetEnabled for an unconfigured service should error")
	}
}

func TestGenerateAndVerifyTriggerToken(t *testing.T) {
	s := openTestDB(t)
	token, err := GenerateTriggerToken(s.DB(), "immich")
	if err != nil {
		t.Fatalf("GenerateTriggerToken: %v", err)
	}
	if token == "" {
		t.Fatal("expected a non-empty token")
	}

	ok, err := VerifyTriggerToken(s.DB(), "immich", token)
	if err != nil {
		t.Fatalf("VerifyTriggerToken: %v", err)
	}
	if !ok {
		t.Error("expected the freshly generated token to verify")
	}

	ok, err = VerifyTriggerToken(s.DB(), "immich", "wrong-token")
	if err != nil {
		t.Fatalf("VerifyTriggerToken: %v", err)
	}
	if ok {
		t.Error("expected a wrong token not to verify")
	}
}

func TestVerifyTriggerTokenUnknownServiceOrEmptyToken(t *testing.T) {
	s := openTestDB(t)
	ok, err := VerifyTriggerToken(s.DB(), "nope", "anything")
	if err != nil {
		t.Fatalf("VerifyTriggerToken: %v", err)
	}
	if ok {
		t.Error("unconfigured service must never verify")
	}

	if _, err := GenerateTriggerToken(s.DB(), "immich"); err != nil {
		t.Fatalf("GenerateTriggerToken: %v", err)
	}
	ok, err = VerifyTriggerToken(s.DB(), "immich", "")
	if err != nil {
		t.Fatalf("VerifyTriggerToken: %v", err)
	}
	if ok {
		t.Error("an empty token must never verify")
	}
}

// TestNoTokenHashCannotMatch guards the stand-in hash compared for a
// service without a token: it must be as long as a real hash (same
// comparison work) yet impossible for any token to hash to.
func TestNoTokenHashCannotMatch(t *testing.T) {
	genuine := hashToken("anything")
	if len(noTokenHash) != len(genuine) {
		t.Errorf("len(noTokenHash) = %d, want %d (a real hash's length)", len(noTokenHash), len(genuine))
	}
	if _, err := hex.DecodeString(noTokenHash); err == nil {
		t.Error("noTokenHash is valid hex, so a token could hash to it")
	}
}

func TestVerifyTriggerTokenIsPerService(t *testing.T) {
	s := openTestDB(t)
	token, err := GenerateTriggerToken(s.DB(), "immich")
	if err != nil {
		t.Fatalf("GenerateTriggerToken: %v", err)
	}
	ok, err := VerifyTriggerToken(s.DB(), "vaultwarden", token)
	if err != nil {
		t.Fatalf("VerifyTriggerToken: %v", err)
	}
	if ok {
		t.Error("immich's token must not verify for a service without a token")
	}
}

func TestGenerateTriggerTokenNeverStoresRawToken(t *testing.T) {
	s := openTestDB(t)
	token, err := GenerateTriggerToken(s.DB(), "immich")
	if err != nil {
		t.Fatalf("GenerateTriggerToken: %v", err)
	}
	var stored string
	if err := s.DB().QueryRow(`SELECT token_hash FROM trigger_tokens WHERE service = ?`, "immich").Scan(&stored); err != nil {
		t.Fatalf("reading stored token hash: %v", err)
	}
	if stored == token {
		t.Fatal("the raw token must never be stored, only its hash (spec rule 2)")
	}
	if stored != hashToken(token) {
		t.Error("stored value should be the SHA-256 hash of the token")
	}
}

func TestGenerateTriggerTokenRotatesPreviousToken(t *testing.T) {
	s := openTestDB(t)
	first, err := GenerateTriggerToken(s.DB(), "immich")
	if err != nil {
		t.Fatalf("GenerateTriggerToken: %v", err)
	}
	second, err := GenerateTriggerToken(s.DB(), "immich")
	if err != nil {
		t.Fatalf("GenerateTriggerToken: %v", err)
	}
	if first == second {
		t.Fatal("expected a freshly generated token to differ from the previous one")
	}
	ok, err := VerifyTriggerToken(s.DB(), "immich", first)
	if err != nil {
		t.Fatalf("VerifyTriggerToken: %v", err)
	}
	if ok {
		t.Error("rotated-out token must no longer verify")
	}
	ok, err = VerifyTriggerToken(s.DB(), "immich", second)
	if err != nil {
		t.Fatalf("VerifyTriggerToken: %v", err)
	}
	if !ok {
		t.Error("current token must verify")
	}
}

func TestLastFiredAndMarkFired(t *testing.T) {
	s := openTestDB(t)

	got, err := LastFired(s.DB(), "immich")
	if err != nil || !got.IsZero() {
		t.Fatalf("LastFired with no schedule = %v, %v; want zero, nil", got, err)
	}
	if err := MarkFired(s.DB(), "immich", time.Now()); err == nil {
		t.Error("MarkFired with no schedule configured should error")
	}

	if err := SetSchedule(s.DB(), "immich", "0 3 * * *"); err != nil {
		t.Fatalf("SetSchedule: %v", err)
	}
	if got, err := LastFired(s.DB(), "immich"); err != nil || !got.IsZero() {
		t.Fatalf("LastFired for a never-fired schedule = %v, %v; want zero, nil", got, err)
	}

	at := time.Date(2026, 3, 5, 3, 0, 12, 345, time.FixedZone("UTC+10", 10*60*60))
	if err := MarkFired(s.DB(), "immich", at); err != nil {
		t.Fatalf("MarkFired: %v", err)
	}
	got, err = LastFired(s.DB(), "immich")
	if err != nil {
		t.Fatalf("LastFired: %v", err)
	}
	if !got.Equal(at) {
		t.Errorf("LastFired = %v, want %v", got, at)
	}

	// A zero time clears the record again.
	if err := MarkFired(s.DB(), "immich", time.Time{}); err != nil {
		t.Fatalf("MarkFired(zero): %v", err)
	}
	if got, err := LastFired(s.DB(), "immich"); err != nil || !got.IsZero() {
		t.Fatalf("LastFired after clearing = %v, %v; want zero, nil", got, err)
	}
	if err := MarkFired(s.DB(), "immich", at); err != nil {
		t.Fatalf("MarkFired: %v", err)
	}

	// Re-setting the cron expression must not forget the last firing.
	if err := SetSchedule(s.DB(), "immich", "0 4 * * *"); err != nil {
		t.Fatalf("SetSchedule: %v", err)
	}
	if got, _ := LastFired(s.DB(), "immich"); !got.Equal(at) {
		t.Errorf("LastFired after SetSchedule = %v, want it kept at %v", got, at)
	}
}

func TestLastFiredRejectsCorruptTimestamp(t *testing.T) {
	s := openTestDB(t)
	if err := SetSchedule(s.DB(), "immich", "0 3 * * *"); err != nil {
		t.Fatalf("SetSchedule: %v", err)
	}
	if _, err := s.DB().Exec(`UPDATE schedules SET last_fired_at = 'garbage'`); err != nil {
		t.Fatalf("corrupting last_fired_at: %v", err)
	}
	if _, err := LastFired(s.DB(), "immich"); err == nil {
		t.Error("LastFired must reject an unparseable timestamp")
	}
}

func TestScheduleBaselineCountsManualRuns(t *testing.T) {
	s := openTestDB(t)
	db := s.DB()
	if err := SetSchedule(db, "immich", "0 3 * * *"); err != nil {
		t.Fatal(err)
	}
	sched, _ := ParseCron("0 3 * * *")
	now := time.Date(2026, 3, 5, 12, 0, 0, 0, time.UTC)

	// Never backed up at all: the only always-due case.
	if b, _ := ScheduleBaseline(db, "immich"); !b.IsZero() || !sched.IsDue(now, b) {
		t.Fatalf("baseline = %v, want zero and due", b)
	}
	ins := func(state string, fin time.Time) {
		t.Helper()
		db.Exec(`INSERT OR IGNORE INTO jobs (service, created_at) VALUES ('immich', ?)`, fin.Format(time.RFC3339Nano))
		if _, err := db.Exec(`INSERT INTO runs (job_id, state, started_at, finished_at)
			SELECT id, ?, ?, ? FROM jobs WHERE service = 'immich'`, state, fin.Format(time.RFC3339Nano), fin.Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
	ins("FAILED", now.Add(-time.Hour))
	if b, _ := ScheduleBaseline(db, "immich"); !b.IsZero() {
		t.Errorf("baseline = %v after only a failed run, want zero", b)
	}
	manual := now.Add(-2 * time.Hour)
	ins("SUCCESS", manual)
	b, err := ScheduleBaseline(db, "immich")
	if err != nil || !b.Equal(manual) {
		t.Fatalf("baseline = %v, %v; want %v", b, err, manual)
	}
	if sched.IsDue(now, b) {
		t.Error("schedule due right after a manual backup, want not due until 03:00 tomorrow")
	}
}

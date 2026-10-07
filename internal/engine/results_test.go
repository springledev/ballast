package engine

import (
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

func TestSetSnapshotID(t *testing.T) {
	db := openTestDB(t).DB()
	m, err := StartRun(db, "immich")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if err := m.SetSnapshotID("abc123"); err != nil {
		t.Fatalf("SetSnapshotID: %v", err)
	}
	if m.Run().SnapshotID != "abc123" {
		t.Errorf("Run().SnapshotID = %q, want abc123", m.Run().SnapshotID)
	}

	loaded, err := LoadRun(db, m.Run().ID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if loaded.Run().SnapshotID != "abc123" {
		t.Errorf("LoadRun SnapshotID = %q, want abc123", loaded.Run().SnapshotID)
	}
}

func TestRecordVerificationAndResults(t *testing.T) {
	db := openTestDB(t).DB()
	m, err := StartRun(db, "immich")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	rec := VerificationRecord{
		Method:   "archive-integrity",
		Passed:   true,
		Detail:   "ok",
		Output:   "checked",
		Duration: 2 * time.Second,
	}
	if err := m.RecordVerification(rec); err != nil {
		t.Fatalf("RecordVerification: %v", err)
	}
	rec2 := VerificationRecord{Method: "restic-read-back", Passed: false, Warning: "amber"}
	if err := m.RecordVerification(rec2); err != nil {
		t.Fatalf("RecordVerification 2: %v", err)
	}

	results, err := VerificationResults(db, m.Run().ID)
	if err != nil {
		t.Fatalf("VerificationResults: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(results))
	}
	if results[0].Method != "archive-integrity" || !results[0].Passed || results[0].Duration != 2*time.Second {
		t.Errorf("results[0] = %+v, want archive-integrity passed 2s", results[0])
	}
	if results[1].Method != "restic-read-back" || results[1].Passed || results[1].Warning != "amber" {
		t.Errorf("results[1] = %+v, want restic-read-back failed with warning", results[1])
	}
}

func TestVerificationResultsEmpty(t *testing.T) {
	db := openTestDB(t).DB()
	results, err := VerificationResults(db, 999)
	if err != nil {
		t.Fatalf("VerificationResults: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("results = %v, want none for a nonexistent run", results)
	}
}

func TestRecentRuns(t *testing.T) {
	db := openTestDB(t).DB()
	var ids []int64
	for i := 0; i < 3; i++ {
		m, err := StartRun(db, "immich")
		if err != nil {
			t.Fatalf("StartRun: %v", err)
		}
		ids = append(ids, m.Run().ID)
	}

	runs, err := RecentRuns(db, "immich", 2)
	if err != nil {
		t.Fatalf("RecentRuns: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("len(runs) = %d, want 2 (limit applied)", len(runs))
	}
	// Newest first.
	if runs[0].ID != ids[2] || runs[1].ID != ids[1] {
		t.Errorf("runs = %+v, want newest first [%d, %d]", runs, ids[2], ids[1])
	}
}

func TestRecentRunsUnknownService(t *testing.T) {
	db := openTestDB(t).DB()
	runs, err := RecentRuns(db, "nope", 5)
	if err != nil {
		t.Fatalf("RecentRuns: %v", err)
	}
	if len(runs) != 0 {
		t.Errorf("runs = %v, want none", runs)
	}
}

func TestRequireVerifications(t *testing.T) {
	type rec struct {
		method string
		passed bool
	}
	cases := []struct {
		name     string
		required []string
		recs     []rec
		wantErr  string // "" = SUCCESS allowed
	}{
		{name: "no requirements", recs: []rec{{"archive-integrity", true}}},
		{name: "all required passed", required: []string{"archive-integrity", "restart-smoke"}, recs: []rec{{"archive-integrity", true}, {"restart-smoke", true}, {"restic-read-back", true}}},
		{name: "required method never ran", required: []string{"archive-integrity", "restart-smoke"}, recs: []rec{{"archive-integrity", true}}, wantErr: `"restart-smoke" never ran`},
		{name: "required method failed", required: []string{"restart-smoke"}, recs: []rec{{"restart-smoke", false}}, wantErr: `"restart-smoke" did not pass`},
		{name: "one of several results failed", required: []string{"db-restore-drill"}, recs: []rec{{"db-restore-drill", true}, {"db-restore-drill", false}}, wantErr: "did not pass"},
		{name: "nothing recorded", required: []string{"archive-integrity"}, wantErr: "never ran"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := openTestDB(t).DB()
			m, err := StartRun(db, "vaultwarden")
			if err != nil {
				t.Fatalf("StartRun: %v", err)
			}
			m.RequireVerifications(tc.required...)
			for _, s := range []State{PREPARING, DUMPING, UPLOADING, VERIFYING} {
				if err := m.Transition(s, string(s)); err != nil {
					t.Fatalf("Transition(%s): %v", s, err)
				}
			}
			for _, r := range tc.recs {
				if err := m.RecordVerification(VerificationRecord{Method: r.method, Passed: r.passed}); err != nil {
					t.Fatalf("RecordVerification: %v", err)
				}
			}
			err = m.Transition(SUCCESS, "backup verified")
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Transition(SUCCESS): %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Transition(SUCCESS) = %v, want error containing %q", err, tc.wantErr)
			}
			if m.Run().State != VERIFYING {
				t.Errorf("state after refused SUCCESS = %s, want VERIFYING", m.Run().State)
			}
			// A refused SUCCESS must still leave FAILED reachable.
			if err := m.Transition(FAILED, "verification incomplete"); err != nil {
				t.Errorf("Transition(FAILED) after refusal: %v", err)
			}
		})
	}
}

func TestRecordTargetAndRunTargets(t *testing.T) {
	db := openTestDB(t).DB()
	m, err := StartRun(db, "immich")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []TargetResult{
		{Destination: "ssd", State: SUCCESS, SnapshotID: "aaa", Detail: "backup verified"},
		{Destination: "b2", State: FAILED, Detail: "unreachable"},
	} {
		if err := m.RecordTarget(r); err != nil {
			t.Fatal(err)
		}
	}
	// A second record for the same destination replaces the first.
	if err := m.RecordTarget(TargetResult{Destination: "b2", State: SUCCESS_UNVERIFIED, SnapshotID: "bbb"}); err != nil {
		t.Fatal(err)
	}
	got, err := RunTargets(db, m.Run().ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Destination != "ssd" || got[1].State != SUCCESS_UNVERIFIED || got[1].SnapshotID != "bbb" {
		t.Errorf("RunTargets = %+v", got)
	}
	if err := m.RecordVerification(VerificationRecord{Target: "b2", Method: "restic-read-back", Passed: true}); err != nil {
		t.Fatal(err)
	}
	recs, _ := VerificationResults(db, m.Run().ID)
	if len(recs) != 1 || recs[0].Target != "b2" {
		t.Errorf("verification target = %+v", recs)
	}
}

func TestRecordTargetKeepsTheTier(t *testing.T) {
	db := openTestDB(t).DB()
	m, err := StartRun(db, "vault")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []TargetResult{
		{Destination: "ssd", State: SUCCESS},
		{Destination: "proton", State: SUCCESS_UNVERIFIED, Detail: "sign in again", Secondary: true},
	} {
		if err := m.RecordTarget(r); err != nil {
			t.Fatal(err)
		}
	}
	got, err := RunTargets(db, m.Run().ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Secondary || !got[1].Secondary || got[1].Detail != "sign in again" {
		t.Errorf("RunTargets = %+v, want ssd primary and proton secondary", got)
	}
}

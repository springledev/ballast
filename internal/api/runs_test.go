package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/springledev/ballast/internal/engine"
	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/store"
	secretpkg "github.com/springledev/keel/secret"
)

// seedRun records one finished run for service and returns its ID.
func seedRun(t *testing.T, h *harness, service string, final engine.State) int64 {
	t.Helper()
	m, err := engine.StartRun(h.ops.DB, service)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	steps := []engine.State{engine.PREPARING, engine.DUMPING, engine.UPLOADING, engine.VERIFYING, final}
	if final == engine.FAILED {
		steps = []engine.State{engine.PREPARING, engine.FAILED}
	}
	for _, s := range steps {
		if err := m.Transition(s, "step "+string(s)); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	return m.Run().ID
}

func TestOpsListRuns(t *testing.T) {
	h := newHarness(t)
	withService(t, h)
	var ids []int64 // oldest first
	for i := 0; i < 5; i++ {
		ids = append(ids, seedRun(t, h, "svc", engine.SUCCESS))
	}
	tests := []struct {
		name       string
		limit      int
		before     int64
		wantIDs    []int64
		wantNext   int64
		wantErrIs  error
		service    string
		wantNoRuns bool
	}{
		{name: "default newest first", wantIDs: []int64{ids[4], ids[3], ids[2], ids[1], ids[0]}},
		{name: "limit 2 has cursor", limit: 2, wantIDs: []int64{ids[4], ids[3]}, wantNext: ids[3]},
		{name: "before cursor", limit: 2, before: ids[3], wantIDs: []int64{ids[2], ids[1]}, wantNext: ids[1]},
		{name: "last page no cursor", limit: 2, before: ids[1], wantIDs: []int64{ids[0]}},
		{name: "huge limit capped", limit: 5000, wantIDs: []int64{ids[4], ids[3], ids[2], ids[1], ids[0]}},
		{name: "before oldest is empty", before: ids[0], wantNoRuns: true},
		{name: "negative limit", limit: -1, wantErrIs: ErrValidation},
		{name: "unknown service", service: "nope", wantErrIs: inventory.ErrNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := tc.service
			if svc == "" {
				svc = "svc"
			}
			page, err := h.ops.ListRuns(svc, tc.limit, tc.before)
			if tc.wantErrIs != nil {
				if !errors.Is(err, tc.wantErrIs) {
					t.Fatalf("err = %v, want %v", err, tc.wantErrIs)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if page.Runs == nil {
				t.Fatal("Runs must be an empty slice, not nil")
			}
			var got []int64
			for _, r := range page.Runs {
				got = append(got, r.ID)
			}
			if len(got) != len(tc.wantIDs) {
				t.Fatalf("ids = %v, want %v", got, tc.wantIDs)
			}
			for i := range got {
				if got[i] != tc.wantIDs[i] {
					t.Fatalf("ids = %v, want %v", got, tc.wantIDs)
				}
			}
			if page.NextBefore != tc.wantNext {
				t.Errorf("NextBefore = %d, want %d", page.NextBefore, tc.wantNext)
			}
		})
	}
}

func TestOpsListRunsRespectsHistoryFloor(t *testing.T) {
	h := newHarness(t)
	withService(t, h)
	old := seedRun(t, h, "svc", engine.SUCCESS)
	keep := seedRun(t, h, "svc", engine.SUCCESS)
	if _, err := h.ops.DB.Exec(`INSERT INTO meta (key, value) VALUES (?, ?)`, store.HistoryFloorKeyPrefix+"svc", old); err != nil {
		t.Fatal(err)
	}
	page, err := h.ops.ListRuns("svc", 0, 0)
	if err != nil || len(page.Runs) != 1 || page.Runs[0].ID != keep {
		t.Fatalf("page = %+v, err = %v; want only run %d", page, err, keep)
	}
	if _, err := h.ops.GetRun(old); !errors.Is(err, inventory.ErrNotFound) {
		t.Errorf("GetRun(hidden) err = %v, want not found", err)
	}
}

func TestOpsRunTargetsStaySeparate(t *testing.T) {
	h := newHarness(t)
	withService(t, h)
	m, err := engine.StartRun(h.ops.DB, "svc")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []engine.State{engine.PREPARING, engine.DUMPING, engine.UPLOADING, engine.VERIFYING, engine.SUCCESS_UNVERIFIED} {
		if err := m.Transition(s, "x"); err != nil {
			t.Fatal(err)
		}
	}
	for _, tr := range []engine.TargetResult{
		{Destination: "local", State: engine.SUCCESS, SnapshotID: "abc"},
		{Destination: "b2", State: engine.FAILED, Detail: "upload failed"},
	} {
		if err := m.RecordTarget(tr); err != nil {
			t.Fatal(err)
		}
	}
	d, err := h.ops.GetRun(m.Run().ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.State != "SUCCESS_UNVERIFIED" || d.Light != "amber" {
		t.Errorf("state/light = %s/%s, want stored state and amber", d.State, d.Light)
	}
	if len(d.Targets) != 2 || d.Targets[0].Light != "green" || d.Targets[1].Light != "red" || d.Targets[1].Detail != "upload failed" {
		t.Errorf("targets = %+v", d.Targets)
	}
}

func TestOpsGetRunLogTailCap(t *testing.T) {
	h := newHarness(t)
	withService(t, h)
	m, err := engine.StartRun(h.ops.DB, "svc")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Transition(engine.PREPARING, "start"); err != nil {
		t.Fatal(err)
	}
	// Pad the log directly so we do not need hundreds of legal transitions.
	for i := 0; i < 300; i++ {
		if _, err := h.ops.DB.Exec(`INSERT INTO run_transitions (run_id, from_state, to_state, at, detail) VALUES (?, 'PREPARING', 'PREPARING', '2026-01-01T00:00:00Z', ?)`, m.Run().ID, strings.Repeat("x", 1000)+string(rune('0'+i%10))); err != nil {
			t.Fatal(err)
		}
	}
	d, err := h.ops.GetRun(m.Run().ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.LogTail) > MaxLogLines || !d.LogTruncated {
		t.Fatalf("lines = %d truncated = %v", len(d.LogTail), d.LogTruncated)
	}
	total := 0
	for _, l := range d.LogTail {
		total += len(l.Text) + 1
	}
	if total > MaxLogBytes {
		t.Errorf("log tail is %d bytes, cap %d", total, MaxLogBytes)
	}
	// Newest lines are kept: the last one is the final inserted row.
	if !strings.HasSuffix(d.LogTail[len(d.LogTail)-1].Text, "x9") {
		t.Errorf("tail does not end with the newest line")
	}
}

func TestOpsGetRunUnknown(t *testing.T) {
	h := newHarness(t)
	if _, err := h.ops.GetRun(999); !errors.Is(err, inventory.ErrNotFound) {
		t.Fatalf("err = %v, want not found", err)
	}
}

// TestRunHistoryNeverReturnsSeededSecret: a secret placed in a run log,
// a verification detail/output and a target detail does not come back
// from either route (rule 2).
func TestRunHistoryNeverReturnsSeededSecret(t *testing.T) {
	const secret = "s3cr3t-token-xyz-12345"
	h := newHarness(t)
	withService(t, h)
	h.ops.Redact = secretpkg.NewSet()
	h.ops.Redact.Add(secret)

	m, err := engine.StartRun(h.ops.DB, "svc")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Transition(engine.PREPARING, "using token "+secret); err != nil {
		t.Fatal(err)
	}
	if err := m.Transition(engine.FAILED, "login failed for "+secret); err != nil {
		t.Fatal(err)
	}
	if err := m.RecordVerification(engine.VerificationRecord{Method: "restic-read-back", Warning: "w " + secret, Detail: "d " + secret, Output: "o " + secret}); err != nil {
		t.Fatal(err)
	}
	if err := m.RecordTarget(engine.TargetResult{Destination: "local", State: engine.FAILED, Detail: "t " + secret}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ops.DB.Exec(`UPDATE runs SET error = ? WHERE id = ?`, "err "+secret, m.Run().ID); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v1/services/svc/runs", "/api/v1/runs/1"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+testAPIToken)
		NewHandler(h.ops, testAPIToken).ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
		}
		if strings.Contains(rec.Body.String(), secret) {
			t.Errorf("%s leaked the secret: %s", path, rec.Body)
		}
		if !strings.Contains(rec.Body.String(), "[redacted]") {
			t.Errorf("%s: expected redaction marker in %s", path, rec.Body)
		}
		var v any
		if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
	}
}

func TestListRunsQueryUsesIndex(t *testing.T) {
	h := newHarness(t)
	withService(t, h)
	for i := 0; i < 50; i++ {
		seedRun(t, h, "svc", engine.SUCCESS)
	}
	rows, err := h.ops.DB.Query(`EXPLAIN QUERY PLAN SELECT r.id FROM runs r JOIN jobs j ON j.id = r.job_id
		WHERE j.service = ? AND `+store.RunsAfterFloorSQL+` AND r.id < ? ORDER BY r.id DESC LIMIT ?`, "svc", 100, 20)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(detail, "SCAN r") || strings.HasPrefix(detail, "SCAN runs") {
			t.Errorf("full scan of runs: %s", detail)
		}
	}
}

func TestHTTPGoldenRunHistory(t *testing.T) {
	two := func(t *testing.T, h *harness) {
		withService(t, h)
		seedRun(t, h, "svc", engine.SUCCESS)
		seedRun(t, h, "svc", engine.FAILED)
	}
	get := func(path string) func(*harness) *http.Request {
		return func(h *harness) *http.Request { return httptest.NewRequest(http.MethodGet, path, nil) }
	}
	for _, tc := range []goldenCase{
		{name: "runs_list", arrange: two, req: get("/api/v1/services/svc/runs")},
		{name: "runs_list_paged", arrange: two, req: get("/api/v1/services/svc/runs?limit=1")},
		{name: "runs_list_empty", arrange: withService, req: get("/api/v1/services/svc/runs")},
		{name: "runs_list_unknown_service", req: get("/api/v1/services/nope/runs")},
		{name: "runs_list_bad_limit", arrange: withService, req: get("/api/v1/services/svc/runs?limit=abc")},
		{name: "runs_get", arrange: two, req: get("/api/v1/runs/2")},
		{name: "runs_get_unknown", req: get("/api/v1/runs/99")},
	} {
		runGolden(t, tc)
	}
}

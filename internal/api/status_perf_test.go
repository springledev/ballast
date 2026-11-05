package api

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"
	"time"

	"github.com/springledev/ballast/internal/engine"
	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/scheduler"
	"github.com/springledev/ballast/internal/store"
)

// Status performance fixture (spec §22.3, K5): 60 services over two
// destinations, about 3,200 runs whose ids follow time order and
// interleave across services, the way a real nightly history grows.

const (
	perfServices = 60
	perfDays     = 54 // 58 services with runs x ~54 nights ~= 3,200 runs
)

// perfKind says how a fixture service's newest run ended.
type perfKind int

const (
	perfGreen perfKind = iota
	perfAmber
	perfRed
	perfStale
	perfNever
)

// perfKindFor spreads the end states: 40 green, 8 amber, 7 red, 3
// stale, 2 never run.
func perfKindFor(i int) perfKind {
	switch {
	case i < 40:
		return perfGreen
	case i < 48:
		return perfAmber
	case i < 55:
		return perfRed
	case i < 58:
		return perfStale
	default:
		return perfNever
	}
}

// newStatusPerfFixture builds the fixture in a fresh store and returns
// Ops over it. Run timestamps are relative to the wall clock (status
// ages use it), anchored on the hour so they stay well inside or well
// outside each freshness window.
func newStatusPerfFixture(tb testing.TB) *Ops {
	tb.Helper()
	dir := tb.TempDir()
	st, err := store.Open(filepath.Join(dir, "ballast.db"))
	if err != nil {
		tb.Fatalf("opening store: %v", err)
	}
	tb.Cleanup(func() { st.Close() })
	db := st.DB()
	credDir := filepath.Join(dir, "creds")
	if err := os.MkdirAll(credDir, 0o700); err != nil {
		tb.Fatal(err)
	}

	for _, d := range []string{"nas", "b2"} {
		if err := inventory.PutDestination(db, inventory.Destination{
			Name: d, Location: "/stub-repo-" + d, KeepDaily: 7, KeepWeekly: 4, KeepMonthly: 6,
		}); err != nil {
			tb.Fatalf("PutDestination %s: %v", d, err)
		}
	}
	names := make([]string, perfServices)
	for i := range names {
		names[i] = fmt.Sprintf("svc-%02d", i)
		svc := inventory.Service{
			Name: names[i], CatalogEntry: "folder", Destination: "nas",
			Params: map[string]string{"path": "/srv/" + names[i]},
		}
		if i%2 == 0 { // half of them go to both destinations
			svc.Targets = []inventory.Target{{Destination: "nas"}, {Destination: "b2"}}
		}
		if err := inventory.PutService(db, svc); err != nil {
			tb.Fatalf("PutService %s: %v", names[i], err)
		}
		if err := scheduler.SetSchedule(db, names[i], fmt.Sprintf("%d 3 * * *", i%60)); err != nil {
			tb.Fatalf("SetSchedule %s: %v", names[i], err)
		}
	}

	base := time.Now().UTC().Truncate(time.Hour)
	tx, err := db.Begin()
	if err != nil {
		tb.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	jobs := map[string]int64{}
	for i, n := range names {
		if perfKindFor(i) == perfNever {
			continue
		}
		res, err := tx.Exec(`INSERT INTO jobs (service, created_at) VALUES (?, ?)`, n, base.Add(-perfDays*24*time.Hour).Format(time.RFC3339Nano))
		if err != nil {
			tb.Fatal(err)
		}
		jobs[n], _ = res.LastInsertId()
	}
	runs := 0
	for day := perfDays; day >= 1; day-- {
		// A deterministic, day-dependent order so ids interleave.
		order := append([]string(nil), names...)
		sort.Slice(order, func(a, b int) bool {
			ha, hb := (int(order[a][4])*31+int(order[a][5])*7+day*13)%61, (int(order[b][4])*31+int(order[b][5])*7+day*13)%61
			if ha != hb {
				return ha < hb
			}
			return order[a] < order[b]
		})
		for slot, n := range order {
			i := int(n[4]-'0')*10 + int(n[5]-'0')
			kind := perfKindFor(i)
			if kind == perfNever || (kind == perfStale && day < 6) {
				continue // stale services stopped running five days ago
			}
			newest := day == 1 || (kind == perfStale && day == 6)
			started := base.Add(-time.Duration(day)*24*time.Hour + 2*time.Hour + time.Duration(slot)*time.Minute)
			if day == 1 {
				started = base.Add(-3*time.Hour + time.Duration(slot)*time.Second)
			}
			finished := started.Add(90 * time.Second)
			state, errText := engine.SUCCESS, ""
			switch {
			case newest && kind == perfAmber:
				state = engine.SUCCESS_UNVERIFIED
			case newest && kind == perfRed:
				state, errText = engine.FAILED, "restic backup to nas failed: repository is locked"
			case day%17 == 0:
				state, errText = engine.FAILED, "dump failed: connection refused"
			}
			var fin any = finished.Format(time.RFC3339Nano)
			if newest && i == 7 { // one green service is mid-run right now
				state, fin = engine.UPLOADING, nil
			}
			res, err := tx.Exec(`INSERT INTO runs (job_id, state, started_at, finished_at, error, snapshot_id) VALUES (?, ?, ?, ?, ?, ?)`,
				jobs[n], string(state), started.Format(time.RFC3339Nano), fin, nullIfEmpty(errText), fmt.Sprintf("snap%05d", runs))
			if err != nil {
				tb.Fatal(err)
			}
			id, _ := res.LastInsertId()
			runs++
			for k, to := range []engine.State{engine.PENDING, engine.PREPARING, engine.DUMPING, engine.UPLOADING, engine.VERIFYING, state} {
				if _, err := tx.Exec(`INSERT INTO run_transitions (run_id, from_state, to_state, at, duration_ms, detail) VALUES (?, ?, ?, ?, ?, ?)`,
					id, string(engine.PENDING), string(to), started.Add(time.Duration(k)*10*time.Second).Format(time.RFC3339Nano), 10000, "step"); err != nil {
					tb.Fatal(err)
				}
			}
			if fin == nil {
				continue
			}
			dests := []string{"nas"}
			if i%2 == 0 {
				dests = append(dests, "b2")
			}
			for di, d := range dests {
				tstate, detail := state, ""
				if state == engine.FAILED {
					detail = errText
				}
				// One two-target service: b2 failed while nas succeeded.
				if newest && i == 2 && di == 1 {
					tstate, detail = engine.FAILED, "b2: upload failed: 503 service unavailable"
				}
				if _, err := tx.Exec(`INSERT INTO run_targets (run_id, destination, state, snapshot_id, detail) VALUES (?, ?, ?, ?, ?)`,
					id, d, string(tstate), fmt.Sprintf("snap%05d-%s", runs, d), detail); err != nil {
					tb.Fatal(err)
				}
				if _, err := tx.Exec(`INSERT INTO verification_results (run_id, method, passed, detail, duration_ms, recorded_at, target) VALUES (?, ?, ?, ?, ?, ?, ?)`,
					id, "restic_check", boolInt(state == engine.SUCCESS), "read back "+d, 1200, finished.Format(time.RFC3339Nano), d); err != nil {
					tb.Fatal(err)
				}
			}
		}
	}
	for i := 0; i < 3; i++ {
		if _, err := tx.Exec(`INSERT INTO alert_decisions (service, at, type, outcome, reason) VALUES (?, ?, ?, ?, ?)`,
			names[50+i], base.Add(-time.Hour).Format(time.RFC3339Nano), "failure", "sent", "the backup failed"); err != nil {
			tb.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		tb.Fatal(err)
	}
	if runs < 3100 || runs > 3300 {
		tb.Fatalf("fixture has %d runs, want about 3,200", runs)
	}
	return &Ops{DB: db, Now: func() time.Time { return base }, CredentialsDir: credDir}
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// statusBody serves GET /api/v1/status through the real router.
func statusBody(tb testing.TB, h http.Handler, query string) []byte {
	tb.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/status"+query, nil)
	req.Header.Set("Authorization", "Bearer "+testAPIToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		tb.Fatalf("GET /api/v1/status%s = %d: %s", query, rec.Code, rec.Body.String())
	}
	return rec.Body.Bytes()
}

func BenchmarkStatus(b *testing.B) {
	ops := newStatusPerfFixture(b)
	h := NewHandler(ops, testAPIToken)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		statusBody(b, h, "")
	}
}

// TestStatusUnder200ms keeps GET /api/v1/status fast (spec §22.3): the
// median of five calls must stay well under the 200 ms target; it fails
// above 400 ms so a slow CI machine does not flake.
func TestStatusUnder200ms(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test skipped in -short mode")
	}
	if raceEnabled {
		t.Skip("timing test skipped under -race: the race detector slows SQLite many times over; BenchmarkStatus and the plain test run check speed")
	}
	ops := newStatusPerfFixture(t)
	h := NewHandler(ops, testAPIToken)
	statusBody(t, h, "") // warm up
	var took []time.Duration
	for i := 0; i < 5; i++ {
		start := time.Now()
		statusBody(t, h, "")
		took = append(took, time.Since(start))
	}
	sort.Slice(took, func(a, b int) bool { return took[a] < took[b] })
	median := took[2]
	t.Logf("GET /api/v1/status median %v (all %v)", median, took)
	if median > 400*time.Millisecond {
		t.Fatalf("GET /api/v1/status took %v (median of 5) with %d services; want under 200 ms", median, perfServices)
	}
}

var (
	perfTimestamp = regexp.MustCompile(`"\d{4}-\d\d-\d\dT[0-9:.]+(Z|[+-]\d\d:\d\d)"`)
	perfAges      = regexp.MustCompile(`"(freshness_age_seconds|verified_age_seconds|elapsed_seconds)":\d+`)
)

// TestStatusBytesStable pins the full status body for the performance
// fixture, with and without verification records, so speeding status
// up can never change what it reports. Only wall-clock values (times,
// ages) are masked. Regenerate with -update.
func TestStatusBytesStable(t *testing.T) {
	ops := newStatusPerfFixture(t)
	h := NewHandler(ops, testAPIToken)
	for _, tc := range []struct{ name, query string }{
		{"status_perf_fixture", ""},
		{"status_perf_fixture_verify", "?verify=1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := statusBody(t, h, tc.query)
			got = perfTimestamp.ReplaceAll(got, []byte(`"<time>"`))
			got = perfAges.ReplaceAll(got, []byte(`"$1":0`))
			path := filepath.Join("testdata", "golden", tc.name+".json")
			if *updateGolden {
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading golden %s: %v (run with -update to create it)", path, err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("status body for %s changed; diff against %s", tc.name, path)
			}
		})
	}
}

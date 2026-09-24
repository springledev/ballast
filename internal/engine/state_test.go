package engine

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/springledev/ballast/internal/store"
)

func openMachineDB(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "ballast.db"))
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// TestHappyPathIsLegal walks the full §4.1 happy path and asserts that
// every recorded step persisted with a timestamp and that the run ends
// SUCCESS (green) only after VERIFYING.
func TestHappyPathIsLegal(t *testing.T) {
	s := openMachineDB(t)
	m, err := StartRun(s.DB(), "immich")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	path := []State{PREPARING, DUMPING, UPLOADING, VERIFYING, SUCCESS}
	for _, to := range path {
		if err := m.Transition(to, ""); err != nil {
			t.Fatalf("transition to %s: %v", to, err)
		}
	}
	r := m.Run()
	if r.State != SUCCESS {
		t.Fatalf("final state = %s, want SUCCESS", r.State)
	}
	if r.FinishedAt == nil {
		t.Error("terminal run must have finished_at set")
	}

	db := s.DB()
	var count int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM run_transitions WHERE run_id = ?`, r.ID,
	).Scan(&count); err != nil {
		t.Fatalf("counting transitions: %v", err)
	}
	// Initial PENDING record + 5 transitions.
	if count != 6 {
		t.Errorf("transition records = %d, want 6", count)
	}

	var started, finished string
	if err := db.QueryRow(
		`SELECT started_at, finished_at FROM runs WHERE id = ?`, r.ID,
	).Scan(&started, &finished); err != nil {
		t.Fatalf("reading run: %v", err)
	}
	if _, err := time.Parse(time.RFC3339, finished); err != nil {
		t.Errorf("finished_at not a timestamp: %v", err)
	}
	if _, err := time.Parse(time.RFC3339, started); err != nil {
		t.Errorf("started_at not a timestamp: %v", err)
	}
	var firstTransition string
	if err := db.QueryRow(
		`SELECT at FROM run_transitions WHERE run_id = ? ORDER BY id LIMIT 1`, r.ID,
	).Scan(&firstTransition); err != nil {
		t.Fatalf("reading initial transition: %v", err)
	}
	if firstTransition != started {
		t.Errorf("initial transition at = %q, want run started_at %q", firstTransition, started)
	}
}

// TestLegalTransitions is the independent positive pin for the state
// machine (spec §4.1). It lists the exact expected successor states for
// every from-state, so a wrong adjacency table cannot hide behind the
// implementation's own helper.
func TestLegalTransitions(t *testing.T) {
	cases := []struct {
		from State
		want []State
	}{
		{PENDING, []State{PREPARING, FAILED, CANCELLED}},
		{PREPARING, []State{DUMPING, FAILED, CANCELLED}},
		{DUMPING, []State{UPLOADING, FAILED, CANCELLED}},
		{UPLOADING, []State{VERIFYING, FAILED, CANCELLED}},
		{VERIFYING, []State{SUCCESS, SUCCESS_UNVERIFIED, FAILED, CANCELLED}},
		{SUCCESS, nil},
		{SUCCESS_UNVERIFIED, nil},
		{FAILED, nil},
		{CANCELLED, nil},
	}
	for _, tc := range cases {
		t.Run(tc.from.String(), func(t *testing.T) {
			got := NextStates(tc.from)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("NextStates(%s) = %v, want %v", tc.from, got, tc.want)
			}
		})
	}
}

// TestEveryTransitionPair is an exhaustive consistency check: for each
// pair we construct the from-state and call Transition, comparing the
// result to the separately-pinned adjacency table.
func TestEveryTransitionPair(t *testing.T) {
	all := AllStates()
	var allPairs []struct {
		from, to State
		legal    bool
	}
	for _, from := range all {
		for _, to := range all {
			allPairs = append(allPairs, struct {
				from, to State
				legal    bool
			}{from, to, CanTransition(from, to)})
		}
	}

	for _, tc := range allPairs {
		name := tc.from.String() + "->" + tc.to.String()
		m, _, err := startRunAt(t, tc.from)
		if err != nil {
			t.Fatalf("cannot establish state %s for pair %s: %v", tc.from, name, err)
		}
		err = m.Transition(tc.to, "detail")
		if tc.legal && err != nil {
			t.Errorf("%s: expected legal, got error %v", name, err)
		}
		if !tc.legal && err == nil {
			t.Errorf("%s: expected illegal transition to be rejected", name)
		}
	}
}

// TestIllegalPairsExplicit pins the most important illegal pairs by
// name so failures read as requirements, not arithmetic: state
// skipping, SUCCESS without VERIFYING, and terminal states.
func TestIllegalPairsExplicit(t *testing.T) {
	cases := []struct {
		name string
		from State
		to   State
	}{
		{"cannot skip wait to dumping", PENDING, DUMPING},
		{"cannot skip waiting to uploading", PENDING, UPLOADING},
		{"cannot go verifying without upload", PREPARING, VERIFYING},
		{"cannot back up to dumping", DUMPING, PREPARING},
		{"success requires verifying", UPLOADING, SUCCESS},
		{"success requires verifying (any other state)", PREPARING, SUCCESS},
		{"terminal success cannot fail again", SUCCESS, FAILED},
		{"terminal failure cannot cancel", FAILED, CANCELLED},
		{"terminal cancelled cannot prepare", CANCELLED, PREPARING},
		{"terminal success unverified cannot succeed", SUCCESS_UNVERIFIED, SUCCESS},
		{"verifying cannot go backwards to uploading", VERIFYING, UPLOADING},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if CanTransition(tc.from, tc.to) {
				t.Errorf("CanTransition says %s->%s is legal, but it is a spec violation", tc.from, tc.to)
			}
		})
	}
}

// TestUnverifiableRunIsAmber asserts spec §4.1: when verification is
// impossible the run lands SUCCESS_UNVERIFIED (amber), never green
// SUCCESS, and never FAILED (the backup itself succeeded).
func TestUnverifiableRunIsAmber(t *testing.T) {
	m, _, _ := startRunAt(t, VERIFYING)
	if err := m.Transition(SUCCESS_UNVERIFIED, "destination temporarily unreadable"); err != nil {
		t.Fatalf("SUCCESS_UNVERIFIED rejected: %v", err)
	}
	if !m.Run().State.Amber() {
		t.Errorf("state %s: unverifiable run must be amber", m.Run().State)
	}
	if m.Run().State == SUCCESS {
		t.Error("unverifiable run must never be green")
	}
}

// TestSuccessRequiresVerifyingState asserts the headline rule of
// §4.1: no code path may go SUCCESS without having gone VERIFYING.
func TestSuccessRequiresVerifyingState(t *testing.T) {
	for _, s := range AllStates() {
		if s != VERIFYING && !s.Terminal() && CanTransition(s, SUCCESS) {
			t.Errorf("SUCCESS legal from %s; only VERIFYING may produce green", s)
		}
	}
}

// TestCancelFromEveryActiveState asserts CANCELLED is reachable from
// every non-terminal state.
func TestCancelFromEveryActiveState(t *testing.T) {
	for _, from := range AllStates() {
		if from.Terminal() {
			continue
		}
		if !CanTransition(from, CANCELLED) {
			t.Errorf("CANCELLED must be legal from %s", from)
		}
	}
}

// TestFailedDetailRequired: a FAILED or CANCELLED transition must
// carry a reason (any state), so no bad outcome is silent.
func TestFailedDetailRequired(t *testing.T) {
	for _, from := range []State{PENDING, PREPARING, DUMPING, UPLOADING, VERIFYING} {
		m, _, err := startRunAt(t, from)
		if err != nil {
			t.Fatalf("setup from %s: %v", from, err)
		}
		if err := m.Transition(FAILED, ""); err == nil {
			t.Errorf("FAILED from %s without detail must be rejected", from)
		}
		m2, _, _ := startRunAt(t, from)
		if err := m2.Transition(CANCELLED, ""); err == nil {
			t.Errorf("CANCELLED from %s without detail must be rejected", from)
		}
	}
}

// TestLoadRunRoundTrip: a machine built for an in-progress run can be
// reloaded from the DB and its state carries over.
func TestLoadRunRoundTrip(t *testing.T) {
	m, s, _ := startRunAt(t, UPLOADING)
	loaded, err := LoadRun(s.DB(), m.Run().ID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if loaded.Run().State != UPLOADING || loaded.Run().Service != "myfix" {
		t.Errorf("loaded run state/service = %s/%s, want UPLOADING/myfix",
			loaded.Run().State, loaded.Run().Service)
	}

	// A persisted illegal state (hand-corrupted) must be rejected.
	fresh := openMachineDB(t)
	if _, err := fresh.DB().Exec(
		`INSERT INTO jobs (service, created_at) VALUES ('x', '2024-01-01T00:00:00Z')`,
	); err != nil {
		t.Fatalf("inserting job: %v", err)
	}
	if _, err := fresh.DB().Exec(
		`INSERT INTO runs (job_id, state, started_at) VALUES (1, 'BOGUS', '2024-01-01T00:00:00Z')`,
	); err != nil {
		t.Fatalf("inserting corrupt run: %v", err)
	}
	var runID int64
	if err := fresh.DB().QueryRow(`SELECT id FROM runs ORDER BY id DESC LIMIT 1`).Scan(&runID); err != nil {
		t.Fatalf("finding run: %v", err)
	}
	if _, err := LoadRun(fresh.DB(), runID); err == nil {
		t.Error("LoadRun must reject an unrecognized stored state")
	}
}

// TestStaleMachineRejected: a machine holding an older snapshot must
// not overwrite newer DB state; it fails loudly instead.
func TestStaleMachineRejected(t *testing.T) {
	m, s, _ := startRunAt(t, PENDING)
	// Another actor advances the run.
	other := m.Run()
	stranger, err := LoadRun(s.DB(), other.ID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if err := stranger.Transition(PREPARING, ""); err != nil {
		t.Fatalf("stranger transition: %v", err)
	}
	// Machine m still believes PENDING: this transition must fail.
	if err := m.Transition(PREPARING, ""); err == nil {
		t.Error("stale machine transition must fail loudly, not overwrite")
	} else if !strings.Contains(err.Error(), "myfix") {
		t.Errorf("stale transition error = %q, want service context", err)
	}
}

// TestHistoryRecorded: each transition is persisted with from/to and
// its structured detail line.
func TestHistoryRecorded(t *testing.T) {
	s := openMachineDB(t)
	m, err := StartRun(s.DB(), "gitea")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	for _, to := range []State{PREPARING, DUMPING} {
		if err := m.Transition(to, ""); err != nil {
			t.Fatalf("%s: %v", to, err)
		}
	}
	if err := m.Transition(FAILED, "pg_dumpall exited 2"); err != nil {
		t.Fatalf("FAILED: %v", err)
	}

	rows, err := s.DB().Query(
		`SELECT from_state, to_state, detail FROM run_transitions WHERE run_id = ? ORDER BY id`, m.Run().ID)
	if err != nil {
		t.Fatalf("reading history: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var from, to, detail string
		if err := rows.Scan(&from, &to, &detail); err != nil {
			t.Fatalf("scanning: %v", err)
		}
		got = append(got, from+"->"+to+" "+detail)
	}
	if len(got) != 4 {
		t.Fatalf("history length = %d, want 4", len(got))
	}
	if !strings.Contains(got[3], "DUMPING->FAILED pg_dumpall exited 2") {
		t.Errorf("final history row = %q, want detail preserved", got[3])
	}
}

// startRunAt drives a placeholder run through legal transitions until
// it holds target (used to build machines at precise architecture
// points). It returns the machine and the store it lives in. Terminal
// targets are constructed by walking to VERIFYING and finishing the
// run, so every one of the 9 states is reachable in tests.
func startRunAt(t *testing.T, target State) (*Machine, *store.Store, error) {
	t.Helper()
	s := openMachineDB(t)
	m, err := StartRun(s.DB(), "myfix")
	if err != nil {
		return nil, nil, err
	}
	if target == PENDING {
		return m, s, nil
	}
	steps := []State{PREPARING, DUMPING, UPLOADING, VERIFYING}
	for _, step := range steps {
		if err := m.Transition(step, ""); err != nil {
			return nil, nil, fmt.Errorf("walking to %s: %w", target, err)
		}
		if step == target {
			return m, s, nil
		}
	}
	// Remaining targets are terminal: finish the run at VERIFYING.
	if target.Terminal() {
		if err := m.Transition(target, "detail"); err != nil {
			return nil, nil, fmt.Errorf("finishing at %s: %w", target, err)
		}
		return m, s, nil
	}
	return nil, nil, fmt.Errorf("cannot construct a machine holding state %s via pipeline adjacency", target)
}

// TestTransitionsRecordDuration asserts spec §4.1's normative record
// contents: each transition carries timestamp, duration, and detail.
func TestTransitionsRecordDuration(t *testing.T) {
	s := openMachineDB(t)
	m, err := StartRun(s.DB(), "duration-test")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if err := m.Transition(PREPARING, ""); err != nil {
		t.Fatalf("PREPARING: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := m.Transition(DUMPING, ""); err != nil {
		t.Fatalf("DUMPING: %v", err)
	}

	type row struct {
		From, To   string
		DurationMs int64
		Detail     string
	}
	var rows []row
	r, err := s.DB().Query(
		`SELECT from_state, to_state, duration_ms, detail FROM run_transitions WHERE run_id = ? ORDER BY id`,
		m.Run().ID,
	)
	if err != nil {
		t.Fatalf("reading transitions: %v", err)
	}
	defer r.Close()
	for r.Next() {
		var it row
		if err := r.Scan(&it.From, &it.To, &it.DurationMs, &it.Detail); err != nil {
			t.Fatalf("scanning: %v", err)
		}
		rows = append(rows, it)
	}
	if len(rows) != 3 {
		t.Fatalf("transition rows = %d, want 3", len(rows))
	}
	if rows[2].From != "PREPARING" || rows[2].To != "DUMPING" {
		t.Fatalf("row 3 = %s->%s, want PREPARING->DUMPING", rows[2].From, rows[2].To)
	}
	if rows[2].DurationMs < 5 || rows[2].DurationMs > 10000 {
		t.Errorf("PREPARING duration = %dms, want >=5 (slept 10ms)", rows[2].DurationMs)
	}
}

// TestLoadRunRejectsCorruptFinishedAt: fail loudly on unparseable
// persisted timestamps instead of silently ignoring them.
func TestLoadRunRejectsCorruptFinishedAt(t *testing.T) {
	m, s, _ := startRunAt(t, SUCCESS)
	if _, err := s.DB().Exec(
		`UPDATE runs SET finished_at = 'not-a-time' WHERE id = ?`, m.Run().ID,
	); err != nil {
		t.Fatalf("corrupting finished_at: %v", err)
	}
	if _, err := LoadRun(s.DB(), m.Run().ID); err == nil {
		t.Error("LoadRun must reject an unparseable finished_at")
	}
}

// TestStartRunEmptyService rejects the nonsense case before touching
// the database.
func TestStartRunEmptyService(t *testing.T) {
	s := openMachineDB(t)
	if _, err := StartRun(s.DB(), ""); err == nil {
		t.Error("StartRun with empty service must fail")
	}
}

// TestLoadRunMissing: asking for a non-existent run is a loud error.
func TestLoadRunMissing(t *testing.T) {
	s := openMachineDB(t)
	_, err := LoadRun(s.DB(), 9999)
	if err == nil {
		t.Error("LoadRun for missing run must fail")
	}
}

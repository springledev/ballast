package pipeline

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/springledev/ballast/internal/backend"
	"github.com/springledev/ballast/internal/engine"
	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/restic"
)

// forgetRemovingOne is `restic forget --json` output with one kept and
// one removed snapshot.
const forgetRemovingOne = `[{"tags":["ballast","ballast-service:folder"],"host":"","paths":null,` +
	`"keep":[{"time":"2024-01-02T00:00:00Z","id":"2222222222222222222222222222222222222222222222222222222222222222","short_id":"22222222","hostname":"test","paths":["/stub/data"]}],` +
	`"remove":[{"time":"2024-01-01T00:00:00Z","id":"1111111111111111111111111111111111111111111111111111111111111111","short_id":"11111111","hostname":"test","paths":["/stub/data"]}]}]`

// resticCalls returns the logged restic argv lines whose subcommand
// is cmd (the first argument).
func resticCalls(t *testing.T, logPath, cmd string) []string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("reading restic log: %v", err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if strings.HasPrefix(line, cmd+" ") || line == cmd {
			out = append(out, line)
		}
	}
	return out
}

// TestRunAppliesServiceRetention: after a verified backup, forget runs
// with the service's effective policy, scoped to the service's own tag
// as one series; prune runs only when forget removed something, and no
// retention failure changes the verified outcome.
func TestRunAppliesServiceRetention(t *testing.T) {
	tests := []struct {
		name       string
		override   *inventory.Retention
		env        []string
		wantKeep   []string
		notKeep    []string
		wantPrune  bool
		wantLogged string
	}{
		{
			name:     "follows destination",
			wantKeep: []string{"--keep-daily 7", "--keep-weekly 4", "--keep-monthly 6"},
		},
		{
			name:     "service override",
			override: &inventory.Retention{KeepDaily: 2, KeepMonthly: 12},
			wantKeep: []string{"--keep-daily 2", "--keep-monthly 12"},
			notKeep:  []string{"--keep-weekly"},
		},
		{
			name:       "removed snapshots are pruned",
			env:        []string{"RESTIC_STUB_FORGET=" + forgetRemovingOne},
			wantKeep:   []string{"--keep-daily 7"},
			wantPrune:  true,
			wantLogged: "removed 1 older one(s)",
		},
		{
			name:       "prune failure keeps SUCCESS",
			env:        []string{"RESTIC_STUB_FORGET=" + forgetRemovingOne, "RESTIC_STUB_FAIL_CMD=prune"},
			wantKeep:   []string{"--keep-daily 7"},
			wantPrune:  true,
			wantLogged: "reclaiming their space (prune) failed",
		},
		{
			name:       "forget failure keeps SUCCESS and skips prune",
			env:        []string{"RESTIC_STUB_FORGET=" + forgetRemovingOne, "RESTIC_STUB_FAIL_CMD=forget"},
			wantKeep:   []string{"--keep-daily 7"},
			wantLogged: "retention cleanup (forget) failed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			if err := inventory.PutService(h.db, inventory.Service{Name: "folder", CatalogEntry: "folder", Destination: "local", Retention: tt.override}); err != nil {
				t.Fatalf("PutService: %v", err)
			}
			var log bytes.Buffer
			p := h.pipeline(tt.env...)
			p.Log = &log
			p.Catalog = registryWith(t, map[string]string{"folder.yaml": yamlf(folderEntryYAML, t.TempDir())})

			state, err := p.Run(context.Background(), "folder")
			if err != nil || state != engine.SUCCESS {
				t.Fatalf("Run = %s, %v; want SUCCESS (retention never changes a verified outcome)", state, err)
			}

			forgets := resticCalls(t, h.resticLog, "forget")
			if len(forgets) != 1 {
				t.Fatalf("forget calls = %q, want one", forgets)
			}
			for _, part := range append([]string{"--tag ballast-service:folder", "--group-by tags"}, tt.wantKeep...) {
				if !strings.Contains(forgets[0], part) {
					t.Errorf("forget argv = %q, want %q", forgets[0], part)
				}
			}
			for _, part := range tt.notKeep {
				if strings.Contains(forgets[0], part) {
					t.Errorf("forget argv = %q, want no %q", forgets[0], part)
				}
			}
			if got := len(resticCalls(t, h.resticLog, "prune")); (got == 1) != tt.wantPrune || got > 1 {
				t.Errorf("prune calls = %d, want prune=%v", got, tt.wantPrune)
			}
			if tt.wantLogged != "" && !strings.Contains(log.String(), tt.wantLogged) {
				t.Errorf("log = %q, want %q", log.String(), tt.wantLogged)
			}
		})
	}
}

// TestRunSkipsRetentionUnlessVerified: an amber or failed run never
// forgets anything — its newest backup is not known to be restorable,
// so the older ones may be the only good copies.
func TestRunSkipsRetentionUnlessVerified(t *testing.T) {
	tests := []struct {
		name      string
		failCmd   string
		wantState engine.State
	}{
		{name: "read-back unreadable (amber)", failCmd: "ls", wantState: engine.SUCCESS_UNVERIFIED},
		{name: "upload failed", failCmd: "backup", wantState: engine.FAILED},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			if err := inventory.PutService(h.db, inventory.Service{Name: "folder", CatalogEntry: "folder", Destination: "local"}); err != nil {
				t.Fatalf("PutService: %v", err)
			}
			p := h.pipeline("RESTIC_STUB_FAIL_CMD=" + tt.failCmd)
			p.Log = &bytes.Buffer{}
			p.Catalog = registryWith(t, map[string]string{"folder.yaml": yamlf(folderEntryYAML, t.TempDir())})

			state, _ := p.Run(context.Background(), "folder")
			if state != tt.wantState {
				t.Fatalf("state = %s, want %s", state, tt.wantState)
			}
			if calls := resticCalls(t, h.resticLog, "forget"); len(calls) != 0 {
				t.Errorf("forget calls = %q, want none", calls)
			}
			if calls := resticCalls(t, h.resticLog, "prune"); len(calls) != 0 {
				t.Errorf("prune calls = %q, want none", calls)
			}
		})
	}
}

// TestRepoGate: one gate per repository location; retention gets it
// only when nobody uses the repository and is told who does; an
// upload or read-back waits only while a prune actually runs, says
// who holds the repository, and carries on once it is released.
func TestRepoGate(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	if repoGate(a) != repoGate(a) {
		t.Fatal("repoGate returned different gates for one location")
	}
	if repoGate(a) == repoGate(b) {
		t.Fatal("repoGate shared a gate between two locations")
	}
	ctx := context.Background()

	relImmich, err := repoGate(a).holdShared(ctx, "immich", nil)
	if err != nil {
		t.Fatal(err)
	}
	relPaperless, err := repoGate(a).holdShared(ctx, "paperless", nil)
	if err != nil {
		t.Fatal(err)
	}
	if rel, holder, _ := repoGate(a).tryExclusive("vaultwarden"); rel != nil || holder != "immich, paperless" {
		t.Fatalf("tryExclusive while shared = (%v, %q), want refused, held by immich, paperless", rel != nil, holder)
	}
	// Another repository is not affected by a's uploads.
	relB, _, _ := repoGate(b).tryExclusive("vaultwarden")
	if relB == nil {
		t.Fatal("another repository was blocked by a's uploads")
	}
	relB()
	relImmich()
	relPaperless()

	relPrune, _, _ := repoGate(a).tryExclusive("vaultwarden")
	if relPrune == nil {
		t.Fatal("tryExclusive refused a free repository")
	}
	if rel, holder, _ := repoGate(a).tryExclusive("folder"); rel != nil || holder != "vaultwarden" {
		t.Fatalf("second tryExclusive = (%v, %q), want refused, held by vaultwarden", rel != nil, holder)
	}
	var mu sync.Mutex
	var waits []string
	acquired := make(chan func())
	go func() {
		rel, err := repoGate(a).holdShared(ctx, "folder", func(h string) {
			mu.Lock()
			waits = append(waits, h)
			mu.Unlock()
		})
		if err != nil {
			t.Error(err)
		}
		acquired <- rel
	}()
	select {
	case <-acquired:
		t.Fatal("read-back ran while a prune was running")
	case <-time.After(50 * time.Millisecond):
	}
	relPrune()
	select {
	case rel := <-acquired:
		rel()
	case <-time.After(5 * time.Second):
		t.Fatal("read-back never ran after the prune finished")
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"vaultwarden", ""}; strings.Join(waits, "|") != strings.Join(want, "|") {
		t.Errorf("waiting calls = %q, want %q (holder, then cleared)", waits, want)
	}

	// A cancelled wait gives up with the context's error.
	relPrune, _, _ = repoGate(a).tryExclusive("vaultwarden")
	defer relPrune()
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := repoGate(a).holdShared(cctx, "folder", nil); err == nil {
		t.Error("holdShared with a cancelled context succeeded during a prune")
	}
}

// TestRetentionSkippedWhenRepositoryStaysBusy: retention that cannot
// get the repository within its wait limit is skipped and says so;
// it never runs forget or prune under another service's upload.
func TestRetentionSkippedWhenRepositoryStaysBusy(t *testing.T) {
	old := retentionWaitLimit
	retentionWaitLimit = 50 * time.Millisecond
	defer func() { retentionWaitLimit = old }()

	h := newHarness(t)
	loc := t.TempDir()
	log := &syncBuffer{}
	p := h.pipeline()
	p.Log = log
	repo, err := p.Resolver().Open(inventoryTarget(t, h))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	repo.Location = loc
	release, err := repoGate(loc).holdShared(context.Background(), "immich", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	p.applyRetention(context.Background(), "vaultwarden", "local", repo, inventory.Retention{KeepDaily: 7})
	for _, want := range []string{
		"vaultwarden: waiting for repository lock on local (held by immich)",
		"skipped removing old backups this time because local is still busy (in use by immich)",
	} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("log = %q, want %q", log.String(), want)
		}
	}
	if data, err := os.ReadFile(h.resticLog); err == nil && len(data) > 0 {
		t.Errorf("restic calls = %q, want none while the repository is busy", data)
	}
}

// TestRetentionIntegration runs real restic: two services share one
// repository, backups run concurrently (so prune and backup contend
// for the repository lock), and each service's own policy removes
// only its own old snapshots.
func TestRetentionIntegration(t *testing.T) {
	bin := requireRestic(t)
	ctx := context.Background()
	h := newHarness(t)
	repoDir := filepath.Join(t.TempDir(), "repo")
	if _, err := h.db.Exec(`UPDATE destinations SET location = ? WHERE name = 'local'`, repoDir); err != nil {
		t.Fatal(err)
	}
	for _, s := range []inventory.Service{
		{Name: "tight", CatalogEntry: "folder", Destination: "local", Retention: &inventory.Retention{KeepDaily: 1}},
		{Name: "loose", CatalogEntry: "folder", Destination: "local"},
	} {
		if err := inventory.PutService(h.db, s); err != nil {
			t.Fatalf("PutService %s: %v", s.Name, err)
		}
	}
	dataDir := t.TempDir()
	var mu sync.Mutex
	var log bytes.Buffer
	p := &Pipeline{
		DB:        h.db,
		Catalog:   registryWith(t, map[string]string{"folder.yaml": yamlf(folderEntryYAML, dataDir)}),
		Runner:    h.pipeline().Runner,
		CredDir:   h.credDir,
		WorkRoot:  t.TempDir(),
		ResticBin: bin,
		Log:       lockedWriter{mu: &mu, w: &log},
	}
	repo, err := p.Resolver().Open(inventoryTarget(t, h))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := repo.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	const rounds = 3
	for i := 0; i < rounds; i++ {
		if err := os.WriteFile(filepath.Join(dataDir, "round.txt"), []byte(strings.Repeat("x", 1000*(i+1))), 0o644); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		for _, svc := range []string{"tight", "loose"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if state, err := p.Run(ctx, svc); err != nil || state != engine.SUCCESS {
					t.Errorf("round %d: Run(%s) = %s, %v; want SUCCESS", i, svc, state, err)
				}
			}()
		}
		wg.Wait()
	}

	count := func(svc string) int {
		snaps, err := repo.Snapshots(ctx, snapshotFilterFor(svc))
		if err != nil {
			t.Fatalf("Snapshots(%s): %v", svc, err)
		}
		return len(snaps)
	}
	if got := count("tight"); got != 1 {
		t.Errorf("tight snapshots = %d, want 1 (keep_daily 1)", got)
	}
	// All rounds fall on one day. keep_daily 7 keeps that day's newest
	// snapshot, and restic (0.17+) also keeps the oldest one while the
	// policy's daily buckets are not yet full — so 2 of 3. Had tight's
	// keep_daily 1 reached loose's snapshots, only 1 would be left.
	if got := count("loose"); got != 2 {
		t.Errorf("loose snapshots = %d, want 2 of %d (its own 7d/4w/6m policy; tight's policy must not touch them); log:\n%s", got, rounds, log.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Count(log.String(), "tight: retention kept 1 backup(s) and removed 1 older one(s)") != rounds-1 {
		t.Errorf("log = %q, want tight's prune reported", log.String())
	}
	if strings.Contains(log.String(), "failed") {
		t.Errorf("log = %q, want no failures (lock contention must wait, not fail)", log.String())
	}
}

// inventoryTarget is the harness destination's backend target.
func inventoryTarget(t *testing.T, h *testHarness) backend.Target {
	t.Helper()
	d, err := inventory.GetDestination(h.db, "local")
	if err != nil {
		t.Fatalf("GetDestination: %v", err)
	}
	return d.Target()
}

// snapshotFilterFor selects one service's snapshots.
func snapshotFilterFor(service string) restic.SnapshotFilter {
	return restic.SnapshotFilter{Tags: []string{serviceTag(service)}}
}

// lockedWriter serializes writes from concurrent runs.
type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(b)
}

// syncBuffer is a goroutine-safe log sink a test can read while the
// pipeline is still writing to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitFor polls cond until it is true or the deadline passes.
func waitFor(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

// TestPendingRetentionNeverBlocksReadBack (H1): one service's long
// upload holds the repository's shared lock, another service's
// retention is waiting for the exclusive lock, and a third service's
// new backup must still upload, read back and finish verified. The
// pending retention says who it waits for, and runs once the upload
// lets go.
func TestPendingRetentionNeverBlocksReadBack(t *testing.T) {
	h := newHarness(t)
	loc := t.TempDir() // a lock key no other test shares
	dest := inventory.Destination{Name: "shared", Location: loc, KeepDaily: 7}
	if err := inventory.PutDestination(h.db, dest); err != nil {
		t.Fatalf("PutDestination: %v", err)
	}
	if err := os.WriteFile(filepath.Join(h.credDir, "ballast-restic-password.shared"), []byte("stub-password\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := inventory.PutService(h.db, inventory.Service{Name: "folder", CatalogEntry: "folder", Destination: "shared"}); err != nil {
		t.Fatalf("PutService: %v", err)
	}
	log := &syncBuffer{}
	p := h.pipeline("RESTIC_STUB_FORGET=" + forgetRemovingOne)
	p.Log = log
	p.Catalog = registryWith(t, map[string]string{"folder.yaml": yamlf(folderEntryYAML, t.TempDir())})
	stored, err := inventory.GetDestination(h.db, "shared")
	if err != nil {
		t.Fatalf("GetDestination: %v", err)
	}
	repo, err := p.Resolver().Open(stored.Target())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()

	// immich's hours-long upload.
	release, err := repoGate(loc).holdShared(ctx, "immich", nil)
	if err != nil {
		t.Fatalf("holdShared: %v", err)
	}
	released := false
	defer func() {
		if !released {
			release()
		}
	}()

	// vaultwarden's retention, pending behind it.
	retained := make(chan struct{})
	go func() {
		p.applyRetention(ctx, "vaultwarden", "shared", repo, inventory.Retention{KeepDaily: 7})
		close(retained)
	}()
	if !waitFor(5*time.Second, func() bool {
		return strings.Contains(log.String(), "vaultwarden: waiting for repository lock on shared (held by immich) before removing old backups")
	}) {
		t.Errorf("log = %q, want the pending retention to say it waits for immich", log.String())
	}

	// folder's new backup must reach a verified SUCCESS regardless.
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = p.Run(ctx, "folder")
	}()
	if !waitFor(10*time.Second, func() bool {
		runs, err := engine.RecentRuns(h.db, "folder", 1)
		return err == nil && len(runs) == 1 && runs[0].State == engine.SUCCESS
	}) {
		t.Fatalf("folder's read-back never completed while a retention step was pending; log = %q", log.String())
	}
	select {
	case <-retained:
		t.Fatal("retention ran while immich's upload held the repository")
	default:
	}

	// SUCCESS must have come through VERIFYING, with a passed read-back.
	runs, err := engine.RecentRuns(h.db, "folder", 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("RecentRuns: %v, %v", runs, err)
	}
	var states []string
	rows, err := h.db.Query(`SELECT to_state FROM run_transitions WHERE run_id = ? ORDER BY id`, runs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		states = append(states, s)
	}
	rows.Close()
	if n := len(states); n < 2 || states[n-2] != string(engine.VERIFYING) || states[n-1] != string(engine.SUCCESS) {
		t.Errorf("transitions = %v, want ... VERIFYING, SUCCESS", states)
	}
	results, err := engine.VerificationResults(h.db, runs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	readBack := false
	for _, r := range results {
		if r.Method == readBackMethod && r.Passed {
			readBack = true
		}
	}
	if !readBack {
		t.Errorf("verification results = %+v, want a passed %s", results, readBackMethod)
	}

	release()
	released = true
	for name, ch := range map[string]chan struct{}{"vaultwarden retention": retained, "folder run": done} {
		select {
		case <-ch:
		case <-time.After(10 * time.Second):
			t.Fatalf("%s never finished after the upload released the repository", name)
		}
	}
	if got := len(resticCalls(t, h.resticLog, "prune")); got != 2 {
		t.Errorf("prune calls = %d, want 2 (vaultwarden's and folder's retention both run eventually)", got)
	}
}

// TestRunSaysWhichServiceHoldsTheRepository: a backup that has to wait
// for another service's running prune says who holds the repository
// in the log and in its run status, clears it once it carries on, and
// still finishes verified.
func TestRunSaysWhichServiceHoldsTheRepository(t *testing.T) {
	h := newHarness(t)
	loc := t.TempDir()
	if err := inventory.PutDestination(h.db, inventory.Destination{Name: "shared", Location: loc, KeepDaily: 7}); err != nil {
		t.Fatalf("PutDestination: %v", err)
	}
	if err := os.WriteFile(filepath.Join(h.credDir, "ballast-restic-password.shared"), []byte("stub-password\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := inventory.PutService(h.db, inventory.Service{Name: "folder", CatalogEntry: "folder", Destination: "shared"}); err != nil {
		t.Fatalf("PutService: %v", err)
	}
	log := &syncBuffer{}
	p := h.pipeline()
	p.Log = log
	p.Catalog = registryWith(t, map[string]string{"folder.yaml": yamlf(folderEntryYAML, t.TempDir())})

	// vaultwarden's prune is running.
	release, _, _ := repoGate(loc).tryExclusive("vaultwarden")
	if release == nil {
		t.Fatal("tryExclusive refused a free repository")
	}
	type result struct {
		state engine.State
		err   error
	}
	done := make(chan result, 1)
	go func() {
		state, err := p.Run(context.Background(), "folder")
		done <- result{state, err}
	}()

	const note = "waiting for repository lock on shared (held by vaultwarden)"
	if !waitFor(10*time.Second, func() bool {
		last, err := engine.LastStartedRun(h.db, "folder")
		return err == nil && last.Waiting == note
	}) {
		last, _ := engine.LastStartedRun(h.db, "folder")
		t.Errorf("run waiting = %q, want %q", last.Waiting, note)
	}
	if !strings.Contains(log.String(), "folder: "+note) {
		t.Errorf("log = %q, want %q", log.String(), note)
	}
	release()

	select {
	case r := <-done:
		if r.err != nil || r.state != engine.SUCCESS {
			t.Fatalf("Run = %s, %v; want SUCCESS once the prune finished", r.state, r.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run never finished after the prune released the repository")
	}
	runs, err := engine.RecentRuns(h.db, "folder", 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("RecentRuns: %v, %v", runs, err)
	}
	var waiting sql.NullString
	if err := h.db.QueryRow(`SELECT waiting FROM runs WHERE id = ?`, runs[0].ID).Scan(&waiting); err != nil {
		t.Fatal(err)
	}
	if waiting.Valid {
		t.Errorf("waiting = %q after the run carried on, want cleared", waiting.String)
	}
}

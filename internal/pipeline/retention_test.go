package pipeline

import (
	"bytes"
	"context"
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

// TestRepoLock: one lock per repository location; retention's
// exclusive side waits for a backup's shared side, while other
// repositories are unaffected.
func TestRepoLock(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	first, second := repoLock(a), repoLock(a)
	if first != second {
		t.Fatal("repoLock returned different locks for one location")
	}
	if repoLock(a) == repoLock(b) {
		t.Fatal("repoLock shared a lock between two locations")
	}

	repoLock(a).RLock()
	acquired := make(chan struct{})
	go func() {
		repoLock(a).Lock()
		close(acquired)
		repoLock(a).Unlock()
	}()

	// Another repository is not blocked by a's backup.
	if !repoLock(b).TryLock() {
		t.Fatal("another repository was blocked by a's backup")
	}
	repoLock(b).Unlock()

	select {
	case <-acquired:
		t.Fatal("exclusive lock acquired while a backup held the shared lock")
	case <-time.After(50 * time.Millisecond):
	}
	repoLock(a).RUnlock()
	select {
	case <-acquired:
	case <-time.After(5 * time.Second):
		t.Fatal("exclusive lock never acquired after the shared lock was released")
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

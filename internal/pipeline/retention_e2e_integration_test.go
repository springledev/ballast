package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/springledev/ballast/internal/api"
	"github.com/springledev/ballast/internal/backend"
	"github.com/springledev/ballast/internal/engine"
	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/restic"
	"github.com/springledev/ballast/internal/runner"
	"github.com/springledev/ballast/internal/scheduler"
	"github.com/springledev/ballast/internal/secret"
	"github.com/springledev/ballast/internal/store"
)

// TestRepoBootstrapRetentionEndToEnd is the acceptance test for repo
// bootstrap + retention, driven through the same REST API the web UI
// uses, against real restic:
//
//  1. a destination added without setting up its repository makes a
//     backup fail red (never SUCCESS), with a hint naming the fix;
//  2. POST /destinations/{name}/init sets it up, and is idempotent;
//  3. per-service retention is set with PUT /services/{name};
//  4. every backup reaches VERIFYING → SUCCESS, and afterwards each
//     service's snapshot list (GET /services/{name}/snapshots, the UI's
//     Backups list) reflects only its own policy — two services
//     sharing one repository never prune each other;
//  5. the repository password appears in no API response, log line,
//     error, or the database.
func TestRepoBootstrapRetentionEndToEnd(t *testing.T) {
	resticBin := requireRestic(t)
	ctx := context.Background()

	dbPath := filepath.Join(t.TempDir(), "ballast.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	credDir := filepath.Join(t.TempDir(), "credentials")
	repoDir := filepath.Join(t.TempDir(), "repo")
	dataDir := t.TempDir()
	redact := secret.NewSet()
	r := &runner.Runner{Redact: redact}
	var logMu sync.Mutex
	var logBuf bytes.Buffer
	logW := lockedWriter{mu: &logMu, w: &logBuf}

	p := &Pipeline{
		DB:         st.DB(),
		Catalog:    registryWith(t, map[string]string{"folder.yaml": yamlf(folderEntryYAML, dataDir)}),
		Runner:     r,
		CredDir:    credDir,
		OwnCredDir: credDir,
		WorkRoot:   t.TempDir(),
		ResticBin:  resticBin,
		Redact:     redact,
		Log:        logW,
	}
	coord := &scheduler.Coordinator{
		DB: st.DB(), Locks: scheduler.FileLocker{Dir: filepath.Join(t.TempDir(), "locks")},
		Runner: p, LockWait: -1, Redact: redact, Log: logW,
	}
	ops := &api.Ops{
		DB: st.DB(), Catalog: p.Catalog, Coord: coord, Runner: r, ResticBin: resticBin, Ctx: ctx,
		Repo:                 func(d inventory.Destination) (*restic.Repository, error) { return p.Resolver().Open(d.Target()) },
		CredentialsDir:       credDir,
		CredentialSearchDirs: []string{credDir},
		Redact:               redact,
	}
	const token = "e2e-api-token"
	srv := httptest.NewServer(api.NewHandler(ops, token))
	t.Cleanup(srv.Close)

	var responses strings.Builder // every response body, for the leak check
	call := func(method, path string, body any) (int, []byte) {
		t.Helper()
		var rd io.Reader
		if body != nil {
			b, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			rd = bytes.NewReader(b)
		}
		req, err := http.NewRequestWithContext(ctx, method, srv.URL+path, rd)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		responses.Write(out)
		return resp.StatusCode, out
	}
	mustOK := func(code int, body []byte, what string) {
		t.Helper()
		if code/100 != 2 {
			t.Fatalf("%s: HTTP %d: %s", what, code, body)
		}
	}
	lastRun := func(svc string) engine.Run {
		t.Helper()
		runs, err := engine.RecentRuns(st.DB(), svc, 1)
		if err != nil || len(runs) != 1 {
			t.Fatalf("RecentRuns(%s) = %v, %v", svc, runs, err)
		}
		return runs[0]
	}
	backup := func(svc string) engine.Run {
		t.Helper()
		code, body := call("POST", "/api/v1/services/"+svc+"/backup", nil)
		mustOK(code, body, "start backup "+svc)
		ops.Wait()
		return lastRun(svc)
	}

	// Destination added without setting up its repository.
	code, body := call("POST", "/api/v1/destinations", map[string]any{
		"name": "local", "type": "local", "settings": map[string]string{"path": repoDir},
		"keep_daily": 7, "keep_weekly": 4, "keep_monthly": 6, "init": false,
	})
	mustOK(code, body, "create destination")
	for _, s := range []map[string]any{
		{"name": "tight", "catalog_entry": "folder", "destination": "local"},
		{"name": "loose", "catalog_entry": "folder", "destination": "local"},
	} {
		code, body := call("POST", "/api/v1/services", s)
		mustOK(code, body, "create service")
	}
	if err := os.WriteFile(filepath.Join(dataDir, "a.txt"), []byte("round 0"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 1. No repository yet: red, never green, with the fix named.
	run := backup("tight")
	if run.State != engine.FAILED {
		t.Fatalf("backup before init = %s, want FAILED", run.State)
	}
	if !strings.Contains(run.Error, "ballast destination init") {
		t.Errorf("failure detail = %q, want the `ballast destination init` hint", run.Error)
	}

	// 2. Set the repository up, twice (idempotent).
	for i := 0; i < 2; i++ {
		code, body := call("POST", "/api/v1/destinations/local/init", nil)
		mustOK(code, body, "init destination")
	}

	// 3. Per-service retention for tight only; bad policies are refused.
	code, body = call("PUT", "/api/v1/services/tight", map[string]any{
		"catalog_entry": "folder", "destination": "local", "retention": map[string]int{"keep_daily": 1},
	})
	mustOK(code, body, "set tight's retention")
	if code, body := call("PUT", "/api/v1/services/loose", map[string]any{
		"catalog_entry": "folder", "destination": "local", "retention": map[string]int{"keep_daily": 0},
	}); code != http.StatusBadRequest {
		t.Errorf("all-zero retention: HTTP %d %s, want 400", code, body)
	}

	// 4. Three rounds of backups, both services concurrently.
	const rounds = 3
	for i := 1; i <= rounds; i++ {
		if err := os.WriteFile(filepath.Join(dataDir, "a.txt"), []byte(strings.Repeat("round ", i*100)), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, svc := range []string{"tight", "loose"} {
			code, body := call("POST", "/api/v1/services/"+svc+"/backup", nil)
			mustOK(code, body, "start backup "+svc)
		}
		ops.Wait()
		for _, svc := range []string{"tight", "loose"} {
			run := lastRun(svc)
			if run.State != engine.SUCCESS {
				t.Fatalf("round %d %s = %s (%s), want SUCCESS", i, svc, run.State, run.Error)
			}
			if seq := transitionsOf(t, st.DB(), run.ID); !containsInOrder(seq, string(engine.VERIFYING), string(engine.SUCCESS)) {
				t.Errorf("round %d %s transitions = %v, want VERIFYING before SUCCESS", i, svc, seq)
			}
		}
	}

	snapshots := func(svc string) []restic.Snapshot {
		t.Helper()
		code, body := call("GET", "/api/v1/services/"+svc+"/snapshots", nil)
		mustOK(code, body, "list snapshots "+svc)
		var snaps []restic.Snapshot
		if err := json.Unmarshal(body, &snaps); err != nil {
			t.Fatalf("decoding snapshots: %v (%s)", err, body)
		}
		return snaps
	}
	tight, loose := snapshots("tight"), snapshots("loose")
	if len(tight) != 1 || tight[0].ID != lastRun("tight").SnapshotID {
		t.Errorf("tight snapshots = %+v, want only its newest (%s) under keep_daily 1", tight, lastRun("tight").SnapshotID)
	}
	// Same day: keep_daily 7 keeps the newest, and restic keeps the
	// oldest while daily buckets are unfilled — 2 of 3. tight's policy
	// reaching loose would leave 1.
	if len(loose) != 2 {
		t.Errorf("loose snapshots = %d, want 2 of %d (its own 7d/4w/6m policy)", len(loose), rounds)
	}
	logMu.Lock()
	logText := logBuf.String()
	logMu.Unlock()
	if !strings.Contains(logText, "tight: retention kept 1 backup(s) and removed 1 older one(s)") {
		t.Errorf("log does not report tight's prune:\n%s", logText)
	}

	// The prune really reclaimed data, and the repository is sound.
	repo, err := p.Resolver().Open(inventoryTargetOf(t, st, "local"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(ctx, nil, nil, runner.Command{Name: resticBin, Args: []string{"check"}, Env: []string{"RESTIC_REPOSITORY=" + repo.Location, "RESTIC_PASSWORD_FILE=" + repo.PasswordFile}}); err != nil {
		t.Errorf("restic check after pruning: %v", err)
	}

	// 5. The generated repository password leaks nowhere.
	pw, err := os.ReadFile(filepath.Join(credDir, "ballast-restic-password.local"))
	if err != nil {
		t.Fatalf("reading generated password: %v", err)
	}
	secretText := strings.TrimSpace(string(pw))
	if len(secretText) < 16 {
		t.Fatalf("generated password unexpectedly short")
	}
	for name, text := range map[string]string{"API responses": responses.String(), "log": logText, "run error": run.Error} {
		if strings.Contains(text, secretText) {
			t.Errorf("repository password found in %s", name)
		}
	}
	for _, f := range []string{dbPath, dbPath + "-wal"} {
		if b, err := os.ReadFile(f); err == nil && bytes.Contains(b, []byte(secretText)) {
			t.Errorf("repository password found in %s", f)
		}
	}
}

// inventoryTargetOf is destination name's backend target.
func inventoryTargetOf(t *testing.T, st *store.Store, name string) backend.Target {
	t.Helper()
	d, err := inventory.GetDestination(st.DB(), name)
	if err != nil {
		t.Fatalf("GetDestination: %v", err)
	}
	return d.Target()
}

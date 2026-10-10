package pipeline

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/springledev/keel/dockerhost"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/springledev/ballast/internal/catalog"
	"github.com/springledev/ballast/internal/engine"
	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/store"
	"github.com/springledev/keel/runner"
)

// --- restic stub -----------------------------------------------------
//
// A fake restic CLI, hermetic (no real restic binary required). It
// logs every invocation to $RESTIC_STUB_LOG and answers each
// subcommand with fixed, internally-consistent --json output: one
// snapshot ("1111...") covering path "/stub/data", with one sample
// file node so ReadBack's LS+Dump sampling succeeds. Any subcommand
// named by $RESTIC_STUB_FAIL_CMD exits 1 instead. `forget` answers
// $RESTIC_STUB_FORGET when set (to simulate removed snapshots).
const resticStubScript = `#!/bin/sh
printf '%s\n' "$*" >> "$RESTIC_STUB_LOG"
cmd="$1"
if [ -n "$RESTIC_STUB_FAIL_CMD" ] && [ "$cmd" = "$RESTIC_STUB_FAIL_CMD" ]; then
  echo "stub: simulated restic $cmd failure" >&2
  exit 1
fi
case "$cmd" in
  backup)
    printf '{"message_type":"summary","snapshot_id":"1111111111111111111111111111111111111111111111111111111111111111","files_new":1,"total_files_processed":1}\n'
    ;;
  snapshots)
    if [ -n "$RESTIC_STUB_NO_SNAPSHOTS" ]; then printf '[]'; exit 0; fi
    printf '[{"time":"2024-01-01T00:00:00Z","id":"1111111111111111111111111111111111111111111111111111111111111111","short_id":"11111111","hostname":"test","tags":["ballast"],"paths":["/stub/data"]}]\n'
    ;;
  ls)
    printf '{"message_type":"snapshot","time":"2024-01-01T00:00:00Z","id":"1111111111111111111111111111111111111111111111111111111111111111","short_id":"11111111","hostname":"test","paths":["/stub/data"]}\n'
    printf '{"message_type":"node","name":"sample.txt","type":"file","path":"/stub/data/sample.txt","size":11}\n'
    ;;
  dump)
    printf 'sample-content'
    ;;
  forget)
    if [ -n "$RESTIC_STUB_FORGET" ]; then printf '%s' "$RESTIC_STUB_FORGET"; else printf '[]'; fi
    ;;
  prune)
    ;;
  *)
    echo "unknown stub restic command: $cmd" >&2
    exit 2
    ;;
esac
exit 0
`

// --- docker stub -------------------------------------------------------
//
// A fake docker CLI. Logs every invocation to $DOCKER_STUB_LOG,
// sleeps $DOCKER_STUB_SLEEP seconds first when set (for cancellation
// tests), and answers `docker exec ...` with $DOCKER_STUB_DUMP
// (defaulting to a small SQL statement) on stdout.
const dockerStubScript = `#!/bin/sh
printf '%s\n' "$*" >> "$DOCKER_STUB_LOG"
if [ -n "$DOCKER_STUB_SLEEP" ]; then
  # exec-replace (not a plain "sleep &") so this process IS the sleep:
  # a SIGKILL of this PID takes effect immediately, instead of killing
  # a shell parent while an orphaned "sleep" grandchild keeps the
  # stdout pipe open until it finishes on its own.
  exec sleep "$DOCKER_STUB_SLEEP"
fi
# Skip per-host connection flags (a remote Docker host).
while [ $# -gt 0 ]; do
  case "$1" in
    --host|--tlscacert|--tlscert|--tlskey) shift 2 ;;
    --tlsverify) shift ;;
    *) break ;;
  esac
done
cmd="$1"
case "$cmd" in
  exec)
    if [ -n "$DOCKER_STUB_DUMP" ]; then
      printf '%s' "$DOCKER_STUB_DUMP"
    else
      printf 'CREATE TABLE stub(id int);\n'
    fi
    ;;
esac
exit 0
`

func writeStub(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("writing stub %s: %v", name, err)
	}
	return path
}

// testHarness bundles everything a pipeline test needs: a database, a
// runner pointed at the restic/docker stubs, and a configured
// credential + work directory.
type testHarness struct {
	db        *sql.DB
	runnerEnv []string
	resticLog string
	dockerLog string
	credDir   string
	workRoot  string
	resticBin string
	dockerBin string
}

func newHarness(t *testing.T) *testHarness {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "ballast.db"))
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	resticBin := writeStub(t, "restic-stub", resticStubScript)
	dockerBin := writeStub(t, "docker-stub", dockerStubScript)
	resticLog := filepath.Join(t.TempDir(), "restic.log")
	dockerLog := filepath.Join(t.TempDir(), "docker.log")

	credDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(credDir, "ballast-restic-password.local"), []byte("stub-password\n"), 0o600); err != nil {
		t.Fatalf("writing password file: %v", err)
	}

	h := &testHarness{
		db:        s.DB(),
		resticLog: resticLog,
		dockerLog: dockerLog,
		credDir:   credDir,
		workRoot:  t.TempDir(),
		resticBin: resticBin,
		dockerBin: dockerBin,
	}
	h.runnerEnv = append(os.Environ(), "RESTIC_STUB_LOG="+resticLog, "DOCKER_STUB_LOG="+dockerLog)

	if err := inventory.PutDestination(h.db, inventory.Destination{
		Name: "local", Location: "/stub-repo", KeepDaily: 7, KeepWeekly: 4, KeepMonthly: 6,
	}); err != nil {
		t.Fatalf("PutDestination: %v", err)
	}
	return h
}

func (h *testHarness) pipeline(extraEnv ...string) *Pipeline {
	return &Pipeline{
		DB:        h.db,
		Runner:    &runner.Runner{BaseEnv: append(append([]string(nil), h.runnerEnv...), extraEnv...)},
		CredDir:   h.credDir,
		WorkRoot:  h.workRoot,
		ResticBin: h.resticBin,
		DockerBin: h.dockerBin,
	}
}

// registryWith loads a Registry from inline YAML catalog entries.
func registryWith(t *testing.T, yamls map[string]string) *catalog.Registry {
	t.Helper()
	fsys := fstest.MapFS{}
	for name, content := range yamls {
		fsys[name] = &fstest.MapFile{Data: []byte(content)}
	}
	r, err := catalog.LoadRegistry(fsys, nil)
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	return r
}

const folderEntryYAML = `
apiVersion: ballast.dev/v1
service: folder
version: 1
backup:
  strategy: files
  files:
    - path: %q
verify:
  method: archive-integrity
`

const dumpTarEntryYAML = `
apiVersion: ballast.dev/v1
service: dumptar
version: 1
backup:
  strategy: dump
  dumps:
    - name: data
      container: mycontainer
      command: ["mydump"]
      format: tar
verify:
  method: archive-integrity
`

const dumpSQLEntryYAML = `
apiVersion: ballast.dev/v1
service: dumpsql
version: 1
backup:
  strategy: dump
  dumps:
    - name: data
      container: mycontainer
      command: ["mydump"]
      format: sql
verify:
  method: archive-integrity
`

const dumpSecretEntryYAML = `
apiVersion: ballast.dev/v1
service: dumpsecret
version: 1
backup:
  strategy: dump
  dumps:
    - name: data
      container: mycontainer
      command: ["mydump"]
      format: sql
      secretEnv:
        DB_PASSWORD: "{{ .Secrets.db_password }}"
verify:
  method: archive-integrity
`

func yamlf(tmpl, arg string) string {
	return strings.ReplaceAll(tmpl, "%q", `"`+arg+`"`)
}

// transitionsOf returns the ordered to_state values recorded for run.
func transitionsOf(t *testing.T, db *sql.DB, runID int64) []string {
	t.Helper()
	rows, err := db.Query(`SELECT to_state FROM run_transitions WHERE run_id = ? ORDER BY id`, runID)
	if err != nil {
		t.Fatalf("querying transitions: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scanning transition: %v", err)
		}
		out = append(out, s)
	}
	return out
}

func containsInOrder(seq []string, a, b string) bool {
	ai, bi := -1, -1
	for i, s := range seq {
		if s == a && ai == -1 {
			ai = i
		}
		if s == b {
			bi = i
		}
	}
	return ai != -1 && bi != -1 && ai < bi
}

func TestRunFilesHappyPathZeroArtifacts(t *testing.T) {
	h := newHarness(t)
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "hello.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := inventory.PutService(h.db, inventory.Service{Name: "folder", CatalogEntry: "folder", Destination: "local"}); err != nil {
		t.Fatalf("PutService: %v", err)
	}
	p := h.pipeline()
	p.Catalog = registryWith(t, map[string]string{"folder.yaml": yamlf(folderEntryYAML, dataDir)})

	state, err := p.Run(context.Background(), "folder")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if state != engine.SUCCESS {
		t.Fatalf("state = %s, want SUCCESS", state)
	}

	runs, err := engine.RecentRuns(h.db, "folder", 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("RecentRuns: %v, %v", runs, err)
	}
	run := runs[0]
	if run.SnapshotID == "" {
		t.Error("SnapshotID not recorded")
	}
	seq := transitionsOf(t, h.db, run.ID)
	if !containsInOrder(seq, string(engine.VERIFYING), string(engine.SUCCESS)) {
		t.Errorf("transitions = %v, want VERIFYING before SUCCESS", seq)
	}

	results, err := engine.VerificationResults(h.db, run.ID)
	if err != nil {
		t.Fatalf("VerificationResults: %v", err)
	}
	methods := map[string]bool{}
	for _, r := range results {
		methods[r.Method] = true
		if !r.Passed {
			t.Errorf("record %s not passed: %+v", r.Method, r)
		}
	}
	if !methods["archive-integrity"] || !methods["restic-read-back"] {
		t.Errorf("verification methods = %v, want archive-integrity and restic-read-back both recorded", methods)
	}
}

func TestRunUnknownServiceNoRun(t *testing.T) {
	h := newHarness(t)
	p := h.pipeline()
	p.Catalog = registryWith(t, nil)

	state, err := p.Run(context.Background(), "nope")
	if err == nil {
		t.Fatal("Run: want an error for an unknown service")
	}
	if state != "" {
		t.Errorf("state = %q, want empty (no run created)", state)
	}
	runs, err := engine.RecentRuns(h.db, "nope", 10)
	if err != nil {
		t.Fatalf("RecentRuns: %v", err)
	}
	if len(runs) != 0 {
		t.Errorf("runs = %v, want none for a service that never started", runs)
	}
}

func TestRunUnknownCatalogEntryNoRun(t *testing.T) {
	h := newHarness(t)
	if err := inventory.PutService(h.db, inventory.Service{Name: "svc", CatalogEntry: "ghost", Destination: "local"}); err != nil {
		t.Fatalf("PutService: %v", err)
	}
	p := h.pipeline()
	p.Catalog = registryWith(t, nil)

	state, err := p.Run(context.Background(), "svc")
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("Run error = %v, want it to name the missing catalog entry", err)
	}
	if state != "" {
		t.Errorf("state = %q, want empty", state)
	}
	runs, err := engine.RecentRuns(h.db, "svc", 10)
	if err != nil {
		t.Fatalf("RecentRuns: %v", err)
	}
	if len(runs) != 0 {
		t.Errorf("runs = %v, want none", runs)
	}
}

func TestRunUploadFailure(t *testing.T) {
	h := newHarness(t)
	if err := inventory.PutService(h.db, inventory.Service{Name: "dumpsql", CatalogEntry: "dumpsql", Destination: "local"}); err != nil {
		t.Fatalf("PutService: %v", err)
	}
	p := h.pipeline("RESTIC_STUB_FAIL_CMD=backup")
	p.Catalog = registryWith(t, map[string]string{"dumpsql.yaml": dumpSQLEntryYAML})

	state, err := p.Run(context.Background(), "dumpsql")
	if err == nil {
		t.Fatal("Run: want an error when the restic backup fails")
	}
	if state != engine.FAILED {
		t.Errorf("state = %s, want FAILED", state)
	}
	if !strings.Contains(err.Error(), "dumpsql") {
		t.Errorf("error = %v, want it to name the service", err)
	}

	runs, err := engine.RecentRuns(h.db, "dumpsql", 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("RecentRuns: %v, %v", runs, err)
	}
	if runs[0].State != engine.FAILED {
		t.Errorf("persisted state = %s, want FAILED", runs[0].State)
	}
}

func TestRunCheckerFailsFromVerifying(t *testing.T) {
	h := newHarness(t)
	if err := inventory.PutService(h.db, inventory.Service{Name: "dumptar", CatalogEntry: "dumptar", Destination: "local"}); err != nil {
		t.Fatalf("PutService: %v", err)
	}
	// The docker stub's default exec output ("CREATE TABLE...") is not
	// valid tar content, so archive-integrity (format: tar) fails.
	p := h.pipeline()
	p.Catalog = registryWith(t, map[string]string{"dumptar.yaml": dumpTarEntryYAML})

	state, err := p.Run(context.Background(), "dumptar")
	if err == nil {
		t.Fatal("Run: want an error when archive-integrity fails")
	}
	if state != engine.FAILED {
		t.Errorf("state = %s, want FAILED", state)
	}

	runs, err := engine.RecentRuns(h.db, "dumptar", 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("RecentRuns: %v, %v", runs, err)
	}
	seq := transitionsOf(t, h.db, runs[0].ID)
	if !containsInOrder(seq, string(engine.VERIFYING), string(engine.FAILED)) {
		t.Errorf("transitions = %v, want the failure recorded from VERIFYING", seq)
	}
	// The last transition must be VERIFYING -> FAILED specifically
	// (not e.g. DUMPING -> FAILED), so NotifyVerificationFailure (not
	// NotifyJobFailure) is what the engine fires.
	if seq[len(seq)-1] != string(engine.FAILED) {
		t.Fatalf("transitions = %v, want FAILED last", seq)
	}

	results, err := engine.VerificationResults(h.db, runs[0].ID)
	if err != nil {
		t.Fatalf("VerificationResults: %v", err)
	}
	found := false
	for _, r := range results {
		if r.Method == "archive-integrity" {
			found = true
			if r.Passed {
				t.Error("archive-integrity record Passed = true, want false")
			}
		}
	}
	if !found {
		t.Error("no archive-integrity record persisted")
	}
}

func TestRunReadBackFailureAmber(t *testing.T) {
	h := newHarness(t)
	dataDir := t.TempDir()
	if err := inventory.PutService(h.db, inventory.Service{Name: "folder", CatalogEntry: "folder", Destination: "local"}); err != nil {
		t.Fatalf("PutService: %v", err)
	}
	p := h.pipeline("RESTIC_STUB_FAIL_CMD=ls")
	p.Catalog = registryWith(t, map[string]string{"folder.yaml": yamlf(folderEntryYAML, dataDir)})

	state, err := p.Run(context.Background(), "folder")
	if err == nil {
		t.Fatal("Run: want a non-nil error for SUCCESS_UNVERIFIED")
	}
	if state != engine.SUCCESS_UNVERIFIED {
		t.Fatalf("state = %s, want SUCCESS_UNVERIFIED", state)
	}

	runs, err := engine.RecentRuns(h.db, "folder", 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("RecentRuns: %v, %v", runs, err)
	}
	if runs[0].State != engine.SUCCESS_UNVERIFIED {
		t.Errorf("persisted state = %s, want SUCCESS_UNVERIFIED", runs[0].State)
	}
	results, err := engine.VerificationResults(h.db, runs[0].ID)
	if err != nil {
		t.Fatalf("VerificationResults: %v", err)
	}
	for _, r := range results {
		if r.Method == "restic-read-back" && r.Passed {
			t.Error("restic-read-back Passed = true, want false (ls was made to fail)")
		}
	}
}

func TestRunCancelledMidDump(t *testing.T) {
	h := newHarness(t)
	if err := inventory.PutService(h.db, inventory.Service{Name: "dumpsql", CatalogEntry: "dumpsql", Destination: "local"}); err != nil {
		t.Fatalf("PutService: %v", err)
	}
	p := h.pipeline("DOCKER_STUB_SLEEP=2")
	p.Catalog = registryWith(t, map[string]string{"dumpsql.yaml": dumpSQLEntryYAML})

	ctx, cancel := context.WithCancel(context.Background())
	var (
		state engine.State
		err   error
		wg    sync.WaitGroup
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		state, err = p.Run(ctx, "dumpsql")
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}

	if state != engine.CANCELLED {
		t.Errorf("state = %s, want CANCELLED", state)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled", err)
	}

	runs, err2 := engine.RecentRuns(h.db, "dumpsql", 1)
	if err2 != nil || len(runs) != 1 {
		t.Fatalf("RecentRuns: %v, %v", runs, err2)
	}
	if runs[0].State != engine.CANCELLED {
		t.Errorf("persisted state = %s, want CANCELLED", runs[0].State)
	}
}

// stubNotifier records which outcome hooks were called, for asserting
// that the pipeline attaches a Notifier to every Machine it creates.
type stubNotifier struct {
	mu                sync.Mutex
	jobFailures       int
	verificationFails int
	successHeartbeats int
}

func (n *stubNotifier) NotifyJobFailure(_ context.Context, _ string, _ int64, _ string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.jobFailures++
	return nil
}
func (n *stubNotifier) NotifyVerificationFailure(_ context.Context, _ string, _ int64, _ string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.verificationFails++
	return nil
}
func (n *stubNotifier) NotifySuccessHeartbeat(_ context.Context, _ string, _ int64) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.successHeartbeats++
	return nil
}

func TestRunNotifiesOnOutcomes(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		h := newHarness(t)
		dataDir := t.TempDir()
		if err := inventory.PutService(h.db, inventory.Service{Name: "folder", CatalogEntry: "folder", Destination: "local"}); err != nil {
			t.Fatalf("PutService: %v", err)
		}
		n := &stubNotifier{}
		p := h.pipeline()
		p.Catalog = registryWith(t, map[string]string{"folder.yaml": yamlf(folderEntryYAML, dataDir)})
		p.Notifier = n

		if _, err := p.Run(context.Background(), "folder"); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if n.successHeartbeats != 1 {
			t.Errorf("successHeartbeats = %d, want 1", n.successHeartbeats)
		}
	})

	t.Run("verification failure", func(t *testing.T) {
		h := newHarness(t)
		if err := inventory.PutService(h.db, inventory.Service{Name: "dumptar", CatalogEntry: "dumptar", Destination: "local"}); err != nil {
			t.Fatalf("PutService: %v", err)
		}
		n := &stubNotifier{}
		p := h.pipeline()
		p.Catalog = registryWith(t, map[string]string{"dumptar.yaml": dumpTarEntryYAML})
		p.Notifier = n

		if _, err := p.Run(context.Background(), "dumptar"); err == nil {
			t.Fatal("Run: want an error")
		}
		if n.verificationFails != 1 {
			t.Errorf("verificationFails = %d, want 1", n.verificationFails)
		}
		if n.jobFailures != 0 {
			t.Errorf("jobFailures = %d, want 0 (this is a verification failure, not a plain job failure)", n.jobFailures)
		}
	})
}

// TestRunResolvesCatalogSecretEnv checks the spec §14 step 15 path
// end to end: a service's Secrets map points at a root-only
// credential file, catalog.Entry.ResolveSecrets fills the dump's
// {{ .Secrets.NAME }} secretEnv placeholder with its plaintext value,
// the value is forwarded into the container via `docker exec -e KEY`
// (never argv), the run still succeeds, and the plaintext value never
// appears in the pipeline's own log output.
func TestRunResolvesCatalogSecretEnv(t *testing.T) {
	h := newHarness(t)
	credName := "ballast-secret.dumpsecret.db_password"
	if err := os.WriteFile(filepath.Join(h.credDir, credName), []byte("hunter2\n"), 0o600); err != nil {
		t.Fatalf("writing secret credential file: %v", err)
	}
	if err := inventory.PutService(h.db, inventory.Service{
		Name: "dumpsecret", CatalogEntry: "dumpsecret", Destination: "local",
		Secrets: map[string]string{"db_password": ""},
	}); err != nil {
		t.Fatalf("PutService: %v", err)
	}

	var logBuf strings.Builder
	p := h.pipeline()
	p.Catalog = registryWith(t, map[string]string{"dumpsecret.yaml": dumpSecretEntryYAML})
	p.Log = &logBuf

	state, err := p.Run(context.Background(), "dumpsecret")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if state != engine.SUCCESS {
		t.Fatalf("state = %s, want SUCCESS", state)
	}

	dockerLog, err := os.ReadFile(h.dockerLog)
	if err != nil {
		t.Fatalf("reading docker log: %v", err)
	}
	if !strings.Contains(string(dockerLog), "-e DB_PASSWORD") {
		t.Errorf("docker invocations = %q, want -e DB_PASSWORD forwarded", dockerLog)
	}
	if strings.Contains(string(dockerLog), "hunter2") {
		t.Errorf("docker argv leaked the secret value: %q", dockerLog)
	}
	if strings.Contains(logBuf.String(), "hunter2") {
		t.Errorf("pipeline log leaked the secret value: %q", logBuf.String())
	}
}

// TestRunUnknownCatalogSecretFails checks that a service whose
// Secrets map is missing an entry the catalog's secretEnv references
// fails the run loudly (via ResolveSecrets), rather than running the
// dump with an empty/unsubstituted placeholder. It is a run-time
// failure, so a FAILED run is recorded (red status, notification).
func TestRunUnknownCatalogSecretFails(t *testing.T) {
	h := newHarness(t)
	if err := inventory.PutService(h.db, inventory.Service{
		Name: "dumpsecret", CatalogEntry: "dumpsecret", Destination: "local",
	}); err != nil {
		t.Fatalf("PutService: %v", err)
	}
	p := h.pipeline()
	p.Catalog = registryWith(t, map[string]string{"dumpsecret.yaml": dumpSecretEntryYAML})

	state, err := p.Run(context.Background(), "dumpsecret")
	if err == nil || !strings.Contains(err.Error(), "db_password") {
		t.Fatalf("Run error = %v, want it to name the unresolved secret db_password", err)
	}
	if state != engine.FAILED {
		t.Errorf("state = %q, want FAILED (recorded run, so the service goes red)", state)
	}
}

// TestRunMissingSecretFileFails checks that a declared secret whose
// credential file is missing records a FAILED run.
func TestRunMissingSecretFileFails(t *testing.T) {
	h := newHarness(t)
	if err := inventory.PutService(h.db, inventory.Service{
		Name: "dumpsecret", CatalogEntry: "dumpsecret", Destination: "local",
		Secrets: map[string]string{"db_password": "no-such-credential-file"},
	}); err != nil {
		t.Fatalf("PutService: %v", err)
	}
	p := h.pipeline()
	p.Catalog = registryWith(t, map[string]string{"dumpsecret.yaml": dumpSecretEntryYAML})

	state, err := p.Run(context.Background(), "dumpsecret")
	if err == nil || !strings.Contains(err.Error(), "no-such-credential-file") {
		t.Fatalf("Run error = %v, want it to name the missing credential file", err)
	}
	if state != engine.FAILED {
		t.Errorf("state = %q, want FAILED", state)
	}
}

func TestEnsureServiceContext(t *testing.T) {
	base := errors.New("service svc: something broke")
	if got := ensureServiceContext("svc", base); got.Error() != base.Error() {
		t.Errorf("ensureServiceContext should not double-wrap: got %q", got.Error())
	}
	other := errors.New("repository unreachable")
	got := ensureServiceContext("svc", other)
	if !strings.HasPrefix(got.Error(), "service svc:") {
		t.Errorf("ensureServiceContext = %q, want a service prefix", got.Error())
	}
}

// TestRunRoutesDockerToServiceHost checks that every docker command a
// run issues goes to the service's own Docker host, never the local
// daemon, and that a host whose credential files are missing fails
// the run (red, notified) rather than falling back to local.
func TestRunRoutesDockerToServiceHost(t *testing.T) {
	h := newHarness(t)
	for _, f := range []string{"ballast-dockerhost.media.ca.pem", "ballast-dockerhost.media.cert.pem", "ballast-dockerhost.media.key.pem"} {
		if err := os.WriteFile(filepath.Join(h.credDir, f), []byte("pem\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, host := range []dockerhost.Host{
		{Name: "media", Kind: dockerhost.RemoteTLS, Address: "media", Credential: "ballast-dockerhost.media"},
		{Name: "broken", Kind: dockerhost.RemoteTLS, Address: "media", Credential: "ballast-dockerhost.gone"},
	} {
		if _, err := inventory.PutDockerHost(h.db, host); err != nil {
			t.Fatal(err)
		}
	}
	for svc, host := range map[string]string{"onmedia": "media", "onbroken": "broken"} {
		if err := inventory.PutService(h.db, inventory.Service{Name: svc, CatalogEntry: "dumpsql", Destination: "local", DockerHost: host}); err != nil {
			t.Fatal(err)
		}
	}
	p := h.pipeline()
	p.Catalog = registryWith(t, map[string]string{"dumpsql.yaml": dumpSQLEntryYAML})

	state, err := p.Run(context.Background(), "onmedia")
	if err != nil || state != engine.SUCCESS {
		t.Fatalf("Run = %s, %v; want SUCCESS", state, err)
	}
	log, err := os.ReadFile(h.dockerLog)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(log)), "\n")
	prefix := "--host tcp://media:2376 --tlsverify --tlscacert " + filepath.Join(h.credDir, "ballast-dockerhost.media.ca.pem")
	for _, l := range lines {
		if !strings.HasPrefix(l, prefix) {
			t.Errorf("docker call %q did not target host media", l)
		}
	}
	if !strings.Contains(string(log), " exec ") {
		t.Errorf("no docker exec was routed to the host: %q", log)
	}

	state, err = p.Run(context.Background(), "onbroken")
	if state != engine.FAILED || err == nil || !strings.Contains(err.Error(), "docker host broken") {
		t.Fatalf("Run on a host without its TLS files = %s, %v; want FAILED naming the host", state, err)
	}
}

const dumpSmokeEntryYAML = `
apiVersion: ballast.dev/v1
service: dumpsmoke
version: 1
backup:
  strategy: dump
  dumps:
    - name: data
      container: mycontainer
      command: ["mydump"]
      format: sql
verify:
  methods:
    - archive-integrity
    - %q
  smoke:
    container: mycontainer
    timeout: 1s
`

// TestRunEveryDeclaredMethodEnforced pins spec §17.6: a run declaring
// several verification methods is FAILED when any one of them fails
// (here restart-smoke — the docker stub never reports the container
// as running — while archive-integrity passes), and a declared method
// Ballast cannot run is a hard failure before anything is touched,
// never a silent pass.
func TestRunEveryDeclaredMethodEnforced(t *testing.T) {
	cases := []struct {
		name        string
		method      string
		wantRecords map[string]bool // method -> Passed; nil = no run should reach VERIFYING
		wantErr     string
	}{
		{
			name:        "failing restart-smoke fails the run",
			method:      "restart-smoke",
			wantRecords: map[string]bool{"archive-integrity": true, "restart-smoke": false, "restic-read-back": true},
			wantErr:     "restart-smoke",
		},
		{
			name:    "unimplemented method fails the run",
			method:  "read-back",
			wantErr: `"read-back" is not available`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			if err := inventory.PutService(h.db, inventory.Service{Name: "dumpsmoke", CatalogEntry: "dumpsmoke", Destination: "local"}); err != nil {
				t.Fatalf("PutService: %v", err)
			}
			p := h.pipeline()
			p.Catalog = registryWith(t, map[string]string{"dumpsmoke.yaml": yamlf(dumpSmokeEntryYAML, tc.method)})

			state, err := p.Run(context.Background(), "dumpsmoke")
			if state != engine.FAILED {
				t.Errorf("state = %s, want FAILED", state)
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Run error = %v, want it to contain %q", err, tc.wantErr)
			}

			runs, err := engine.RecentRuns(h.db, "dumpsmoke", 1)
			if err != nil || len(runs) != 1 {
				t.Fatalf("RecentRuns: %v, %v", runs, err)
			}
			seq := transitionsOf(t, h.db, runs[0].ID)
			for _, s := range seq {
				if s == string(engine.SUCCESS) || s == string(engine.SUCCESS_UNVERIFIED) {
					t.Fatalf("transitions = %v, must never reach %s", seq, s)
				}
			}
			results, err := engine.VerificationResults(h.db, runs[0].ID)
			if err != nil {
				t.Fatalf("VerificationResults: %v", err)
			}
			got := map[string]bool{}
			for _, r := range results {
				got[r.Method] = r.Passed
			}
			if len(got) != len(tc.wantRecords) {
				t.Fatalf("verification records = %v, want %v", got, tc.wantRecords)
			}
			for m, passed := range tc.wantRecords {
				if g, ok := got[m]; !ok || g != passed {
					t.Errorf("record %s = %v (present %v), want passed=%v", m, g, ok, passed)
				}
			}
		})
	}
}

// TestRunEnvFileSecret checks the spec §18.2 path: a secret that points
// at a key in an app's .env is read at run time (so a rotated password
// is used with no re-setup), is forwarded like any other secret without
// reaching argv or the log, and a missing key fails the run at PREPARING
// naming the exact file and key.
func TestRunEnvFileSecret(t *testing.T) {
	h := newHarness(t)
	envPath := filepath.Join(t.TempDir(), ".env")
	writeEnv := func(content string) {
		t.Helper()
		if err := os.WriteFile(envPath, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeEnv("DB_PASSWORD=hunter2\n")
	if err := inventory.PutService(h.db, inventory.Service{
		Name: "dumpsecret", CatalogEntry: "dumpsecret", Destination: "local",
		Secrets: map[string]string{"db_password": inventory.EnvRef(envPath, "DB_PASSWORD")},
	}); err != nil {
		t.Fatalf("PutService: %v", err)
	}
	var logBuf strings.Builder
	p := h.pipeline()
	p.Catalog = registryWith(t, map[string]string{"dumpsecret.yaml": dumpSecretEntryYAML})
	p.Log = &logBuf

	if state, err := p.Run(context.Background(), "dumpsecret"); err != nil || state != engine.SUCCESS {
		t.Fatalf("Run = %s, %v; want SUCCESS", state, err)
	}
	// Rotation: new password in the .env is picked up and redacted.
	writeEnv("DB_PASSWORD=rotated-pw\n")
	if state, err := p.Run(context.Background(), "dumpsecret"); err != nil || state != engine.SUCCESS {
		t.Fatalf("Run after rotation = %s, %v", state, err)
	}
	dockerLog, _ := os.ReadFile(h.dockerLog)
	for _, leak := range []string{"hunter2", "rotated-pw"} {
		if strings.Contains(string(dockerLog), leak) || strings.Contains(logBuf.String(), leak) {
			t.Errorf("secret %q leaked into docker argv or log", leak)
		}
	}
	var dbDump strings.Builder
	rows, err := h.db.Query(`SELECT name, secrets_json FROM services`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var a, b string
		_ = rows.Scan(&a, &b)
		dbDump.WriteString(a + b)
	}
	rows.Close()
	if strings.Contains(dbDump.String(), "rotated-pw") || strings.Contains(dbDump.String(), "hunter2") {
		t.Error("resolved value found in the database")
	}

	// Loud failure: key removed.
	writeEnv("OTHER=1\n")
	state, err := p.Run(context.Background(), "dumpsecret")
	if state != engine.FAILED || err == nil || !strings.Contains(err.Error(), envPath) || !strings.Contains(err.Error(), "DB_PASSWORD") {
		t.Fatalf("Run = %s, %v; want FAILED naming %s and DB_PASSWORD", state, err, envPath)
	}
}

// execScriptEntryYAML is an exec service; %s is the verifyScript line
// (empty for none).
const execScriptEntryYAML = `apiVersion: ballast.dev/v1
service: myexec
version: 1
backup:
  strategy: exec
  exec:
    script: %s
%s
verify:
  method: exec-script
`

// TestRunExecScriptVerification checks the exec-script contract end to
// end: a passing verify script is green, a failing one is red, and an
// exec service without one is amber (never green).
func TestRunExecScriptVerification(t *testing.T) {
	tests := []struct {
		name        string
		verifyBody  string // "" = no verifyScript
		wantState   engine.State
		wantRecord  bool
		wantPassing bool
	}{
		{"passing verify script", "#!/bin/sh\ntest -s \"$BALLAST_ARTIFACT_DIR/out.txt\"\n", engine.SUCCESS, true, true},
		{"failing verify script", "#!/bin/sh\nexit 1\n", engine.FAILED, true, false},
		{"no verify script", "", engine.SUCCESS_UNVERIFIED, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			if err := inventory.PutService(h.db, inventory.Service{Name: "myexec", CatalogEntry: "myexec", Destination: "local"}); err != nil {
				t.Fatalf("PutService: %v", err)
			}
			backup := writeStub(t, "backup.sh", "#!/bin/sh\necho data > \"$BALLAST_ARTIFACT_DIR/out.txt\"\n")
			verifyLine := ""
			if tt.verifyBody != "" {
				verifyLine = "    verifyScript: " + writeStub(t, "verify.sh", tt.verifyBody)
			}
			p := h.pipeline()
			p.Catalog = registryWith(t, map[string]string{"myexec.yaml": fmt.Sprintf(execScriptEntryYAML, backup, verifyLine)})

			state, _ := p.Run(context.Background(), "myexec")
			if state != tt.wantState {
				t.Fatalf("state = %s, want %s", state, tt.wantState)
			}
			runs, err := engine.RecentRuns(h.db, "myexec", 1)
			if err != nil || len(runs) != 1 {
				t.Fatalf("RecentRuns: %v, %v", runs, err)
			}
			if seq := transitionsOf(t, h.db, runs[0].ID); !containsInOrder(seq, string(engine.VERIFYING), string(tt.wantState)) {
				t.Errorf("transitions = %v, want VERIFYING then %s", seq, tt.wantState)
			}
			results, err := engine.VerificationResults(h.db, runs[0].ID)
			if err != nil {
				t.Fatalf("VerificationResults: %v", err)
			}
			var found bool
			for _, r := range results {
				if r.Method == "exec-script" {
					found = true
					if r.Passed != tt.wantPassing {
						t.Errorf("exec-script Passed = %v, want %v (%s)", r.Passed, tt.wantPassing, r.Detail)
					}
				}
			}
			if found != tt.wantRecord {
				t.Errorf("exec-script record present = %v, want %v", found, tt.wantRecord)
			}
		})
	}
}

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	bundledcatalog "github.com/springledev/ballast/catalog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/springledev/ballast/internal/backend"
	"github.com/springledev/ballast/internal/catalog"
	"github.com/springledev/ballast/internal/engine"
	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/notify"
	"github.com/springledev/ballast/internal/restic"
	"github.com/springledev/ballast/internal/scheduler"
	"github.com/springledev/ballast/internal/store"
	"github.com/springledev/keel/runner"
	secretpkg "github.com/springledev/keel/secret"
)

// resticStubScript is a minimal, hermetic fake restic CLI shared by
// every Ops test that needs a repository: init/backup/snapshots/ls/
// dump/forget all succeed against one fixed fake snapshot.
const resticStubScript = `#!/bin/sh
printf '%s\n' "$*" >> "$STUB_LOG"
cmd="$1"
if [ -n "$STUB_FAIL_CMD" ] && [ "$cmd" = "$STUB_FAIL_CMD" ]; then
  echo "stub: simulated restic $cmd failure" >&2
  exit "${STUB_FAIL_CODE:-1}"
fi
# STUB_FAIL_CMD2/STUB_FAIL_CODE2 is a second, independent fail rule
# (some tests need two different commands to fail differently, e.g.
# "snapshots" reporting the repo missing while "init" itself fails).
if [ -n "$STUB_FAIL_CMD2" ] && [ "$cmd" = "$STUB_FAIL_CMD2" ]; then
  echo "stub: simulated restic $cmd failure (2)" >&2
  exit "${STUB_FAIL_CODE2:-1}"
fi
# STUB_SNAPSHOTS_MISSING_COUNT makes the first N "snapshots" calls
# report "no repository here yet" (exit 10) and every call after that
# succeed normally, so a test can tell an initial probe from a later
# re-probe apart without a second binary.
if [ "$cmd" = "snapshots" ] && [ -n "$STUB_SNAPSHOTS_MISSING_COUNT" ]; then
  count_file="$STUB_LOG.snapshots_count"
  n=0
  [ -f "$count_file" ] && n=$(cat "$count_file")
  n=$((n + 1))
  echo "$n" > "$count_file"
  if [ "$n" -le "$STUB_SNAPSHOTS_MISSING_COUNT" ]; then
    echo "stub: simulated missing repository (snapshots call $n)" >&2
    exit 10
  fi
fi
# STUB_INIT_PASSWORD_LOG records the password an "init" call was given
# (read via RESTIC_PASSWORD_FILE, exactly as restic itself would), so
# a test can confirm which generated password a successful init
# actually used.
if [ "$cmd" = "init" ] && [ -n "$STUB_INIT_PASSWORD_LOG" ] && [ -n "$RESTIC_PASSWORD_FILE" ]; then
  cat "$RESTIC_PASSWORD_FILE" >> "$STUB_INIT_PASSWORD_LOG"
fi
case "$cmd" in
  init) exit 0 ;;
  backup)
    printf '{"message_type":"summary","snapshot_id":"1111111111111111111111111111111111111111111111111111111111111111","files_new":1,"total_files_processed":1}\n'
    ;;
  snapshots)
    printf '[{"time":"2024-01-01T00:00:00Z","id":"1111111111111111111111111111111111111111111111111111111111111111","short_id":"11111111","hostname":"test","tags":["ballast","ballast-service:svc"],"paths":["/stub/data"]}]\n'
    ;;
  ls)
    printf '{"message_type":"snapshot","time":"2024-01-01T00:00:00Z","id":"1111111111111111111111111111111111111111111111111111111111111111","short_id":"11111111","hostname":"test","paths":["/stub/data"]}\n'
    printf '{"message_type":"node","name":"sample.txt","type":"file","path":"/stub/data/sample.txt","size":11}\n'
    ;;
  dump) printf 'sample-content' ;;
  restore) exit 0 ;;
  forget) printf '[]' ;;
  version) printf 'restic %s compiled with go1.22 on linux/amd64\n' "${STUB_VERSION:-0.17.3}" ;;
  *) echo "unknown stub restic command: $cmd" >&2; exit 2 ;;
esac
exit 0
`

func writeResticStub(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "restic-stub")
	if err := os.WriteFile(path, []byte(resticStubScript), 0o755); err != nil {
		t.Fatalf("writing restic stub: %v", err)
	}
	return path
}

// resticInvocations returns the restic argv lines the stub recorded
// for this harness, one call per line (see the Repo closure's
// STUB_LOG wiring in newHarness).
func (h *harness) resticInvocations(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(h.credDir, "restic.log"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("reading restic invocation log: %v", err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

// spyRunner is a stub scheduler.Runner recording every service run.
type spyRunner struct {
	mu    sync.Mutex
	ran   []string
	state engine.State
	err   error
	block chan struct{} // if set, Run blocks until this is closed
}

func (r *spyRunner) Run(_ context.Context, service string) (engine.State, error) {
	r.mu.Lock()
	r.ran = append(r.ran, service)
	block := r.block
	r.mu.Unlock()
	if block != nil {
		<-block
	}
	state := r.state
	if state == "" {
		state = engine.SUCCESS
	}
	return state, r.err
}

const folderEntryYAML = `
apiVersion: ballast.dev/v1
service: folder
version: 1
parameters:
  - name: path
    required: true
backup:
  strategy: files
  files:
    - path: "{{ .Params.path }}"
verify:
  method: archive-integrity
`

// harness bundles a fresh Ops with a stub restic binary and stub
// scheduler.Runner for table-driven Ops tests.
type harness struct {
	t            *testing.T
	ops          *Ops
	runner       *spyRunner
	resticBin    string
	credDir      string
	locks        scheduler.FileLocker
	triggerToken string // scratch field http_test.go's golden cases stash a generated token in
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "ballast.db"))
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	resticBin := writeResticStub(t)
	credDir := t.TempDir()

	fsys := fstest.MapFS{"folder.yaml": &fstest.MapFile{Data: []byte(folderEntryYAML)}}
	reg, err := catalog.LoadRegistry(fsys, nil)
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}

	r := &spyRunner{}
	locks := scheduler.FileLocker{Dir: filepath.Join(t.TempDir(), "locks")}
	coord := &scheduler.Coordinator{DB: s.DB(), Locks: locks, Runner: r, LockWait: -1}

	ops := &Ops{
		DB:      s.DB(),
		Catalog: reg,
		Coord:   coord,
		Repo: func(dest inventory.Destination) (*restic.Repository, error) {
			return restic.Open(dest.Location, restic.Options{
				Runner:       &runner.Runner{BaseEnv: append(os.Environ(), "STUB_LOG="+filepath.Join(credDir, "restic.log"))},
				PasswordFile: filepath.Join(credDir, dest.PasswordCredential),
				ResticBin:    resticBin,
			})
		},
		CredentialsDir: credDir,
	}
	return &harness{t: t, ops: ops, runner: r, resticBin: resticBin, credDir: credDir, locks: locks}
}

// dockerPsStub is a fake `docker ps` answering with one fixed running
// container, so DetectServices tests are hermetic (no Docker daemon
// needed).
const dockerPsStub = "#!/bin/sh\n" +
	`if [ "$1" = "ps" ]; then printf 'immich_postgres\tghcr.io/immich-app/postgres:14\n'; fi` + "\n" +
	"exit 0\n"

// withStubDocker points h.ops.Runner at a fake `docker` binary on
// $PATH, for DetectServices golden cases.
func (h *harness) withStubDocker(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "docker")
	if err := os.WriteFile(path, []byte(dockerPsStub), 0o755); err != nil {
		t.Fatalf("writing docker stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	h.ops.Runner = &runner.Runner{}
}

// holdRunLock simulates a live run of service, so a Start/Backup call
// refuses with scheduler.ErrAlreadyRunning.
func (h *harness) holdRunLock(t *testing.T, service string) {
	t.Helper()
	release, ok, err := h.locks.TryLock(service)
	if err != nil || !ok {
		t.Fatalf("holding run lock for %s: %v, %v", service, ok, err)
	}
	t.Cleanup(release)
}

// addDestination configures a destination "local" with a working
// password credential file already in place.
func (h *harness) addDestination(name string) {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(h.credDir, "ballast-restic-password."+name), []byte("pw\n"), 0o600); err != nil {
		h.t.Fatal(err)
	}
	if err := inventory.PutDestination(h.ops.DB, inventory.Destination{
		Name: name, Location: "/stub-repo", KeepDaily: 7, KeepWeekly: 4, KeepMonthly: 6,
	}); err != nil {
		h.t.Fatalf("PutDestination: %v", err)
	}
}

func TestOpsPutServiceValidatesCatalog(t *testing.T) {
	h := newHarness(t)
	h.addDestination("local")

	tests := []struct {
		name    string
		svc     inventory.Service
		wantErr string
	}{
		{"unknown catalog entry", inventory.Service{Name: "svc", CatalogEntry: "ghost", Destination: "local"}, "catalog entry"},
		{"missing required param", inventory.Service{Name: "svc", CatalogEntry: "folder", Destination: "local"}, "missing required parameter"},
		{"unknown destination", inventory.Service{Name: "svc", CatalogEntry: "folder", Destination: "nope", Params: map[string]string{"path": "/x"}}, ""},
		{"ok", inventory.Service{Name: "svc", CatalogEntry: "folder", Destination: "local", Params: map[string]string{"path": "/x"}}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := h.ops.PutService(context.Background(), tt.svc)
			if tt.name == "ok" {
				if err != nil {
					t.Fatalf("PutService: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("PutService: want an error")
			}
			if tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestOpsServiceCRUD(t *testing.T) {
	h := newHarness(t)
	h.addDestination("local")
	svc := inventory.Service{Name: "svc", CatalogEntry: "folder", Destination: "local", Params: map[string]string{"path": "/x"}}
	if err := h.ops.PutService(context.Background(), svc); err != nil {
		t.Fatalf("PutService: %v", err)
	}
	list, err := h.ops.ListServices(context.Background())
	if err != nil || len(list) != 1 {
		t.Fatalf("ListServices: %v, %v", list, err)
	}
	got, err := h.ops.GetService(context.Background(), "svc")
	if err != nil || got.Name != "svc" {
		t.Fatalf("GetService: %v, %v", got, err)
	}
	if err := h.ops.DeleteService(context.Background(), "svc"); err != nil {
		t.Fatalf("DeleteService: %v", err)
	}
	if _, err := h.ops.GetService(context.Background(), "svc"); !errors.Is(err, inventory.ErrNotFound) {
		t.Errorf("GetService after delete = %v, want ErrNotFound", err)
	}
}

// TestOpsSetSecret checks the spec §14 step 15 credential store path:
// a first call defaults the credential file name, writes it 0600 into
// CredentialsDir, records the name (never the value) against the
// service, and redacts the value; a second call rotates the value in
// place (no O_EXCL — unlike a destination's generated repo password,
// this is a user-supplied secret the operator may legitimately
// change); an explicit credential name is honoured instead of the
// default.
func TestOpsSetSecret(t *testing.T) {
	h := newHarness(t)
	h.addDestination("local")
	svc := inventory.Service{Name: "svc", CatalogEntry: "folder", Destination: "local", Params: map[string]string{"path": "/x"}}
	if err := h.ops.PutService(context.Background(), svc); err != nil {
		t.Fatalf("PutService: %v", err)
	}
	h.ops.Redact = secretpkg.NewSet()

	if err := h.ops.SetSecret(context.Background(), "svc", "db_password", "hunter2"); err != nil {
		t.Fatalf("SetSecret: %v", err)
	}
	got, err := h.ops.GetService(context.Background(), "svc")
	if err != nil {
		t.Fatalf("GetService: %v", err)
	}
	credName := got.Secrets["db_password"]
	if credName != "ballast-secret.svc.db_password" {
		t.Errorf("Secrets[db_password] = %q, want the default name", credName)
	}
	path := filepath.Join(h.credDir, credName)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat secret file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("secret file permissions = %04o, want 0600", perm)
	}
	data, err := os.ReadFile(path)
	if err != nil || strings.TrimSpace(string(data)) != "hunter2" {
		t.Errorf("secret file contents = %q, %v, want hunter2", data, err)
	}
	if !strings.Contains(h.ops.Redact.Redact("password is hunter2"), "[redacted]") {
		t.Error("SetSecret did not register the value with Ops.Redact")
	}

	// Rotation: same name, new value, file overwritten in place.
	if err := h.ops.SetSecret(context.Background(), "svc", "db_password", "new-value"); err != nil {
		t.Fatalf("SetSecret (rotate): %v", err)
	}
	data, err = os.ReadFile(path)
	if err != nil || strings.TrimSpace(string(data)) != "new-value" {
		t.Errorf("secret file after rotation = %q, %v, want new-value", data, err)
	}
	got, err = h.ops.GetService(context.Background(), "svc")
	if err != nil || got.Secrets["db_password"] != credName {
		t.Errorf("credential file name changed across rotation: %v (%v)", got.Secrets, err)
	}

	if err := h.ops.SetSecret(context.Background(), "svc", "api_key", ""); err == nil {
		t.Error("SetSecret(empty value) = nil error, want error")
	}
	if err := h.ops.SetSecret(context.Background(), "nope", "x", "y"); !errors.Is(err, inventory.ErrNotFound) {
		t.Errorf("SetSecret(unknown service) = %v, want ErrNotFound", err)
	}
}

// TestOpsSetSecretNeverShadowsOperatorCredential checks that SetSecret
// refuses to write a credential file when one of the same name already
// exists in a different, higher-priority search directory (e.g. an
// operator's systemd LoadCredential= file): FindCredentialDir/
// resolveSecrets would keep resolving to the operator's file regardless
// of what Ballast wrote, so silently accepting the value here would
// look like it worked while having no effect on the next run.
// TestOpsSetSecretTightensExistingFileMode checks that rotating a
// secret whose file was loosened on disk leaves it owner-only again.
func TestOpsSetSecretTightensExistingFileMode(t *testing.T) {
	h := newHarness(t)
	h.addDestination("local")
	svc := inventory.Service{Name: "svc", CatalogEntry: "folder", Destination: "local", Params: map[string]string{"path": "/x"}}
	if err := h.ops.PutService(context.Background(), svc); err != nil {
		t.Fatalf("PutService: %v", err)
	}
	path := filepath.Join(h.credDir, "ballast-secret.svc.db_password")
	if err := os.WriteFile(path, []byte("old\n"), 0o644); err != nil {
		t.Fatalf("seeding loose secret file: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := h.ops.SetSecret(context.Background(), "svc", "db_password", "hunter2"); err != nil {
		t.Fatalf("SetSecret: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 600", info.Mode().Perm())
	}
}

func TestOpsSetSecretNeverShadowsOperatorCredential(t *testing.T) {
	h := newHarness(t)
	h.addDestination("local")
	svc := inventory.Service{Name: "svc", CatalogEntry: "folder", Destination: "local", Params: map[string]string{"path": "/x"}}
	if err := h.ops.PutService(context.Background(), svc); err != nil {
		t.Fatalf("PutService: %v", err)
	}

	operatorDir := t.TempDir()
	operatorFile := filepath.Join(operatorDir, "ballast-secret.svc.db_password")
	if err := os.WriteFile(operatorFile, []byte("operator-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.ops.CredentialSearchDirs = []string{operatorDir, h.credDir}

	err := h.ops.SetSecret(context.Background(), "svc", "db_password", "hunter2")
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("SetSecret error = %v, want a refusal naming the existing operator credential", err)
	}
	if _, err := os.Stat(filepath.Join(h.credDir, "ballast-secret.svc.db_password")); err == nil {
		t.Error("SetSecret wrote a shadow file into the writable credential dir despite refusing")
	}
	data, err := os.ReadFile(operatorFile)
	if err != nil || strings.TrimSpace(string(data)) != "operator-value" {
		t.Errorf("operator credential file changed: %q, %v", data, err)
	}
}

func TestOpsDestinationCRUDAndInit(t *testing.T) {
	h := newHarness(t)
	if err := os.WriteFile(filepath.Join(h.credDir, "ballast-restic-password.local"), []byte("pw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := h.ops.PutDestination(context.Background(), inventory.Destination{
		Name: "local", Location: "/stub-repo", KeepDaily: 7,
	}, true /* init */)
	if err != nil {
		t.Fatalf("PutDestination with init: %v", err)
	}

	if err := h.ops.TestDestination(context.Background(), "local"); err != nil {
		t.Fatalf("TestDestination: %v", err)
	}

	list, err := h.ops.ListDestinations(context.Background())
	if err != nil || len(list) != 1 {
		t.Fatalf("ListDestinations: %v, %v", list, err)
	}

	if err := h.ops.DeleteDestination(context.Background(), "local"); err != nil {
		t.Fatalf("DeleteDestination: %v", err)
	}
}

// TestOpsPutDestinationInitIsIdempotent is the regression test for a
// real bug: a destination created without checking "init" (or a GUI
// resubmission of one that already exists) must never leave the
// repository un-initialized only to fail every later backup with a
// raw "repository does not exist" error, and re-running init on an
// already-initialized repository must not itself become a new error.
func TestOpsPutDestinationInitIsIdempotent(t *testing.T) {
	h := newHarness(t)
	if err := os.WriteFile(filepath.Join(h.credDir, "ballast-restic-password.local"), []byte("pw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dest := inventory.Destination{Name: "local", Location: "/stub-repo", PasswordCredential: "ballast-restic-password.local", KeepDaily: 7}

	// First init: the repo does not exist yet (simulated exit 10 on
	// the probing Snapshots call) — Init must actually run.
	t.Setenv("STUB_FAIL_CMD", "snapshots")
	t.Setenv("STUB_FAIL_CODE", "10")
	if err := h.ops.PutDestination(context.Background(), dest, true); err != nil {
		t.Fatalf("PutDestination (first init): %v", err)
	}
	inv := h.resticInvocations(t)
	if !containsPrefix(inv, "init") {
		t.Errorf("invocations = %v, want an init call when the repository did not exist", inv)
	}

	// Second init: the repo now "exists" (stub's snapshots succeeds by
	// default) — Init must be skipped, not attempted a second time.
	t.Setenv("STUB_FAIL_CMD", "")
	t.Setenv("STUB_FAIL_CODE", "")
	if err := h.ops.PutDestination(context.Background(), dest, true); err != nil {
		t.Fatalf("PutDestination (second init): %v", err)
	}
	inv2 := h.resticInvocations(t)[len(inv):]
	if containsPrefix(inv2, "init") {
		t.Errorf("invocations after second init = %v, want no init call (repository already exists)", inv2)
	}
}

func containsPrefix(lines []string, prefix string) bool {
	for _, l := range lines {
		if strings.HasPrefix(l, prefix) {
			return true
		}
	}
	return false
}

// TestOpsPutDestinationGeneratesLocalPassword: a brand-new local-disk
// destination with no pre-existing credential file gets one generated
// automatically (spec §5.3.6: never ask a beginner to invent or paste
// a secret for the common local-folder case) — and a second Init call
// against the same destination must not clobber it.
func TestOpsPutDestinationGeneratesLocalPassword(t *testing.T) {
	h := newHarness(t)
	credPath := filepath.Join(h.credDir, "ballast-restic-password.local")
	if _, err := os.Stat(credPath); !os.IsNotExist(err) {
		t.Fatalf("credential file already exists before the test: %v", err)
	}

	dest := inventory.Destination{Name: "local", Location: "/stub-repo", KeepDaily: 7}
	if err := h.ops.PutDestination(context.Background(), dest, true /* init */); err != nil {
		t.Fatalf("PutDestination with init: %v", err)
	}

	first, err := os.ReadFile(credPath)
	if err != nil {
		t.Fatalf("reading generated credential: %v", err)
	}
	if len(strings.TrimSpace(string(first))) == 0 {
		t.Fatal("generated repository password is empty")
	}

	if err := h.ops.PutDestination(context.Background(), dest, true /* init */); err != nil {
		t.Fatalf("PutDestination (second init): %v", err)
	}
	second, err := os.ReadFile(credPath)
	if err != nil {
		t.Fatalf("reading credential after second init: %v", err)
	}
	if string(first) != string(second) {
		t.Error("a second init overwrote the already-generated repository password")
	}
}

// TestOpsPutDestinationNeverGeneratesForRemoteBackend: a non-local
// destination (a restic backend URL, e.g. s3:...) must still require
// its own pre-existing credential file — auto-generating a password
// there would create a fresh, wrong-password repository instead of
// using the operator's real provider credentials.
func TestOpsPutDestinationNeverGeneratesForRemoteBackend(t *testing.T) {
	h := newHarness(t)
	dest := inventory.Destination{Name: "remote", Location: "s3:https://example.com/bucket", PasswordCredential: "ballast-restic-password.remote"}
	err := h.ops.PutDestination(context.Background(), dest, true /* init */)
	if err == nil {
		t.Fatal("PutDestination for a remote backend with no credential file: want an error")
	}
	if _, statErr := os.Stat(filepath.Join(h.credDir, "ballast-restic-password.remote")); !os.IsNotExist(statErr) {
		t.Error("a credential file was created for a remote backend; it must not be")
	}
}

// TestOpsPutDestinationGeneratedPasswordIsPrivateAndRedacted: the
// generated password file is owner-only (spec rule 2) and the password
// is redacted from Ops errors from the moment it exists.
func TestOpsPutDestinationGeneratedPasswordIsPrivateAndRedacted(t *testing.T) {
	h := newHarness(t)
	h.ops.Redact = secretpkg.NewSet()
	t.Setenv("STUB_FAIL_CMD", "snapshots")
	t.Setenv("STUB_FAIL_CODE", "10")
	dest := inventory.Destination{Name: "local", Location: "/stub-repo", KeepDaily: 7}
	if err := h.ops.PutDestination(context.Background(), dest, true); err != nil {
		t.Fatalf("PutDestination: %v", err)
	}

	path := filepath.Join(h.credDir, "ballast-restic-password.local")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat generated password: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("generated password file mode = %o, want 600", perm)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pw := strings.TrimSpace(string(data))
	if got := h.ops.redact("leaked " + pw); strings.Contains(got, pw) {
		t.Errorf("redact(%q) = %q, want the generated password redacted", "leaked "+pw, got)
	}
}

// TestOpsPutDestinationInitFailureCleansUp: when setting up the
// repository fails, nothing half-made is left behind — a new
// destination is removed again, so a retry never silently reuses a
// destination row from a setup that never finished. An existing
// destination is kept.
//
// A generated password file's fate depends on whether a repository
// might now actually exist at the location (review finding B): the
// first three cases below all have a persistent, ambiguous restic
// failure (wrong password, or a plain unexplained error) that reads
// exactly the same on cleanup's re-probe as it did on the first
// attempt, so nothing here can tell "definitely no repository" from
// "maybe one now exists" — cleanup must keep the file. Only the last
// case, where the probe can positively confirm the repository is
// still missing, gets the file removed.
func TestOpsPutDestinationInitFailureCleansUp(t *testing.T) {
	tests := []struct {
		name                  string
		existing              bool   // destination row already configured before the call
		failCode              string // restic exit code for every "snapshots" call
		failCmd2, failCode2   string // a second, independent fail rule (e.g. "init" itself failing)
		snapshotsMissingCount string // STUB_SNAPSHOTS_MISSING_COUNT: first N snapshots calls report "missing"
		wantErrText           string
		wantKept              bool // generated password file must still exist afterward
		wantRowAfter          bool
	}{
		{
			// The generated password never opened that repository, so
			// it protects nothing: removed, so a retry asks again.
			name:        "wrong password: a repository already exists here",
			failCode:    "12",
			wantErrText: "Ballast does not have its password",
			wantKept:    false,
		},
		{
			name:        "unexplained failure on a new destination",
			failCode:    "1",
			wantErrText: "setting up the backup repository",
			wantKept:    true,
		},
		{
			name:         "unexplained failure on an existing destination keeps the row too",
			existing:     true,
			failCode:     "1",
			wantErrText:  "setting up the backup repository",
			wantKept:     true,
			wantRowAfter: true,
		},
		{
			name:                  "repository confirmed still missing after a failed init",
			snapshotsMissingCount: "99", // every snapshots call, before and after, reports "missing"
			failCmd2:              "init",
			failCode2:             "1", // init itself fails (simulates being killed mid-run)
			wantErrText:           "setting up the backup repository",
			wantKept:              false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			dest := inventory.Destination{Name: "local", Location: "/stub-repo", KeepDaily: 7}
			if tt.existing {
				if err := inventory.PutDestination(h.ops.DB, dest); err != nil {
					t.Fatal(err)
				}
			}
			if tt.failCode != "" {
				t.Setenv("STUB_FAIL_CMD", "snapshots")
				t.Setenv("STUB_FAIL_CODE", tt.failCode)
			}
			if tt.failCmd2 != "" {
				t.Setenv("STUB_FAIL_CMD2", tt.failCmd2)
				t.Setenv("STUB_FAIL_CODE2", tt.failCode2)
			}
			if tt.snapshotsMissingCount != "" {
				t.Setenv("STUB_SNAPSHOTS_MISSING_COUNT", tt.snapshotsMissingCount)
			}

			err := h.ops.PutDestination(context.Background(), dest, true)
			if err == nil {
				t.Fatal("PutDestination = nil, want the setup failure")
			}
			if !strings.Contains(err.Error(), tt.wantErrText) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantErrText)
			}
			_, statErr := os.Stat(filepath.Join(h.credDir, "ballast-restic-password.local"))
			gotKept := statErr == nil
			if gotKept != tt.wantKept {
				t.Errorf("generated password file present after failure = %v (stat err %v), want %v", gotKept, statErr, tt.wantKept)
			}
			if tt.wantKept && !strings.Contains(err.Error(), "was kept because a repository may already have been created") {
				t.Errorf("error = %q, want it to explain the kept password file", err)
			}
			_, getErr := inventory.GetDestination(h.ops.DB, "local")
			if gotRow := getErr == nil; gotRow != tt.wantRowAfter {
				t.Errorf("destination row present after failure = %v, want %v", gotRow, tt.wantRowAfter)
			}
		})
	}
}

// TestOpsEnsureLocalPasswordNeverOverwrites: if the credential file
// already exists — created by an operator, or by a concurrent
// PutDestination call that won a creation race — ensurePassword
// must report "nothing generated" and leave its contents untouched,
// never silently replace it (review finding A: a TOCTOU stat-then-
// write could otherwise clobber a password restic init already used).
func TestOpsEnsureLocalPasswordNeverOverwrites(t *testing.T) {
	h := newHarness(t)
	dest := inventory.Destination{Name: "local", Location: "/stub-repo", PasswordCredential: "ballast-restic-password.local"}
	path := filepath.Join(h.credDir, dest.PasswordCredential)
	const original = "already-here-password\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := h.ops.ensurePassword(dest)
	if err != nil {
		t.Fatalf("ensurePassword: %v", err)
	}
	if got != "" {
		t.Errorf("ensurePassword returned path %q, want \"\" (nothing generated) for a pre-existing file", got)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != original {
		t.Errorf("password file content = %q, want it unchanged (%q)", after, original)
	}
}

// TestOpsPutDestinationConcurrentInitIsSerialized: several goroutines
// racing to PutDestination(init=true) the same brand-new local
// destination (a GUI double-click, or an API client retrying) must
// behave as if they ran one at a time — review finding A. Only one
// password can ever be generated and used to initialize the
// repository; nothing overwrites it, and no cleanup from one call can
// delete state another call already relies on.
// TestOpsPutDestinationWaitsForOtherProcess: with DestinationLocks
// set, PutDestination waits while another holder (standing in for a
// second Ballast process: flock locks are per open file, so a separate
// FileLocker in this process behaves the same) sets up the same
// destination, gives up cleanly if ctx ends first — touching nothing —
// and goes ahead once the lock is free.
func TestOpsPutDestinationWaitsForOtherProcess(t *testing.T) {
	h := newHarness(t)
	lockDir := filepath.Join(t.TempDir(), "locks")
	h.ops.DestinationLocks = scheduler.FileLocker{Dir: lockDir}
	dest := inventory.Destination{Name: "local", Location: "/stub-repo", KeepDaily: 7}

	release, ok, err := scheduler.FileLocker{Dir: lockDir}.TryLock("local")
	if err != nil || !ok {
		t.Fatalf("taking the other process's lock: ok=%v err=%v", ok, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err = h.ops.PutDestination(ctx, dest, true /* init */)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("PutDestination while locked elsewhere: err = %v, want context.DeadlineExceeded", err)
	}
	if !strings.Contains(err.Error(), "another Ballast command") {
		t.Errorf("error should say why it waited: %v", err)
	}
	if _, err := inventory.GetDestination(h.ops.DB, "local"); !errors.Is(err, inventory.ErrNotFound) {
		t.Errorf("GetDestination after giving up: err = %v, want ErrNotFound (nothing may be written)", err)
	}
	if _, err := os.Stat(filepath.Join(h.credDir, "ballast-restic-password.local")); !os.IsNotExist(err) {
		t.Errorf("a password file was generated while another process held the lock (stat err = %v)", err)
	}

	done := make(chan error, 1)
	go func() { done <- h.ops.PutDestination(context.Background(), dest, true /* init */) }()
	select {
	case err := <-done:
		t.Fatalf("PutDestination returned (%v) while the other process still held the lock", err)
	case <-time.After(200 * time.Millisecond):
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("PutDestination after the lock was released: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("PutDestination still waiting after the lock was released")
	}
	if _, err := inventory.GetDestination(h.ops.DB, "local"); err != nil {
		t.Errorf("GetDestination after setup: %v", err)
	}
}

func TestOpsPutDestinationConcurrentInitIsSerialized(t *testing.T) {
	h := newHarness(t)
	initPasswordLog := filepath.Join(h.credDir, "init_password.log")
	t.Setenv("STUB_INIT_PASSWORD_LOG", initPasswordLog)
	// The repository doesn't exist yet: only the very first probe
	// should report that, so exactly one goroutine calls Init and
	// every later one (its own, or another goroutine's) finds the
	// repository already there, just like a real restic backend.
	t.Setenv("STUB_SNAPSHOTS_MISSING_COUNT", "1")
	dest := inventory.Destination{Name: "local", Location: "/stub-repo", KeepDaily: 7}

	const n = 5
	errs := make([]error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			errs[i] = h.ops.PutDestination(context.Background(), dest, true /* init */)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: PutDestination: %v", i, err)
		}
	}

	credPath := filepath.Join(h.credDir, "ballast-restic-password.local")
	content, err := os.ReadFile(credPath)
	if err != nil {
		t.Fatalf("reading generated credential: %v", err)
	}
	pw := strings.TrimSpace(string(content))
	if pw == "" {
		t.Fatal("generated repository password is empty")
	}

	// Exactly one restic init call must have happened — the lock
	// makes every call but the first find the repository already
	// initialized (see TestOpsPutDestinationInitIsIdempotent) — and
	// it must have been given the password that ended up on disk, not
	// one a later, discarded generation attempt produced.
	initPasswords, err := os.ReadFile(initPasswordLog)
	if err != nil {
		t.Fatalf("reading init password log: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(initPasswords), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("restic init was called %d times, want exactly 1 (log: %q)", len(lines), initPasswords)
	}
	if strings.TrimSpace(lines[0]) != pw {
		t.Errorf("init was given password %q, but the file on disk holds %q", strings.TrimSpace(lines[0]), pw)
	}

	if _, err := inventory.GetDestination(h.ops.DB, "local"); err != nil {
		t.Errorf("GetDestination after concurrent init: %v", err)
	}
}

// TestOpsPutDestinationTooOldRestic: restic before 0.17 cannot say "no
// repository here yet", so its generic failure must be explained as
// "update restic", not surfaced raw.
func TestOpsPutDestinationTooOldRestic(t *testing.T) {
	h := newHarness(t)
	t.Setenv("STUB_FAIL_CMD", "snapshots")
	t.Setenv("STUB_FAIL_CODE", "1")
	t.Setenv("STUB_VERSION", "0.16.4")
	err := h.ops.PutDestination(context.Background(), inventory.Destination{Name: "local", Location: "/stub-repo", KeepDaily: 7}, true)
	if err == nil || !strings.Contains(err.Error(), "restic 0.16.4 is too old") {
		t.Errorf("PutDestination error = %v, want it to say restic 0.16.4 is too old", err)
	}
}

func TestOpsNotificationSettingsErrors(t *testing.T) {
	tests := []struct {
		name           string
		service        string
		dropTable      bool
		maxAge         int
		wantNotFound   bool
		wantValidation bool
	}{
		{name: "unknown service", service: "nope", wantNotFound: true},
		{name: "non-positive max age", service: "svc", maxAge: -1, wantValidation: true},
		{name: "storage failure is not a validation error", service: "svc", dropTable: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			withService(t, h)
			if tt.dropTable {
				if _, err := h.ops.DB.Exec(`DROP TABLE notification_settings`); err != nil {
					t.Fatal(err)
				}
			}
			s := notify.Settings{Service: tt.service, Enabled: true}
			if tt.maxAge != 0 {
				s.MaxAgeHours = &tt.maxAge
			}
			err := h.ops.SetNotificationSettings(context.Background(), tt.service, s)
			if err == nil {
				t.Fatal("SetNotificationSettings = nil, want an error")
			}
			if got := errors.Is(err, inventory.ErrNotFound); got != tt.wantNotFound {
				t.Errorf("errors.Is(%v, ErrNotFound) = %v, want %v", err, got, tt.wantNotFound)
			}
			if got := errors.Is(err, ErrValidation); got != tt.wantValidation {
				t.Errorf("errors.Is(%v, ErrValidation) = %v, want %v", err, got, tt.wantValidation)
			}
		})
	}
}

func TestOpsDeleteDestinationInUse(t *testing.T) {
	h := newHarness(t)
	h.addDestination("local")
	if err := h.ops.PutService(context.Background(), inventory.Service{
		Name: "svc", CatalogEntry: "folder", Destination: "local", Params: map[string]string{"path": "/x"},
	}); err != nil {
		t.Fatalf("PutService: %v", err)
	}
	if err := h.ops.DeleteDestination(context.Background(), "local"); !errors.Is(err, inventory.ErrInUse) {
		t.Errorf("DeleteDestination = %v, want ErrInUse", err)
	}
}

func TestOpsBackupAndStartBackup(t *testing.T) {
	h := newHarness(t)
	if err := h.ops.Backup(context.Background(), "svc"); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if len(h.runner.ran) != 1 || h.runner.ran[0] != "svc" {
		t.Errorf("ran = %v, want [svc]", h.runner.ran)
	}

	interrupted, err := h.ops.StartBackup(context.Background(), "other")
	if err != nil {
		t.Fatalf("StartBackup: %v", err)
	}
	if interrupted {
		t.Error("interrupted = true, want false")
	}
	h.ops.Wait()
	if len(h.runner.ran) != 2 {
		t.Errorf("ran = %v, want two runs after Wait", h.runner.ran)
	}
}

// TestOpsRedactErrPreservesErrorType: redacting a secret out of an
// error's text must not lose the original error underneath — callers
// (h.writeErr's status mapping, `errors.Is`) still need to recognize
// it, exactly like scheduler.Coordinator's own redactedError.
func TestOpsRedactErrPreservesErrorType(t *testing.T) {
	h := newHarness(t)
	const secret = "s3cret-token"
	h.ops.Redact = secretpkg.NewSet(secret)

	original := fmt.Errorf("service svc: %w: saw token %s", scheduler.ErrAlreadyRunning, secret)
	got := h.ops.redactErr(original)

	if !errors.Is(got, scheduler.ErrAlreadyRunning) {
		t.Errorf("redactErr(%v) = %v, lost errors.Is(scheduler.ErrAlreadyRunning)", original, got)
	}
	if strings.Contains(got.Error(), secret) {
		t.Errorf("redactErr(%v).Error() = %q, still leaks the secret", original, got.Error())
	}
}

func TestOpsStartBackupAlreadyRunning(t *testing.T) {
	h := newHarness(t)
	h.runner.block = make(chan struct{})
	defer close(h.runner.block)

	_, err := h.ops.StartBackup(context.Background(), "svc")
	if err != nil {
		t.Fatalf("first StartBackup: %v", err)
	}
	// Give the goroutine a moment to actually take the lock.
	deadline := time.Now().Add(2 * time.Second)
	for {
		h.runner.mu.Lock()
		n := len(h.runner.ran)
		h.runner.mu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	_, err = h.ops.StartBackup(context.Background(), "svc")
	if !errors.Is(err, scheduler.ErrAlreadyRunning) {
		t.Errorf("second StartBackup = %v, want ErrAlreadyRunning", err)
	}
}

func TestOpsSnapshotsAndRestore(t *testing.T) {
	h := newHarness(t)
	h.addDestination("local")
	if err := inventory.PutService(h.ops.DB, inventory.Service{Name: "svc", CatalogEntry: "folder", Destination: "local"}); err != nil {
		t.Fatalf("PutService: %v", err)
	}

	snaps, err := h.ops.Snapshots(context.Background(), "svc")
	if err != nil {
		t.Fatalf("Snapshots: %v", err)
	}
	if len(snaps) != 1 {
		t.Fatalf("snapshots = %v, want 1", snaps)
	}

	to := filepath.Join(t.TempDir(), "restore-out")
	if err := h.ops.Restore(context.Background(), RestoreRequest{Service: "svc", To: to}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
}

func TestOpsRestoreRejectsNonEmptyTarget(t *testing.T) {
	h := newHarness(t)
	h.addDestination("local")
	if err := inventory.PutService(h.ops.DB, inventory.Service{Name: "svc", CatalogEntry: "folder", Destination: "local"}); err != nil {
		t.Fatalf("PutService: %v", err)
	}
	to := t.TempDir()
	if err := os.WriteFile(filepath.Join(to, "existing"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := h.ops.Restore(context.Background(), RestoreRequest{Service: "svc", To: to})
	if err == nil || !errors.Is(err, ErrValidation) {
		t.Fatalf("Restore into non-empty dir = %v, want ErrValidation", err)
	}
}

func TestOpsRestoreRequiresTo(t *testing.T) {
	h := newHarness(t)
	err := h.ops.Restore(context.Background(), RestoreRequest{Service: "svc"})
	if !errors.Is(err, ErrValidation) {
		t.Errorf("Restore without --to = %v, want ErrValidation", err)
	}
}

func TestOpsRestoreCrossServiceSnapshotRejected(t *testing.T) {
	h := newHarness(t)
	h.addDestination("local")
	if err := inventory.PutService(h.ops.DB, inventory.Service{Name: "svc", CatalogEntry: "folder", Destination: "local"}); err != nil {
		t.Fatalf("PutService: %v", err)
	}
	to := filepath.Join(t.TempDir(), "out")
	err := h.ops.Restore(context.Background(), RestoreRequest{Service: "svc", Snapshot: "deadbeef", To: to})
	if err == nil || !errors.Is(err, ErrValidation) {
		t.Fatalf("Restore with an unknown snapshot id = %v, want ErrValidation", err)
	}
}

func TestOpsSchedules(t *testing.T) {
	h := newHarness(t)
	if err := h.ops.SetSchedule(context.Background(), "svc", "not a cron"); !errors.Is(err, ErrValidation) {
		t.Errorf("SetSchedule with a bad cron = %v, want ErrValidation", err)
	}
	if err := h.ops.SetSchedule(context.Background(), "svc", "0 3 * * *"); err != nil {
		t.Fatalf("SetSchedule: %v", err)
	}
	list, err := h.ops.ListSchedules(context.Background())
	if err != nil || len(list) != 1 || list[0].Service != "svc" {
		t.Fatalf("ListSchedules: %v, %v", list, err)
	}
	if err := h.ops.SetScheduleEnabled(context.Background(), "svc", false); err != nil {
		t.Fatalf("SetScheduleEnabled: %v", err)
	}
	if err := h.ops.SetScheduleEnabled(context.Background(), "nope", false); !errors.Is(err, ErrValidation) {
		t.Errorf("SetScheduleEnabled for unknown service = %v, want ErrValidation", err)
	}
}

func TestOpsTriggerTokenGenerateAndTrigger(t *testing.T) {
	h := newHarness(t)
	token, err := h.ops.GenerateTriggerToken(context.Background(), "svc")
	if err != nil {
		t.Fatalf("GenerateTriggerToken: %v", err)
	}
	if token == "" {
		t.Fatal("token is empty")
	}
	if err := h.ops.Trigger(context.Background(), "svc", "wrong"); !errors.Is(err, scheduler.ErrInvalidTriggerToken) {
		t.Errorf("Trigger with wrong token = %v, want ErrInvalidTriggerToken", err)
	}
	if err := h.ops.Trigger(context.Background(), "svc", token); err != nil {
		t.Fatalf("Trigger with valid token: %v", err)
	}
}

// TestOpsStartTrigger: StartTrigger authenticates like Trigger but
// runs the backup in the background, tracked by Wait, exactly like
// StartBackup (HTTP use: the trigger endpoint must not hold the
// request open, nor let the request's cancellation kill the run).
func TestOpsStartTrigger(t *testing.T) {
	h := newHarness(t)
	token, err := h.ops.GenerateTriggerToken(context.Background(), "svc")
	if err != nil {
		t.Fatalf("GenerateTriggerToken: %v", err)
	}

	if _, err := h.ops.StartTrigger(context.Background(), "svc", "wrong"); !errors.Is(err, scheduler.ErrInvalidTriggerToken) {
		t.Errorf("StartTrigger with wrong token = %v, want ErrInvalidTriggerToken", err)
	}
	if len(h.runner.ran) != 0 {
		t.Fatalf("ran = %v, want no runs for a rejected token", h.runner.ran)
	}

	interrupted, err := h.ops.StartTrigger(context.Background(), "svc", token)
	if err != nil {
		t.Fatalf("StartTrigger with valid token: %v", err)
	}
	if interrupted {
		t.Error("interrupted = true, want false (no prior run)")
	}
	h.ops.Wait()
	if len(h.runner.ran) != 1 || h.runner.ran[0] != "svc" {
		t.Errorf("ran = %v, want [svc]", h.runner.ran)
	}
}

func TestOpsCheckPreflight_ResticAndDockerFound(t *testing.T) {
	tests := []struct {
		version     string
		wantErrText string // "" = restic usable
	}{
		{version: "0.17.3"},
		{version: "0.16.4", wantErrText: "too old"},
	}
	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			h := newHarness(t)
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("STUB_LOG", filepath.Join(dir, "restic.log"))
			t.Setenv("STUB_VERSION", tt.version)
			h.ops.Runner = &runner.Runner{}
			h.ops.ResticBin = h.resticBin

			res := h.ops.CheckPreflight(context.Background())
			if !res.ResticInstalled || res.ResticVersion != tt.version {
				t.Errorf("CheckPreflight restic = installed=%v version=%q, want installed %s", res.ResticInstalled, res.ResticVersion, tt.version)
			}
			if tt.wantErrText == "" && res.ResticError != "" {
				t.Errorf("ResticError = %q, want none", res.ResticError)
			}
			if tt.wantErrText != "" && !strings.Contains(res.ResticError, tt.wantErrText) {
				t.Errorf("ResticError = %q, want it to contain %q", res.ResticError, tt.wantErrText)
			}
			if !res.DockerInstalled || res.DockerError != "" {
				t.Errorf("CheckPreflight docker = installed=%v error=%q, want installed with no error", res.DockerInstalled, res.DockerError)
			}
		})
	}
}

func TestOpsCheckPreflight_ResticMissing(t *testing.T) {
	h := newHarness(t)
	t.Setenv("PATH", t.TempDir()) // nothing on PATH
	h.ops.Runner = &runner.Runner{}

	res := h.ops.CheckPreflight(context.Background())
	if res.ResticInstalled {
		t.Error("CheckPreflight reports restic installed, want not installed")
	}
	if !strings.Contains(res.ResticError, "restic is not installed") {
		t.Errorf("ResticError = %q, want it to say restic is not installed", res.ResticError)
	}
	if res.DockerInstalled {
		t.Error("CheckPreflight reports docker installed, want not installed")
	}
}

func TestOpsCheckPreflight_NoRunnerConfigured(t *testing.T) {
	h := newHarness(t)
	res := h.ops.CheckPreflight(context.Background())
	if res.ResticInstalled || res.DockerInstalled {
		t.Errorf("CheckPreflight with no Runner = %+v, want neither tool reported installed", res)
	}
	if res.ResticError == "" || res.DockerError == "" {
		t.Error("CheckPreflight with no Runner configured should still explain why in both fields")
	}
}

func TestOpsListCatalog(t *testing.T) {
	h := newHarness(t)
	list := h.ops.ListCatalog()
	if len(list) != 1 || list[0].Service != "folder" {
		t.Errorf("ListCatalog = %+v, want [folder]", list)
	}
}

func TestOpsStatusLightsAndFreshness(t *testing.T) {
	h := newHarness(t)
	h.addDestination("local")
	if err := inventory.PutService(h.ops.DB, inventory.Service{Name: "svc", CatalogEntry: "folder", Destination: "local"}); err != nil {
		t.Fatalf("PutService: %v", err)
	}

	// No run yet: "none".
	st, err := h.ops.Status(context.Background(), "svc", false)
	if err != nil || len(st) != 1 {
		t.Fatalf("Status: %v, %v", st, err)
	}
	if st[0].Tiers[0].Light != "none" {
		t.Errorf("light = %s, want none", st[0].Tiers[0].Light)
	}
	if st[0].FreshnessAgeSeconds != nil {
		t.Errorf("freshness = %v, want nil (never run)", st[0].FreshnessAgeSeconds)
	}

	// A verified SUCCESS run: "green", freshness set, verify records
	// present when requested.
	m, err := engine.StartRun(h.ops.DB, "svc")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	for _, s := range []engine.State{engine.PREPARING, engine.DUMPING, engine.UPLOADING, engine.VERIFYING} {
		if err := m.Transition(s, "step"); err != nil {
			t.Fatalf("Transition %s: %v", s, err)
		}
	}
	if err := m.RecordVerification(engine.VerificationRecord{Method: "archive-integrity", Passed: true}); err != nil {
		t.Fatalf("RecordVerification: %v", err)
	}
	if err := m.Transition(engine.SUCCESS, "verified"); err != nil {
		t.Fatalf("Transition SUCCESS: %v", err)
	}

	st, err = h.ops.Status(context.Background(), "svc", true)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st[0].Tiers[0].Light != "green" {
		t.Errorf("light = %s, want green", st[0].Tiers[0].Light)
	}
	if st[0].FreshnessAgeSeconds == nil {
		t.Error("freshness = nil, want set after a SUCCESS")
	}
	if len(st[0].Verification) != 1 || st[0].Verification[0].Method != "archive-integrity" {
		t.Errorf("verification = %+v, want the recorded archive-integrity check", st[0].Verification)
	}
}

// TestOpsStatusStaleTierNeverGreen: a verified backup older than the
// service's max age is reported stale and shown amber, and the tier
// carries the last-verified time and allowed window.
func TestOpsStatusStaleTierNeverGreen(t *testing.T) {
	h := newHarness(t)
	h.addDestination("local")
	if err := inventory.PutService(h.ops.DB, inventory.Service{Name: "svc", CatalogEntry: "folder", Destination: "local"}); err != nil {
		t.Fatalf("PutService: %v", err)
	}
	m, err := engine.StartRun(h.ops.DB, "svc")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	for _, s := range []engine.State{engine.PREPARING, engine.DUMPING, engine.UPLOADING, engine.VERIFYING} {
		if err := m.Transition(s, "step"); err != nil {
			t.Fatalf("Transition %s: %v", s, err)
		}
	}
	if err := m.RecordVerification(engine.VerificationRecord{Method: "archive-integrity", Passed: true}); err != nil {
		t.Fatalf("RecordVerification: %v", err)
	}
	if err := m.Transition(engine.SUCCESS, "verified"); err != nil {
		t.Fatalf("Transition SUCCESS: %v", err)
	}

	tier := func() TierStatus {
		st, err := h.ops.Status(context.Background(), "svc", false)
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		return st[0].Tiers[0]
	}
	if got := tier(); got.Light != "green" || got.Stale || got.LastVerifiedAt == nil || got.MaxAgeSeconds == 0 {
		t.Fatalf("fresh tier = %+v, want green, not stale, verified time and max age set", got)
	}

	old := time.Now().Add(-100 * time.Hour).UTC().Format(time.RFC3339Nano)
	if _, err := h.ops.DB.Exec(`UPDATE runs SET finished_at = ?`, old); err != nil {
		t.Fatalf("aging run: %v", err)
	}
	if got := tier(); got.Light != "amber" || !got.Stale {
		t.Errorf("old tier = light %s stale %v, want amber and stale", got.Light, got.Stale)
	}
}

func TestOpsStatusUnknownServiceErrors(t *testing.T) {
	h := newHarness(t)
	if _, err := h.ops.Status(context.Background(), "nope", false); !errors.Is(err, inventory.ErrNotFound) {
		t.Errorf("Status = %v, want ErrNotFound", err)
	}
}

// TestOpsEnsureLocalPasswordRespectsSearchDirs: a password the
// operator provided in another credential directory (systemd's
// $CREDENTIALS_DIRECTORY) is used as is — no second, generated one
// may shadow it in Ballast's own directory.
func TestOpsEnsureLocalPasswordRespectsSearchDirs(t *testing.T) {
	h := newHarness(t)
	provided := t.TempDir()
	if err := os.WriteFile(filepath.Join(provided, "ballast-restic-password.local"), []byte("operator\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.ops.CredentialSearchDirs = []string{provided, h.credDir}
	dest := inventory.Destination{Name: "local", Location: "/stub-repo", PasswordCredential: "ballast-restic-password.local"}

	got, err := h.ops.ensurePassword(dest)
	if err != nil {
		t.Fatalf("ensurePassword: %v", err)
	}
	if got != "" {
		t.Errorf("generated %q although the operator provided a password", got)
	}
	if _, err := os.Stat(filepath.Join(h.credDir, "ballast-restic-password.local")); err == nil {
		t.Error("a password file was written to Ballast's own directory")
	}
}

// s3Dest builds an unsaved S3 destination for SetupDestination tests.
func s3Dest(name string) inventory.Destination {
	return inventory.Destination{
		Name: name, Type: backend.S3,
		Settings:  backend.Settings{Endpoint: "s3.amazonaws.com", Bucket: "my-bucket", Region: "us-east-1"},
		KeepDaily: 7,
	}
}

// TestOpsSetupDestinationWritesCredentials: SetupDestination saves the
// given backend credentials to a 0600 file and registers the secret
// value for redaction immediately.
func TestOpsSetupDestinationWritesCredentials(t *testing.T) {
	h := newHarness(t)
	h.ops.Redact = secretpkg.NewSet()
	req := DestinationSetup{
		Destination: s3Dest("aws"),
		Credentials: backend.Credentials{AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "topsecretkey"},
	}
	if err := h.ops.SetupDestination(context.Background(), req); err != nil {
		t.Fatalf("SetupDestination: %v", err)
	}

	saved, err := h.ops.GetDestination(context.Background(), "aws")
	if err != nil {
		t.Fatalf("GetDestination: %v", err)
	}
	path := filepath.Join(h.credDir, saved.BackendCredential)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat credential file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("credential file mode = %o, want 0600", perm)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "[default]\naws_access_key_id = AKIAEXAMPLE\naws_secret_access_key = topsecretkey\n"
	if string(data) != want {
		t.Errorf("credential file contents = %q, want %q", data, want)
	}
	if got := h.ops.redact("leaked topsecretkey here"); strings.Contains(got, "topsecretkey") {
		t.Errorf("redact(...) = %q, want the secret access key redacted", got)
	}
}

// TestOpsSetupDestinationOperatorCredentialConflict: a credential file
// of the same name already provided elsewhere (e.g. systemd's
// LoadCredential=) always wins, so writing one here would silently do
// nothing — SetupDestination must refuse loudly instead.
func TestOpsSetupDestinationOperatorCredentialConflict(t *testing.T) {
	h := newHarness(t)
	operatorDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(operatorDir, "ballast-dest-keys.aws"), []byte("operator-provided\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.ops.CredentialSearchDirs = []string{operatorDir, h.credDir}

	req := DestinationSetup{
		Destination: s3Dest("aws"),
		Credentials: backend.Credentials{AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "topsecretkey"},
	}
	err := h.ops.SetupDestination(context.Background(), req)
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("SetupDestination = %v, want ErrValidation", err)
	}
	if _, statErr := os.Stat(filepath.Join(h.credDir, "ballast-dest-keys.aws")); !os.IsNotExist(statErr) {
		t.Error("a credential file was written to Ballast's own directory despite the operator-provided one")
	}
}

// TestOpsSetupDestinationInitRequiresCredentials: Init=true against an
// S3 destination with no credentials given, and no pre-existing
// credential file, is a loud ErrValidation naming what is missing —
// never a raw restic auth failure deep into setup.
func TestOpsSetupDestinationInitRequiresCredentials(t *testing.T) {
	h := newHarness(t)
	req := DestinationSetup{Destination: s3Dest("aws"), Init: true}
	err := h.ops.SetupDestination(context.Background(), req)
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("SetupDestination = %v, want ErrValidation", err)
	}
	if !strings.Contains(err.Error(), "access key ID and secret access key") {
		t.Errorf("error = %q, want it to explain which credentials are missing", err)
	}
	if _, statErr := inventory.GetDestination(h.ops.DB, "aws"); statErr == nil {
		t.Error("destination was saved despite the missing credentials")
	}
}

// TestOpsSetupDestinationInitFailureRemovesNewCredentialFile: when
// Init fails, a credential file SetupDestination itself just wrote for
// a brand-new destination is removed again (nothing half-made is left
// behind), and the new destination row is removed too.
func TestOpsSetupDestinationInitFailureRemovesNewCredentialFile(t *testing.T) {
	h := newHarness(t)
	if err := os.WriteFile(filepath.Join(h.credDir, "ballast-restic-password.aws"), []byte("pw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STUB_FAIL_CMD", "snapshots")
	t.Setenv("STUB_FAIL_CODE", "1")

	req := DestinationSetup{
		Destination: s3Dest("aws"),
		Credentials: backend.Credentials{AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "topsecretkey"},
		Init:        true,
	}
	err := h.ops.SetupDestination(context.Background(), req)
	if err == nil {
		t.Fatal("SetupDestination = nil, want the init failure")
	}
	if _, statErr := os.Stat(filepath.Join(h.credDir, "ballast-dest-keys.aws")); !os.IsNotExist(statErr) {
		t.Error("the newly written credential file was not removed after the init failure")
	}
	if _, getErr := inventory.GetDestination(h.ops.DB, "aws"); getErr == nil {
		t.Error("the half-created destination row was not removed after the init failure")
	}
}

// TestOpsSetupDestinationInitFailureRestoresExistingCredentialFile: for
// an existing destination being updated, a credential file that
// already had contents keeps its original contents after a failed
// Init, rather than being left holding the new, unproven value or
// being deleted outright.
func TestOpsSetupDestinationInitFailureRestoresExistingCredentialFile(t *testing.T) {
	h := newHarness(t)
	dest := s3Dest("aws")
	if err := os.WriteFile(filepath.Join(h.credDir, "ballast-restic-password.aws"), []byte("pw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	const previous = "[default]\naws_access_key_id = OLDKEY\naws_secret_access_key = oldsecret\n"
	if err := inventory.PutDestination(h.ops.DB, dest); err != nil {
		t.Fatal(err)
	}
	saved, err := inventory.GetDestination(h.ops.DB, "aws")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.credDir, saved.BackendCredential), []byte(previous), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("STUB_FAIL_CMD", "snapshots")
	t.Setenv("STUB_FAIL_CODE", "1")
	req := DestinationSetup{
		Destination: dest,
		Credentials: backend.Credentials{AccessKeyID: "NEWKEY", SecretAccessKey: "newsecret"},
		Init:        true,
	}
	if err := h.ops.SetupDestination(context.Background(), req); err == nil {
		t.Fatal("SetupDestination = nil, want the init failure")
	}
	data, err := os.ReadFile(filepath.Join(h.credDir, saved.BackendCredential))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != previous {
		t.Errorf("credential file after failed init = %q, want the previous contents %q restored", data, previous)
	}
}

// TestOpsEnsurePasswordGeneratesForNonLocalTypes: ensurePassword's
// automatic repository-password generation (spec §5.3.6) applies to
// every backend type, not just local disk — a beginner adding a B2 or
// S3 destination should not have to invent a password either.
func TestOpsEnsurePasswordGeneratesForNonLocalTypes(t *testing.T) {
	h := newHarness(t)
	dest := s3Dest("aws")
	if err := dest.Normalize(); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	got, err := h.ops.ensurePassword(dest)
	if err != nil {
		t.Fatalf("ensurePassword: %v", err)
	}
	if got == "" {
		t.Fatal("ensurePassword generated nothing for a non-local destination type")
	}
	data, err := os.ReadFile(got)
	if err != nil {
		t.Fatal(err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		t.Error("generated repository password is empty")
	}
}

// TestOpsInitDestinationCreatesRepo: InitDestination against a
// destination whose repository does not exist yet actually creates
// it (`restic init`), the same idempotent probe-then-init
// ensureRepoInitialized uses during SetupDestination.
func TestOpsInitDestinationCreatesRepo(t *testing.T) {
	h := newHarness(t)
	h.addDestination("local")
	t.Setenv("STUB_SNAPSHOTS_MISSING_COUNT", "1")

	if err := h.ops.InitDestination(context.Background(), "local"); err != nil {
		t.Fatalf("InitDestination: %v", err)
	}
	found := false
	for _, line := range h.resticInvocations(t) {
		if strings.HasPrefix(line, "init") {
			found = true
		}
	}
	if !found {
		t.Errorf("restic invocations = %v, want an init call", h.resticInvocations(t))
	}
}

// TestOpsInitDestinationAlreadyInitialized: InitDestination against a
// repository that already exists is a no-op (never calls `restic
// init` again), so it's safe to call more than once.
func TestOpsInitDestinationAlreadyInitialized(t *testing.T) {
	h := newHarness(t)
	h.addDestination("local")

	if err := h.ops.InitDestination(context.Background(), "local"); err != nil {
		t.Fatalf("InitDestination: %v", err)
	}
	for _, line := range h.resticInvocations(t) {
		if strings.HasPrefix(line, "init") {
			t.Errorf("restic invocations = %v, want no init call (repository already exists)", h.resticInvocations(t))
		}
	}
}

// TestOpsInitDestinationNotFound: InitDestination against an unknown
// destination name is inventory.ErrNotFound, not a raw restic error.
func TestOpsInitDestinationNotFound(t *testing.T) {
	h := newHarness(t)
	err := h.ops.InitDestination(context.Background(), "nope")
	if !errors.Is(err, inventory.ErrNotFound) {
		t.Errorf("InitDestination(nope) = %v, want inventory.ErrNotFound", err)
	}
}

// TestOpsTestDestinationRepoMissing: TestDestination against a
// destination whose repository was never initialized gives a message
// explaining that Ballast reached the destination but found no
// repository there yet, not a raw restic error.
func TestOpsTestDestinationRepoMissing(t *testing.T) {
	h := newHarness(t)
	h.addDestination("local")
	t.Setenv("STUB_FAIL_CMD", "snapshots")
	t.Setenv("STUB_FAIL_CODE", "10")

	err := h.ops.TestDestination(context.Background(), "local")
	if err == nil {
		t.Fatal("TestDestination = nil, want an error (no repository initialized yet)")
	}
	if !strings.Contains(err.Error(), "no backup repository there yet") {
		t.Errorf("error = %q, want it to say there is no backup repository there yet", err)
	}
}

func TestOpsEnvSecret(t *testing.T) {
	h := newHarness(t)
	h.addDestination("local")
	ctx := context.Background()
	if err := h.ops.PutService(ctx, inventory.Service{Name: "svc", CatalogEntry: "folder", Destination: "local", Params: map[string]string{"path": "/x"}}); err != nil {
		t.Fatal(err)
	}
	h.ops.Redact = secretpkg.NewSet()
	envPath := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(envPath, []byte("DB_PASSWORD=s3cr3t-pw\nDB_USERNAME=immich\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	keys, err := h.ops.TestEnvSource(ctx, envPath)
	if err != nil || !reflect.DeepEqual(keys, []string{"DB_PASSWORD", "DB_USERNAME"}) {
		t.Fatalf("TestEnvSource = %v, %v", keys, err)
	}
	if _, err := h.ops.TestEnvSource(ctx, "relative/.env"); !errors.Is(err, ErrValidation) {
		t.Errorf("relative path err = %v, want ErrValidation", err)
	}

	tests := []struct {
		name, path, key string
		wantErr         string
	}{
		{"ok", envPath, "DB_PASSWORD", ""},
		{"missing key", envPath, "NOPE", "NOPE"},
		{"missing file", envPath + ".gone", "DB_PASSWORD", "cannot read"},
		{"relative", "x/.env", "DB_PASSWORD", "absolute"},
		{"bad key", envPath, "A B", "plain variable name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := h.ops.SetEnvSecret(ctx, "svc", "db_password", tt.path, tt.key)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if !errors.Is(err, ErrValidation) || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want ErrValidation containing %q", err, tt.wantErr)
			}
		})
	}

	got, _ := h.ops.GetService(ctx, "svc")
	if got.Secrets["db_password"] != inventory.EnvRef(envPath, "DB_PASSWORD") {
		t.Errorf("stored ref = %q", got.Secrets["db_password"])
	}
	b, _ := json.Marshal(got)
	if strings.Contains(string(b), "s3cr3t-pw") {
		t.Error("service API shape leaks the value")
	}
	if strings.Contains(h.ops.Redact.Redact("x s3cr3t-pw"), "s3cr3t-pw") {
		t.Error("value not registered for redaction")
	}
}

func TestOpsReadEnvSource(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.ops.Redact = secretpkg.NewSet()
	reg, err := catalog.LoadRegistry(bundledcatalog.Bundled, nil)
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	h.ops.Catalog = reg

	write := func(body string) string {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	full := write("UPLOAD_LOCATION=./library\nDB_USERNAME=immich\nDB_PASSWORD=s3cr3t-pw\nDB_DATABASE_NAME=immich\n")

	prop, err := h.ops.ReadEnvSource(ctx, "immich", full)
	if err != nil {
		t.Fatalf("ReadEnvSource: %v", err)
	}
	wantParams := map[string]string{"library_dir": filepath.Join(full, "library"), "db_user": "immich", "db_name": "immich"}
	if !reflect.DeepEqual(prop.Params, wantParams) {
		t.Errorf("Params = %v, want %v", prop.Params, wantParams)
	}
	wantRef := inventory.EnvRef(filepath.Join(full, ".env"), "DB_PASSWORD")
	if prop.Secrets["db_password"] != wantRef || len(prop.Missing) != 0 {
		t.Errorf("Secrets = %v Missing = %v, want db_password -> %s and nothing missing", prop.Secrets, prop.Missing, wantRef)
	}
	if b, _ := json.Marshal(prop); strings.Contains(string(b), "s3cr3t-pw") {
		t.Errorf("proposal leaked the password: %s", b)
	}

	t.Run("missing required key is reported, optional is not", func(t *testing.T) {
		p, err := h.ops.ReadEnvSource(ctx, "immich", write("UPLOAD_LOCATION=/photos\nDB_PASSWORD=x\n"))
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(p.Missing, []string{"DB_USERNAME"}) {
			t.Errorf("Missing = %v, want [DB_USERNAME]", p.Missing)
		}
	})
	t.Run("errors", func(t *testing.T) {
		for name, tc := range map[string]struct{ entry, dir string }{
			"relative dir":  {"immich", "opt/immich"},
			"no .env":       {"immich", t.TempDir()},
			"no shortcut":   {"folder", full},
			"unknown entry": {"nope", full},
		} {
			if _, err := h.ops.ReadEnvSource(ctx, tc.entry, tc.dir); !errors.Is(err, ErrValidation) {
				t.Errorf("%s: err = %v, want ErrValidation", name, err)
			}
		}
	})
}

// TestOpsEnvSourceSetupStoresReferencesOnly walks the one-path setup
// (spec §18.2): read the folder's .env, save the proposed service, and
// check the database holds the path+key reference but never the password.
func TestOpsEnvSourceSetupStoresReferencesOnly(t *testing.T) {
	h := newHarness(t)
	h.addDestination("local")
	ctx := context.Background()
	reg, err := catalog.LoadRegistry(bundledcatalog.Bundled, nil)
	if err != nil {
		t.Fatal(err)
	}
	h.ops.Catalog = reg
	h.ops.Redact = secretpkg.NewSet()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("UPLOAD_LOCATION=/photos\nDB_USERNAME=immich\nDB_PASSWORD=s3cr3t-pw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	prop, err := h.ops.ReadEnvSource(ctx, "immich", dir)
	if err != nil {
		t.Fatal(err)
	}
	params := map[string]string{"db_image": "img", "db_container": "c"}
	for k, v := range prop.Params {
		params[k] = v
	}
	if err := h.ops.PutService(ctx, inventory.Service{Name: "photos", CatalogEntry: "immich", Destination: "local", Params: params, Secrets: prop.Secrets}); err != nil {
		t.Fatalf("PutService: %v", err)
	}
	svc, err := inventory.GetService(h.ops.DB, "photos")
	if err != nil {
		t.Fatal(err)
	}
	if want := inventory.EnvRef(filepath.Join(dir, ".env"), "DB_PASSWORD"); svc.Secrets["db_password"] != want {
		t.Errorf("stored secret = %q, want %q", svc.Secrets["db_password"], want)
	}
	if b, _ := json.Marshal(svc); strings.Contains(string(b), "s3cr3t-pw") {
		t.Errorf("stored service leaked the password: %s", b)
	}
}

// TestOpsPreviewExcludes: the preview applies the draft patterns on top
// of the service's own, reads nothing but names and sizes, and rejects
// bad patterns and unknown services.
func TestOpsPreviewExcludes(t *testing.T) {
	h := newHarness(t)
	h.addDestination("local")
	dir := t.TempDir()
	for name, body := range map[string]string{"a.txt": "aaaa", "b.log": "bbbbbbbb", "c.tmp": "cc"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	svc := inventory.Service{Name: "docs", CatalogEntry: "folder", Destination: "local", Params: map[string]string{"path": dir}}
	if err := h.ops.PutService(context.Background(), svc); err != nil {
		t.Fatalf("PutService: %v", err)
	}
	pv, err := h.ops.PreviewExcludes(context.Background(), "docs", []string{"*.log", "*.tmp"})
	if err != nil {
		t.Fatalf("PreviewExcludes: %v", err)
	}
	if pv.SkippedCount != 2 || pv.SkippedBytes != 10 || pv.KeptBytes != 4 {
		t.Errorf("preview = %+v, want 2 skipped / 10 bytes / 4 kept", pv)
	}
	if _, err := h.ops.PreviewExcludes(context.Background(), "docs", []string{"!x"}); !errors.Is(err, ErrValidation) {
		t.Errorf("bad pattern error = %v, want ErrValidation", err)
	}
	if _, err := h.ops.PreviewExcludes(context.Background(), "ghost", nil); !errors.Is(err, inventory.ErrNotFound) {
		t.Errorf("unknown service error = %v, want ErrNotFound", err)
	}
}

// TestOpsStatusOneLightPerTarget: a failed copy to one destination is
// red on its own and never hidden by a good copy to another, and the
// service's own light is the worst of them.
func TestOpsStatusOneLightPerTarget(t *testing.T) {
	h := newHarness(t)
	h.addDestination("local")
	h.addDestination("offsite")
	svc := inventory.Service{Name: "svc", CatalogEntry: "folder", Destination: "local",
		Targets: []inventory.Target{{Destination: "local"}, {Destination: "offsite", Parts: []string{"library"}}}}
	if err := inventory.PutService(h.ops.DB, svc); err != nil {
		t.Fatalf("PutService: %v", err)
	}
	st, err := h.ops.Status(context.Background(), "svc", false)
	if err != nil || len(st[0].Targets) != 2 || st[0].Targets[0].Light != "none" {
		t.Fatalf("before any run: %+v, %v", st, err)
	}

	m, err := engine.StartRun(h.ops.DB, "svc")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []engine.State{engine.PREPARING, engine.DUMPING, engine.UPLOADING, engine.VERIFYING} {
		if err := m.Transition(s, "step"); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range []engine.TargetResult{
		{Destination: "local", State: engine.SUCCESS, SnapshotID: "aaa"},
		{Destination: "offsite", State: engine.FAILED, Detail: "unreachable"},
	} {
		if err := m.RecordTarget(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Transition(engine.FAILED, "backup failed for offsite: unreachable"); err != nil {
		t.Fatal(err)
	}
	st, err = h.ops.Status(context.Background(), "svc", false)
	if err != nil {
		t.Fatal(err)
	}
	got := st[0].Targets
	if got[0].Destination != "local" || got[0].Light != "green" || got[0].SnapshotID != "aaa" {
		t.Errorf("local target = %+v, want green with its snapshot", got[0])
	}
	if got[1].Destination != "offsite" || got[1].Light != "red" || got[1].LastError != "unreachable" || len(got[1].Parts) != 1 {
		t.Errorf("offsite target = %+v, want red naming the failure", got[1])
	}
	if st[0].Tiers[0].Light != "red" {
		t.Errorf("service light = %s, want red (worst target)", st[0].Tiers[0].Light)
	}
}

package main

import (
	"bytes"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/springledev/ballast/internal/api"
	"github.com/springledev/ballast/internal/engine"
	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/store"
)

// secretNtfyTopic and secretAPIToken are planted secrets for
// TestSecretsNeverLeak. They are deliberately distinctive strings
// that would never appear in ordinary output by accident.
const (
	secretNtfyTopic = "https://ntfy.sh/SECRET-TOPIC-do-not-leak-XYZ789"
	secretAPIToken  = "SECRET-API-TOKEN-do-not-leak-ABC123"
)

// resticLeakingStubScript is a fake restic CLI whose `backup`
// subcommand fails after printing both planted secrets to stderr —
// simulating the worst case (a secret ending up in a subprocess's
// error output) so the test proves the redaction pipeline scrubs it
// everywhere, not just the paths where Ballast itself would normally
// place a secret.
var resticLeakingStubScript = `#!/bin/sh
cmd="$1"
case "$cmd" in
  backup)
    echo "stub: simulated failure leaking ` + secretNtfyTopic + ` and ` + secretAPIToken + `" >&2
    exit 1
    ;;
  version) printf 'restic 0.17.3 compiled with go1.23.3 on linux/amd64\n' ;;
  *) exit 0 ;;
esac
`

// TestSecretsNeverLeak plants a fake ntfy topic and API token in
// credential files and a stub restic that leaks both into stderr and
// fails, then asserts neither secret appears in: CLI stdout/stderr
// (which also carries the Coordinator's log lines), an API response
// body, runs.error, run_transitions.detail, or verification_results.*
// (spec rule 2; spec §13 security layer).
func TestSecretsNeverLeak(t *testing.T) {
	dir := t.TempDir()
	resticPath := filepath.Join(dir, "restic")
	if err := os.WriteFile(resticPath, []byte(resticLeakingStubScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	data := filepath.Join(t.TempDir(), "data")
	conf := filepath.Join(t.TempDir(), "config")
	credDir := filepath.Join(conf, "credentials")
	t.Setenv("BALLAST_DATA_DIR", data)
	t.Setenv("BALLAST_CONFIG_DIR", conf)
	t.Setenv("CREDENTIALS_DIRECTORY", "")
	if err := os.MkdirAll(credDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeCred := func(name, value string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(credDir, name), []byte(value+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeCred("ballast-restic-password.local", "repo-password")
	writeCred("ballast-ntfy-topic", secretNtfyTopic) // global fallback channel
	writeCred("ballast-api-token", secretAPIToken)

	// Configure a destination and a service whose backup will fail
	// (the stub restic's `backup` subcommand always leaks and fails).
	if out, err := execCLI(t, "destination", "add", "local", "--location", "/srv/backups"); err != nil {
		t.Fatalf("destination add: %v (%s)", err, out)
	}
	dataDir := t.TempDir()
	if out, err := execCLI(t, "service", "add", "svc", "--catalog", "folder", "--destination", "local", "--param", "path="+dataDir); err != nil {
		t.Fatalf("service add: %v (%s)", err, out)
	}
	if out, err := execCLI(t, "schedule", "set", "svc", "* * * * *"); err != nil {
		t.Fatalf("schedule set: %v (%s)", err, out)
	}

	assertNoSecret := func(label, s string) {
		t.Helper()
		if strings.Contains(s, secretNtfyTopic) {
			t.Errorf("%s leaks the ntfy topic secret:\n%s", label, s)
		}
		if strings.Contains(s, secretAPIToken) {
			t.Errorf("%s leaks the API token secret:\n%s", label, s)
		}
	}

	backupOut, backupErr := execCLI(t, "backup", "svc")
	if backupErr == nil {
		t.Fatal("backup: want an error (the stub restic always fails)")
	}
	assertNoSecret("backup CLI output", backupOut)
	assertNoSecret("backup error", backupErr.Error())

	runDueOut, runDueErr := execCLI(t, "run-due")
	if runDueErr == nil {
		t.Fatal("run-due: want an error (svc's backup fails every time)")
	}
	assertNoSecret("run-due CLI output", runDueOut)
	assertNoSecret("run-due error", runDueErr.Error())

	// Inspect the database directly: runs.error, run_transitions.detail,
	// verification_results.* (verification never even runs here since
	// upload fails first, but the query still proves no rows leak it).
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "ballast.db"))
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	defer st.Close()

	rows, err := st.DB().Query(`SELECT COALESCE(error, '') FROM runs`)
	if err != nil {
		t.Fatalf("querying runs: %v", err)
	}
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			t.Fatal(err)
		}
		assertNoSecret("runs.error", e)
	}
	rows.Close()

	rows, err = st.DB().Query(`SELECT detail FROM run_transitions`)
	if err != nil {
		t.Fatalf("querying run_transitions: %v", err)
	}
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			t.Fatal(err)
		}
		assertNoSecret("run_transitions.detail", d)
	}
	rows.Close()

	rows, err = st.DB().Query(`SELECT warning, detail, output FROM verification_results`)
	if err != nil {
		t.Fatalf("querying verification_results: %v", err)
	}
	for rows.Next() {
		var w, d, o string
		if err := rows.Scan(&w, &d, &o); err != nil {
			t.Fatal(err)
		}
		assertNoSecret("verification_results.warning", w)
		assertNoSecret("verification_results.detail", d)
		assertNoSecret("verification_results.output", o)
	}
	rows.Close()

	// Confirm the secrets really were loaded (not just absent because
	// nothing tried), then check the API surface: an API request must
	// not leak them either, and the API token itself is checked
	// separately (spec rule 2: it is shown once at generation and
	// never again).
	runs, err := engine.RecentRuns(st.DB(), "svc", 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("RecentRuns: %v, %v", runs, err)
	}
	if runs[0].State != engine.FAILED {
		t.Fatalf("run state = %s, want FAILED", runs[0].State)
	}
	if runs[0].Error == "" {
		t.Fatal("runs.error is empty; the test did not exercise a real failure")
	}

	a, err := openApp(cfg, &discardWriter{})
	if err != nil {
		t.Fatalf("openApp: %v", err)
	}
	defer a.Close()
	if len(a.secrets.Values()) == 0 {
		t.Fatal("no secrets loaded by wiring; the test did not exercise real redaction")
	}

	handler := api.NewHandler(a.ops, a.apiToken)
	req := httptest.NewRequest("GET", "/api/v1/status?service=svc&verify=1", nil)
	req.Header.Set("Authorization", "Bearer "+secretAPIToken)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	assertNoSecret("API response body", rec.Body.String())

	// Trigger token generation: shown once, in its own response only.
	tokReq := httptest.NewRequest("POST", "/api/v1/services/svc/trigger-token", nil)
	tokReq.Header.Set("Authorization", "Bearer "+secretAPIToken)
	tokRec := httptest.NewRecorder()
	handler.ServeHTTP(tokRec, tokReq)
	assertNoSecret("trigger-token response body", tokRec.Body.String())
}

// discardWriter is an io.Writer that discards everything, for a
// silent openApp call in this test (the CLI calls above already
// captured everything relevant).
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// repoPassword is a distinctive restic repository password, planted
// as a destination's credential file, that TestRepositoryPasswordRedacted
// proves reaches loadRepositoryPasswords (wiring.go ~line 201) and is
// therefore redacted everywhere: the CLI, the DB, the app log writer,
// and the API — closing the gap that TestSecretsNeverLeak left open
// (it plants this exact credential file but never asserts on it).
const repoPassword = "repo-pw-7f3c9e"

// repoTestAPIToken authenticates the API requests
// TestRepositoryPasswordRedacted makes; it is not itself under test.
const repoTestAPIToken = "repo-pw-test-api-token"

// resticLeakingRepoPasswordScript is a fake restic CLI whose `backup`
// and `snapshots` subcommands both print repoPassword to stderr and
// fail: `backup` exercises the backup pipeline path (CLI output,
// runs.error, run_transitions.detail, the app log writer), and
// `snapshots` exercises the one Ops call that talks to a
// restic.Repository directly rather than through the pipeline, so the
// API surface (GET .../snapshots) is covered too.
var resticLeakingRepoPasswordScript = `#!/bin/sh
cmd="$1"
case "$cmd" in
  backup)
    echo "stub: simulated failure leaking ` + repoPassword + `" >&2
    exit 1
    ;;
  snapshots)
    echo "stub: simulated failure leaking ` + repoPassword + `" >&2
    exit 1
    ;;
  version) printf 'restic 0.17.3 compiled with go1.23.3 on linux/amd64\n' ;;
  *) exit 0 ;;
esac
`

// TestRepositoryPasswordRedacted closes the gap flagged in the task:
// loadRepositoryPasswords (wiring.go) reads every destination's restic
// repository password at openApp time and adds it to the shared
// *secret.Set, but nothing asserted that the planted password in
// TestSecretsNeverLeak ever actually got redacted. This test plants
// its own distinctive password, makes the stub restic leak it from
// both the backup pipeline and the direct-repository (snapshots) path,
// and checks every surface a leak could reach: CLI output, the DB, the
// app log writer, a direct Redact call, and an API response body.
func TestRepositoryPasswordRedacted(t *testing.T) {
	dir := t.TempDir()
	resticPath := filepath.Join(dir, "restic")
	if err := os.WriteFile(resticPath, []byte(resticLeakingRepoPasswordScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	data := filepath.Join(t.TempDir(), "data")
	conf := filepath.Join(t.TempDir(), "config")
	credDir := filepath.Join(conf, "credentials")
	t.Setenv("BALLAST_DATA_DIR", data)
	t.Setenv("BALLAST_CONFIG_DIR", conf)
	t.Setenv("CREDENTIALS_DIRECTORY", "")
	if err := os.MkdirAll(credDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeCred := func(name, value string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(credDir, name), []byte(value+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeCred("ballast-restic-password.local", repoPassword)
	writeCred("ballast-api-token", repoTestAPIToken)

	if out, err := execCLI(t, "destination", "add", "local", "--location", "/srv/backups"); err != nil {
		t.Fatalf("destination add: %v (%s)", err, out)
	}
	dataDir := t.TempDir()
	if out, err := execCLI(t, "service", "add", "svc", "--catalog", "folder", "--destination", "local", "--param", "path="+dataDir); err != nil {
		t.Fatalf("service add: %v (%s)", err, out)
	}

	var sawRedactedMarker bool
	assertNoSecret := func(label, s string) {
		t.Helper()
		if strings.Contains(s, repoPassword) {
			t.Errorf("%s leaks the repository password:\n%s", label, s)
		}
		if strings.Contains(s, "[redacted]") {
			sawRedactedMarker = true
		}
	}

	// The CLI backup path (pipeline -> runner -> stub restic's
	// leaking, failing `backup` subcommand).
	backupOut, backupErr := execCLI(t, "backup", "svc")
	if backupErr == nil {
		t.Fatal("backup: want an error (the stub restic always fails)")
	}
	assertNoSecret("backup CLI output", backupOut)
	assertNoSecret("backup error", backupErr.Error())

	// The DB: runs.error and run_transitions.detail.
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "ballast.db"))
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	defer st.Close()

	for _, q := range []struct {
		label, query string
	}{
		{"runs.error", `SELECT COALESCE(error, '') FROM runs`},
		{"run_transitions.detail", `SELECT detail FROM run_transitions`},
	} {
		rows, err := st.DB().Query(q.query)
		if err != nil {
			t.Fatalf("querying %s: %v", q.label, err)
		}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			assertNoSecret(q.label, s)
		}
		rows.Close()
	}

	runs, err := engine.RecentRuns(st.DB(), "svc", 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("RecentRuns: %v, %v", runs, err)
	}
	if runs[0].State != engine.FAILED {
		t.Fatalf("run state = %s, want FAILED", runs[0].State)
	}
	if runs[0].Error == "" {
		t.Fatal("runs.error is empty; the test did not exercise a real failure")
	}

	// openApp itself: confirm the password was actually loaded, that
	// the app log writer scrubs it, and that Redact behaves exactly as
	// documented.
	var logBuf bytes.Buffer
	a, err := openApp(cfg, &logBuf)
	if err != nil {
		t.Fatalf("openApp: %v", err)
	}
	defer a.Close()
	if len(a.secrets.Values()) == 0 {
		t.Fatal("no secrets loaded by wiring; the test did not exercise real redaction")
	}
	a.logf("simulated leak of the repository password: %s", repoPassword)
	assertNoSecret("app log writer", logBuf.String())

	if got, want := a.secrets.Redact("x "+repoPassword+" y"), "x [redacted] y"; got != want {
		t.Errorf("a.secrets.Redact(...) = %q, want %q", got, want)
	}

	// The API surface: GET .../snapshots is the one Ops call that
	// talks to a restic.Repository directly (not through the backup
	// pipeline), and the stub restic's `snapshots` subcommand leaks
	// and fails too, so this checks a second, independent code path.
	handler := api.NewHandler(a.ops, a.apiToken)
	req := httptest.NewRequest("GET", "/api/v1/services/svc/snapshots", nil)
	req.Header.Set("Authorization", "Bearer "+repoTestAPIToken)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	assertNoSecret("snapshots API response body", rec.Body.String())
	if rec.Code == 200 {
		t.Errorf("snapshots response status = %d, want a failure (the stub restic's `snapshots` always fails)", rec.Code)
	}

	if !sawRedactedMarker {
		t.Fatal("never saw the [redacted] placeholder anywhere; the test did not exercise real redaction")
	}
}

// TestMissingRepositoryPasswordSecretDoesNotFailOpenApp confirms
// loadRepositoryPasswords' documented behavior (wiring.go ~line 201):
// a destination whose password credential file was never written (or
// was since removed) must not stop openApp from working. A missing
// or unreadable password file is reported loudly by the first command
// that actually tries to reach that destination, not at startup.
func TestMissingRepositoryPasswordSecretDoesNotFailOpenApp(t *testing.T) {
	withStubRestic(t)

	data := filepath.Join(t.TempDir(), "data")
	conf := filepath.Join(t.TempDir(), "config")
	credDir := filepath.Join(conf, "credentials")
	t.Setenv("BALLAST_DATA_DIR", data)
	t.Setenv("BALLAST_CONFIG_DIR", conf)
	t.Setenv("CREDENTIALS_DIRECTORY", "")
	if err := os.MkdirAll(credDir, 0o700); err != nil {
		t.Fatal(err)
	}

	if out, err := execCLI(t, "destination", "add", "nopass", "--location", "/srv/backups"); err != nil {
		t.Fatalf("destination add: %v (%s)", err, out)
	}
	// Deliberately: no ballast-restic-password.nopass file is ever
	// written to credDir.

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	a, err := openApp(cfg, &discardWriter{})
	if err != nil {
		t.Fatalf("openApp with a missing repository password file: %v", err)
	}
	a.Close()
}

// resticEchoPasswordFileScript is a fake restic CLI whose `backup` and
// `snapshots` subcommands print the contents of the password file they
// were handed, then fail — the worst case of a tool echoing its own
// secret. Unlike resticLeakingRepoPasswordScript, what it leaks
// follows the file, so it also exercises a password that changes while
// Ballast is running.
const resticEchoPasswordFileScript = `#!/bin/sh
case "$1" in
  backup|snapshots)
    echo "stub: simulated failure leaking $(cat "$RESTIC_PASSWORD_FILE")" >&2
    exit 1
    ;;
  version) printf 'restic 0.17.3 compiled with go1.23.3 on linux/amd64\n' ;;
  *) exit 0 ;;
esac
`

// TestRepositoryPasswordAddedAfterStartupRedacted: a destination
// created while the app is already open (the web UI under `ballast
// serve`), with an operator-supplied password file that did not exist
// at openApp time, must still have its password redacted — and so must
// a password file replaced after that. loadRepositoryPasswords only
// runs at startup, so this covers the per-use reads in repoFor and
// Pipeline.Run.
func TestRepositoryPasswordAddedAfterStartupRedacted(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "restic"), []byte(resticEchoPasswordFileScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	conf := filepath.Join(t.TempDir(), "config")
	credDir := filepath.Join(conf, "credentials")
	t.Setenv("BALLAST_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv("BALLAST_CONFIG_DIR", conf)
	t.Setenv("CREDENTIALS_DIRECTORY", "")
	if err := os.MkdirAll(credDir, 0o700); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	var logBuf bytes.Buffer
	a, err := openApp(cfg, &logBuf)
	if err != nil {
		t.Fatalf("openApp: %v", err)
	}
	defer a.Close()

	pwFile := filepath.Join(credDir, "ballast-restic-password.late")
	writePassword := func(pw string) {
		t.Helper()
		if err := os.WriteFile(pwFile, []byte(pw+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	checkErr := func(label string, err error, secret string) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: want an error (the stub restic always fails)", label)
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("%s leaks the repository password: %v", label, err)
		}
		if !strings.Contains(err.Error(), "[redacted]") {
			t.Errorf("%s: want the [redacted] placeholder (did the stub leak at all?): %v", label, err)
		}
	}

	ctx := t.Context()
	const firstPW = "late-pw-0d41b7"
	writePassword(firstPW)
	dest := inventory.Destination{Name: "late", Location: t.TempDir(), KeepDaily: 7}

	// API/CLI path: PutDestination -> repoFor -> restic snapshots.
	checkErr("PutDestination", a.ops.PutDestination(ctx, dest, true), firstPW)

	// Backup path: Pipeline.Run -> restic backup.
	if err := a.ops.PutDestination(ctx, dest, false); err != nil {
		t.Fatalf("PutDestination without init: %v", err)
	}
	if err := a.ops.PutService(ctx, inventory.Service{
		Name: "svc", CatalogEntry: "folder", Destination: "late", Params: map[string]string{"path": t.TempDir()},
	}); err != nil {
		t.Fatalf("PutService: %v", err)
	}
	checkErr("backup", a.ops.Backup(ctx, "svc"), firstPW)

	// The password file is replaced while the app keeps running.
	const rotatedPW = "rotated-pw-93e2aa"
	writePassword(rotatedPW)
	_, err = a.ops.Snapshots(ctx, "svc")
	checkErr("snapshots after rotation", err, rotatedPW)
	checkErr("backup after rotation", a.ops.Backup(ctx, "svc"), rotatedPW)

	for _, pw := range []string{firstPW, rotatedPW} {
		if strings.Contains(logBuf.String(), pw) {
			t.Errorf("app log leaks a repository password:\n%s", logBuf.String())
		}
	}
}

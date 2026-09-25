package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/springledev/ballast/internal/api"
	"github.com/springledev/ballast/internal/engine"
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
	if len(a.secrets) == 0 {
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

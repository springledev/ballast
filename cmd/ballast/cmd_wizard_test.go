package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/springledev/ballast/internal/api"
	"github.com/springledev/ballast/internal/engine"
	"github.com/springledev/ballast/internal/store"
)

// wizardInput feeds lines as the wizard's stdin, sets them up before
// execCLI runs "wizard", and restores stdin afterwards.
func wizardInput(t *testing.T, lines ...string) {
	t.Helper()
	rootCmd.SetIn(strings.NewReader(strings.Join(lines, "\n") + "\n"))
	t.Cleanup(func() { rootCmd.SetIn(os.Stdin) })
}

// newBackupSource creates a real directory with one file in it, for
// the "folder" catalog entry's required path parameter — the files
// strategy stats the path and refuses an empty/missing one.
func newBackupSource(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "source-data")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestWizardHappyPath_Folder runs the wizard end to end through the
// REAL backup pipeline (nil runner override, only restic stubbed) —
// not spyRunner — because only the real pipeline actually records a
// run via engine.StartRun; a stubbed scheduler.Runner reports a state
// straight to the Coordinator without ever writing a run row, so
// reportWizardResult's DB-backed status lookup would see nothing.
// The "files" strategy needs no Docker at all, so no docker stub is
// required here.
func TestWizardHappyPath_Folder(t *testing.T) {
	cliEnv(t, nil)
	withStubRestic(t)
	backupDir := newBackupSource(t)

	wizardInput(t,
		"folder",   // catalog entry
		"myfolder", // service name
		backupDir,  // required "path" parameter
		"",         // destination name -> default "local"
		"",         // backup storage folder -> default
		"",         // ntfy link -> skip
		"",         // schedule -> default nightly
	)

	out, err := execCLI(t, "wizard")
	if err != nil {
		t.Fatalf("wizard: %v (output %q)", err, out)
	}
	for _, want := range []string{
		"Saved myfolder.",
		"myfolder: SUCCESS",
		"can be restored",
		"keep a copy of that file somewhere safe", // generated password: warn it's the only key
		"Scheduled (0 3 * * *)",                   // Enter accepts the nightly default
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output = %q, want it to contain %q", out, want)
		}
	}

	out, err = execCLI(t, "status", "--service", "myfolder")
	if err != nil {
		t.Fatalf("status: %v (output %q)", err, out)
	}
	if !strings.Contains(out, "myfolder") {
		t.Errorf("status output = %q, want myfolder listed", out)
	}
}

// wizardRunHistory returns, for service's most recent run, its final
// state and its persisted state-transition path (e.g. "PENDING→…→SUCCESS")
// straight from the DB, plus each recorded verification result.
func wizardRunHistory(t *testing.T, data, service string) (final string, path []string, verifications map[string]bool) {
	t.Helper()
	verifications = map[string]bool{}
	withDB(t, data, func(s *store.Store) {
		var runID int64
		err := s.DB().QueryRow(
			`SELECT r.id, r.state FROM runs r JOIN jobs j ON j.id = r.job_id
			 WHERE j.service = ? ORDER BY r.id DESC LIMIT 1`, service,
		).Scan(&runID, &final)
		if err != nil {
			t.Fatalf("loading %s's run: %v", service, err)
		}
		rows, err := s.DB().Query(
			`SELECT from_state, to_state FROM run_transitions WHERE run_id = ? ORDER BY id`, runID)
		if err != nil {
			t.Fatalf("loading transitions: %v", err)
		}
		defer rows.Close()
		for rows.Next() {
			var from, to string
			if err := rows.Scan(&from, &to); err != nil {
				t.Fatal(err)
			}
			if len(path) == 0 {
				path = append(path, from)
			}
			path = append(path, to)
		}
		vrows, err := s.DB().Query(`SELECT method, passed FROM verification_results WHERE run_id = ?`, runID)
		if err != nil {
			t.Fatalf("loading verification results: %v", err)
		}
		defer vrows.Close()
		for vrows.Next() {
			var method string
			var passed bool
			if err := vrows.Scan(&method, &passed); err != nil {
				t.Fatal(err)
			}
			verifications[method] = passed
		}
	})
	return final, path, verifications
}

// TestWizardAcceptance_FirstVerifiedBackup is the spec §15 / §5.3.6
// acceptance check "fresh state → ballast wizard → first verified
// backup within 15 minutes": it asserts from the DB (not the wizard's
// own printed summary) that the first run passed through VERIFYING
// and ended SUCCESS with every recorded verification passed.
func TestWizardAcceptance_FirstVerifiedBackup(t *testing.T) {
	data := cliEnv(t, nil)
	withStubRestic(t)
	backupDir := newBackupSource(t)

	wizardInput(t, "folder", "myfolder", backupDir, "", "", "", "")

	start := time.Now()
	out, err := execCLI(t, "wizard")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("wizard: %v (output %q)", err, out)
	}
	if elapsed > 15*time.Minute {
		t.Errorf("wizard took %s, spec §15 requires <= 15 min", elapsed)
	}

	final, path, verifications := wizardRunHistory(t, data, "myfolder")
	t.Logf("run path: %s (wizard took %s)", strings.Join(path, "→"), elapsed)
	if final != string(engine.SUCCESS) {
		t.Fatalf("final run state = %s, want SUCCESS", final)
	}
	n := len(path)
	if n < 2 || path[n-2] != string(engine.VERIFYING) || path[n-1] != string(engine.SUCCESS) {
		t.Errorf("transition path %v does not end VERIFYING→SUCCESS", path)
	}
	if len(verifications) == 0 {
		t.Error("no verification results recorded for the wizard's first run")
	}
	for method, passed := range verifications {
		if !passed {
			t.Errorf("verification %s did not pass", method)
		}
	}
}

// TestWizardAcceptance_SuccessNotification checks the last hop of the
// spec §5.3.6 wizard flow: "first verified backup → success
// notification". A user who gives the wizard an ntfy topic must
// actually receive a notification for that first verified backup.
// The topic is given as a full URL pointing at a local fake ntfy
// server so no real network is touched.
func TestWizardAcceptance_SuccessNotification(t *testing.T) {
	cliEnv(t, nil)
	withStubRestic(t)
	backupDir := newBackupSource(t)

	var mu sync.Mutex
	var titles []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		titles = append(titles, r.Header.Get("Title"))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	topicURL := srv.URL + "/ballast-test"
	wizardInput(t, "folder", "myfolder", backupDir, "", "", topicURL, "")

	out, err := execCLI(t, "wizard")
	if err != nil {
		t.Fatalf("wizard: %v (output %q)", err, out)
	}
	// The ntfy link is a secret: echoed input aside, the wizard must
	// never print it back (only where it was saved).
	if strings.Count(out, topicURL) > 0 {
		t.Errorf("output repeats the ntfy link %q; want only the credential file path: %q", topicURL, out)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(titles) == 0 {
		t.Fatalf("no notification was delivered for the wizard's first verified backup (output %q)", out)
	}
	t.Logf("notifications received: %q", titles)
}

func TestWizardRequiredParameterCannotBeBlank(t *testing.T) {
	cliEnv(t, &spyRunner{})
	withStubRestic(t)

	wizardInput(t,
		"folder", // catalog entry
		"svc",    // service name
		"",       // required "path" parameter left blank: must fail
	)

	_, err := execCLI(t, "wizard")
	if err == nil {
		t.Error("wizard with a blank required parameter: want an error")
	}
}

// TestWizardReportsBackupFailure exercises the real pipeline failing
// (a "path" that does not exist on disk, caught by the files
// strategy) end to end, checking the wizard surfaces the failure and
// exits non-zero rather than silently claiming success.
func TestWizardReportsBackupFailure(t *testing.T) {
	cliEnv(t, nil)
	withStubRestic(t)

	missingDir := filepath.Join(t.TempDir(), "does-not-exist")
	wizardInput(t, "folder", "myfolder", missingDir, "", "", "", "")

	out, err := execCLI(t, "wizard")
	if err == nil {
		t.Error("wizard whose backup fails: want a non-zero exit")
	}
	if !strings.Contains(out, "myfolder: FAILED") || !strings.Contains(out, "What went wrong:") {
		t.Errorf("output = %q, want the failure and its cause surfaced", out)
	}
}

// resticStubScriptUnverifiableReadBack is a variant of the shared
// restic stub (cmd_test_helpers_test.go's resticStubScript) where
// every "dump" call fails. "backup" (the upload) still succeeds, but
// restic.Repository.ReadBack calls Dump to sample a file back out of
// the snapshot — so the upload succeeds while the read-back that
// proves the snapshot is restorable cannot complete. That is the only
// path in internal/pipeline.Pipeline.Run that produces
// engine.SUCCESS_UNVERIFIED (amber) for the "folder" catalog entry:
// its verify method is archive-integrity, but with zero strategy
// artifacts (spec: catalog/folder.yaml) the pipeline skips the
// declared checker and relies solely on restic's own read-back
// (internal/pipeline/pipeline.go's runChecks / anyUnverifiable).
const resticStubScriptUnverifiableReadBack = `#!/bin/sh
cmd="$1"
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
  dump) echo "stub restic dump: simulated destination read failure" >&2; exit 1 ;;
  restore) exit 0 ;;
  forget) printf '[]' ;;
  version) printf 'restic 0.17.3 compiled with go1.23.3 on linux/amd64\n' ;;
  *) echo "unknown stub restic command: $cmd" >&2; exit 2 ;;
esac
exit 0
`

// withStubResticUnverifiableReadBack installs
// resticStubScriptUnverifiableReadBack on $PATH, the same way
// withStubRestic installs the happy-path stub.
func withStubResticUnverifiableReadBack(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "restic")
	if err := os.WriteFile(path, []byte(resticStubScriptUnverifiableReadBack), 0o755); err != nil {
		t.Fatalf("writing restic stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestWizardReportsUnverifiedBackupAsAmber exercises the amber path of
// spec §4.1 / AGENTS rule 1 ("a backup is not done until verified;
// unverifiable = amber, never green") end to end through the wizard:
// the upload itself succeeds, but the read-back that proves it is
// restorable cannot be completed (destination unreadable), so the run
// must end SUCCESS_UNVERIFIED, never a plain SUCCESS. The wizard must
// report the WARNING/amber light, never claim the backup can be
// restored, and exit non-zero (Backup returns a non-nil error for
// SUCCESS_UNVERIFIED — the pipeline returns nil only for SUCCESS).
func TestWizardReportsUnverifiedBackupAsAmber(t *testing.T) {
	cliEnv(t, nil)
	withStubResticUnverifiableReadBack(t)
	backupDir := newBackupSource(t)

	wizardInput(t, "folder", "myfolder", backupDir, "", "", "", "")

	out, err := execCLI(t, "wizard")
	if err == nil {
		t.Error("wizard whose backup cannot be verified: want a non-zero exit")
	}
	for _, want := range []string{"myfolder: WARNING", "could NOT be checked"} {
		if !strings.Contains(out, want) {
			t.Errorf("output = %q, want it to contain %q", out, want)
		}
	}
	for _, notWant := range []string{"SUCCESS", "All done"} {
		if strings.Contains(out, notWant) {
			t.Errorf("output = %q, must not contain %q", out, notWant)
		}
	}

	statusOut, statusErr := execCLI(t, "status", "--service", "myfolder")
	if statusErr != nil {
		t.Fatalf("status: %v (output %q)", statusErr, statusOut)
	}
	if !strings.Contains(statusOut, "amber") {
		t.Errorf("status output = %q, want the amber light", statusOut)
	}
}

// TestWizardRejectsNonURLNtfyTopic: a bare topic name would be saved
// and then silently fail every notification, so it is refused up
// front with an example of the expected form.
func TestWizardRejectsNonURLNtfyTopic(t *testing.T) {
	cliEnv(t, nil)
	withStubRestic(t)
	backupDir := newBackupSource(t)

	wizardInput(t, "folder", "myfolder", backupDir, "", "", "my-topic", "")
	_, err := execCLI(t, "wizard")
	if err == nil || !strings.Contains(err.Error(), "https://") {
		t.Errorf("wizard with a bare ntfy topic: error = %v, want one explaining the full https:// link", err)
	}
}

// TestPrintTierResult: each light reads as plain English, amber is
// never presented as success, and a named tier is labelled so a
// second copy (Phase 2) can't hide behind the first.
func TestPrintTierResult(t *testing.T) {
	tests := []struct {
		name    string
		label   string
		tier    api.TierStatus
		want    []string
		notWant string
	}{
		{name: "green", label: "svc", tier: api.TierStatus{Light: "green"}, want: []string{"svc: SUCCESS", "can be restored"}},
		{name: "amber", label: "svc", tier: api.TierStatus{Light: "amber"}, want: []string{"svc: WARNING", "could NOT be checked"}, notWant: "SUCCESS"},
		{name: "red", label: "svc", tier: api.TierStatus{Light: "red", LastError: "disk full"}, want: []string{"svc: FAILED", "What went wrong: disk full"}},
		{name: "named tier", label: "svc (rclone copy)", tier: api.TierStatus{Light: "red"}, want: []string{"svc (rclone copy): FAILED"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var b strings.Builder
			printTierResult(&b, tt.label, tt.tier)
			for _, w := range tt.want {
				if !strings.Contains(b.String(), w) {
					t.Errorf("output = %q, want %q", b.String(), w)
				}
			}
			if tt.notWant != "" && strings.Contains(b.String(), tt.notWant) {
				t.Errorf("output = %q, must not contain %q", b.String(), tt.notWant)
			}
		})
	}
}

// TestWizardGeneratesAndReusesRepositoryPassword checks the wizard
// writes a repository password credential file itself (spec §5.3.6: a
// beginner is never asked to invent or paste a secret for the local
// destination path) and does not clobber it on a second run that
// reuses the same destination name.
func TestWizardGeneratesAndReusesRepositoryPassword(t *testing.T) {
	cliEnv(t, nil)
	withStubRestic(t)
	backupDir := newBackupSource(t)

	wizardInput(t, "folder", "svc1", backupDir, "", "", "", "")
	if _, err := execCLI(t, "wizard"); err != nil {
		t.Fatalf("first wizard run: %v", err)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	credPath := filepath.Join(cfg.CredentialsDir, "ballast-restic-password.local")
	first, err := os.ReadFile(credPath)
	if err != nil {
		t.Fatalf("reading generated credential: %v", err)
	}
	if len(strings.TrimSpace(string(first))) == 0 {
		t.Fatal("generated repository password is empty")
	}

	backupDir2 := newBackupSource(t)
	wizardInput(t, "folder", "svc2", backupDir2, "local", "", "", "")
	if _, err := execCLI(t, "wizard"); err != nil {
		t.Fatalf("second wizard run: %v", err)
	}
	second, err := os.ReadFile(credPath)
	if err != nil {
		t.Fatalf("reading credential after second run: %v", err)
	}
	if string(first) != string(second) {
		t.Error("second wizard run overwrote the existing repository password")
	}
}

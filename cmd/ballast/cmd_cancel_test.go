package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/springledev/ballast/internal/engine"
	"github.com/springledev/ballast/internal/scheduler"
	"github.com/springledev/ballast/internal/store"
)

// TestCLI_Cancel_Daemon: with a daemon running, `ballast cancel` asks
// it through the API and prints its answer on stdout.
func TestCLI_Cancel_Daemon(t *testing.T) {
	cliEnv(t, &spyRunner{})
	credDir := filepath.Join(os.Getenv("BALLAST_CONFIG_DIR"), "credentials")
	if err := os.MkdirAll(credDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(credDir, apiTokenCredential), []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/api/v1/services/immich/cancel":
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"run_id":7,"service":"immich","already_requested":false,"message":"Stopping the backup of immich. The last good backup is kept."}`))
		case "/api/v1/services/idle/cancel":
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"service idle: no backup of this service is running right now, so there is nothing to stop"}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	t.Setenv("BALLAST_URL", srv.URL)

	stdout, _, err := execCLISplit(t, "cancel", "immich")
	if err != nil || !strings.Contains(stdout, "Stopping the backup of immich") {
		t.Fatalf("cancel = %q, %v", stdout, err)
	}
	if !containsString(calls, "POST /api/v1/services/immich/cancel") {
		t.Errorf("daemon calls = %v", calls)
	}
	if _, _, err := execCLISplit(t, "cancel", "idle"); err == nil || !strings.Contains(err.Error(), "nothing to stop") {
		t.Errorf("cancel idle = %v, want the daemon's plain message", err)
	}
}

// TestCLI_Cancel_Direct: with no daemon, `ballast cancel` records the
// stop in the database for the process running the backup (here held
// by the test, standing in for `ballast backup` in another terminal).
func TestCLI_Cancel_Direct(t *testing.T) {
	data := cliEnv(t, &spyRunner{})
	seedTwoServices(t)
	if _, _, err := execCLISplit(t, "cancel", "immich"); err == nil || !strings.Contains(err.Error(), "nothing to stop") {
		t.Fatalf("cancel with nothing running = %v, want nothing to stop", err)
	}

	release, ok, err := scheduler.FileLocker{Dir: filepath.Join(data, "locks")}.TryLock("immich")
	if err != nil || !ok {
		t.Fatalf("holding the run lock: %v %v", ok, err)
	}
	defer release()
	var runID int64
	withDB(t, data, func(s *store.Store) {
		m, err := engine.StartRun(s.DB(), "immich")
		if err != nil {
			t.Fatal(err)
		}
		runID = m.Run().ID
	})

	stdout, stderr, err := execCLISplit(t, "cancel", "immich")
	if err != nil || !strings.Contains(stdout, "Stopping the backup of immich") {
		t.Fatalf("cancel = %q / %q, %v", stdout, stderr, err)
	}
	if !strings.Contains(stderr, "ballast serve") {
		t.Errorf("stderr = %q, want the no-daemon notice there", stderr)
	}
	withDB(t, data, func(s *store.Store) {
		asked, err := engine.CancelRequested(s.DB(), "immich")
		if err != nil || !asked {
			t.Errorf("stop request for run %d not recorded: %v %v", runID, asked, err)
		}
	})
	stdout, _, err = execCLISplit(t, "cancel", "immich")
	if err != nil || !strings.Contains(stdout, "already stopping") {
		t.Errorf("second cancel = %q, %v", stdout, err)
	}
}

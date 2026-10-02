package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/springledev/ballast/internal/api"
	"github.com/springledev/ballast/internal/config"
)

func TestResolveDaemonURL(t *testing.T) {
	tests := []struct {
		name         string
		flag, env    string
		listen       string
		want         string
		wantExplicit bool
	}{
		{name: "from listen", listen: "127.0.0.1:8000", want: "http://127.0.0.1:8000"},
		{name: "wildcard listen maps to loopback", listen: "0.0.0.0:8080", want: "http://127.0.0.1:8080"},
		{name: "empty host", listen: ":8080", want: "http://127.0.0.1:8080"},
		{name: "env var", env: "nas.lan:8080", listen: "127.0.0.1:8000", want: "http://nas.lan:8080", wantExplicit: true},
		{name: "flag beats env", flag: "https://b/", env: "a", listen: "127.0.0.1:8000", want: "https://b", wantExplicit: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("BALLAST_URL", tt.env)
			got, explicit := resolveDaemonURL(tt.flag, config.Config{Listen: tt.listen})
			if got != tt.want || explicit != tt.wantExplicit {
				t.Errorf("resolveDaemonURL = %q, %v; want %q, %v", got, explicit, tt.want, tt.wantExplicit)
			}
		})
	}
}

// fakeDaemon answers like `ballast serve`: 401 without the right token.
func fakeDaemon(t *testing.T, token string, calls *[]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, `{"error":"missing or invalid API token"}`, http.StatusUnauthorized)
			return
		}
		*calls = append(*calls, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/api/v1/services/immich/backup":
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"started":true,"previous_run_interrupted":false}`))
		case "/api/v1/services/immich/verify":
			_, _ = w.Write([]byte(`{"service":"immich","light":"green","checks":[{"method":"restic-read-back","passed":true,"detail":"fine"}]}`))
		case "/api/v1/verify":
			_, _ = w.Write([]byte(`[{"service":"immich","light":"red","error":"no snapshot"}]`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCommandsUseRunningDaemon(t *testing.T) {
	r := &spyRunner{}
	cliEnv(t, r)
	credDir := filepath.Join(os.Getenv("BALLAST_CONFIG_DIR"), "credentials")
	if err := os.MkdirAll(credDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(credDir, apiTokenCredential), []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var calls []string
	srv := fakeDaemon(t, "s3cret", &calls)
	t.Setenv("BALLAST_URL", srv.URL)

	out, err := execCLI(t, "backup", "immich")
	if err != nil || !strings.Contains(out, "backup started by the Ballast daemon") {
		t.Fatalf("backup = %q, %v", out, err)
	}
	out, err = execCLI(t, "verify", "immich")
	if err != nil || !strings.Contains(out, "restic-read-back: ok") {
		t.Fatalf("verify = %q, %v", out, err)
	}
	if _, err = execCLI(t, "verify", "--all"); err == nil || !strings.Contains(err.Error(), "immich") {
		t.Fatalf("verify --all error = %v, want a failure naming immich", err)
	}
	if len(r.ran) != 0 {
		t.Errorf("local pipeline ran %v; the daemon should have done the work", r.ran)
	}
	for _, want := range []string{"POST /api/v1/services/immich/backup", "POST /api/v1/services/immich/verify", "POST /api/v1/verify"} {
		if !containsString(calls, want) {
			t.Errorf("daemon calls %v missing %q", calls, want)
		}
	}
}

func TestDaemonRejectsToken(t *testing.T) {
	cliEnv(t, &spyRunner{})
	var calls []string
	srv := fakeDaemon(t, "right", &calls)
	t.Setenv("BALLAST_URL", srv.URL)
	_, err := execCLI(t, "backup", "immich")
	if err == nil || !strings.Contains(err.Error(), "rejected this machine's API token") {
		t.Fatalf("error = %v, want a token-rejected message", err)
	}
}

func TestNoDaemonFallsBackWithHint(t *testing.T) {
	r := &spyRunner{}
	cliEnv(t, r)
	out, err := execCLI(t, "backup", "immich")
	if err != nil {
		t.Fatalf("backup: %v (%s)", err, out)
	}
	if len(r.ran) != 1 || !strings.Contains(out, "ballast serve") {
		t.Errorf("ran=%v output=%q; want a local run plus a `ballast serve` hint", r.ran, out)
	}
}

func TestExplicitDaemonDownIsAnError(t *testing.T) {
	r := &spyRunner{}
	cliEnv(t, r)
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	_, err := execCLI(t, "backup", "immich", "--url", url)
	if err == nil || len(r.ran) != 0 {
		t.Fatalf("err=%v ran=%v; want an error and no local run", err, r.ran)
	}
}

func TestServeListenAliases(t *testing.T) {
	if f := serveCmd.Flags().Lookup("listen"); f == nil {
		t.Fatal("serve has no --listen flag")
	}
	if got := serveCmd.Flags().GetNormalizeFunc()(serveCmd.Flags(), "addr"); got != "listen" {
		t.Errorf("--addr normalizes to %q, want listen", got)
	}
}

// TestBackupViaRealDaemonMatchesStandalone proves the §18.3 criterion
// "daemon client and standalone fallback have identical outcomes": the
// same pipeline runs, ends SUCCESS, and a failing pipeline fails the
// same way, whether the CLI ran it itself or asked a real API handler.
func TestBackupViaRealDaemonMatchesStandalone(t *testing.T) {
	r := &spyRunner{}
	cliEnv(t, r)
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := execCLI(t, "backup", "immich"); err != nil {
		t.Fatalf("standalone backup: %v", err)
	}

	a, err := openApp(cfg, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	a.ops.Ctx = t.Context()
	srv := httptest.NewServer(api.NewHandler(a.ops, "tok"))
	defer srv.Close()
	credDir := filepath.Join(cfg.ConfigDir, "credentials")
	if err := os.MkdirAll(credDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(credDir, apiTokenCredential), []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := execCLI(t, "backup", "immich", "--url", srv.URL); err != nil {
		t.Fatalf("daemon backup: %v (%s)", err, out)
	}
	a.ops.Wait()
	if got := len(r.ran); got != 2 || r.ran[0] != "immich" || r.ran[1] != "immich" {
		t.Errorf("pipeline runs = %v, want immich twice (once standalone, once via the daemon)", r.ran)
	}
	a.Close()
}

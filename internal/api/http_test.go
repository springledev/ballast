package api

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/springledev/ballast/internal/backend"
	"github.com/springledev/ballast/internal/dockerhost"
	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/notify"
)

// fakeEd25519PrivateKey is a syntactically valid, unencrypted OpenSSH
// private key (the shape backend.RenderCredentials checks: BEGIN/END
// markers and a "PRIVATE KEY" body), used to exercise a docker host's
// --ssh-key credential without depending on ssh-keygen being
// installed. It is not a real key that connects to anything.
const fakeEd25519PrivateKey = "-----BEGIN OPENSSH PRIVATE KEY-----\n" +
	"ZmFrZS1rZXktY29udGVudHMtZm9yLXRlc3Rpbmc=\n" +
	"-----END OPENSSH PRIVATE KEY-----"

// -update regenerates every golden file from the handler's current
// behavior (spec §13: "golden-file REST contract tests"). Run:
//
//	go test ./internal/api/... -run TestHTTPGolden -update
var updateGolden = flag.Bool("update", false, "update golden files")

// goldenCase describes one request/response contract test.
type goldenCase struct {
	name        string
	arrange     func(t *testing.T, h *harness) // configure state before the request
	req         func(h *harness) *http.Request // the request to send
	skipAuth    bool                           // true: send no Authorization header (401 cases)
	badAuth     bool                           // true: send a wrong bearer token
	triggerAuth string                         // non-empty: X-Ballast-Trigger-Token value instead of bearer
}

const testAPIToken = "test-api-token"

func jsonBody(t *testing.T, v any) *bytes.Reader {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshaling request body: %v", err)
	}
	return bytes.NewReader(b)
}

// normalizeGolden replaces volatile values (trigger tokens, RFC3339
// timestamps, whose exact value depends on when the test ran) with a
// fixed placeholder, so golden files compare deterministically.
func normalizeGolden(t *testing.T, body []byte) json.RawMessage {
	t.Helper()
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		// Not JSON: leave as an opaque string so it still shows in
		// the golden file and mismatches are visible.
		b, _ := json.Marshal(string(body))
		return b
	}
	normalizeValue(v)
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("re-marshaling normalized body: %v", err)
	}
	return out
}

func normalizeValue(v any) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if k == "token" {
				t[k] = "<token>"
				continue
			}
			if s, ok := val.(string); ok && looksLikeTimestamp(s) {
				t[k] = "<time>"
				continue
			}
			normalizeValue(val)
		}
	case []any:
		for _, e := range t {
			normalizeValue(e)
		}
	}
}

func looksLikeTimestamp(s string) bool {
	_, err := time.Parse(time.RFC3339Nano, s)
	return err == nil
}

// goldenRecord is what's persisted to disk: status code plus the
// normalized body.
type goldenRecord struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body,omitempty"`
}

func runGolden(t *testing.T, tc goldenCase) {
	t.Run(tc.name, func(t *testing.T) {
		h := newHarness(t)
		if tc.arrange != nil {
			tc.arrange(t, h)
		}
		req := tc.req(h)
		switch {
		case tc.skipAuth:
			// no Authorization header
		case tc.badAuth:
			req.Header.Set("Authorization", "Bearer wrong-token")
		case tc.triggerAuth == "use-harness-token":
			req.Header.Set("X-Ballast-Trigger-Token", h.triggerToken)
		case tc.triggerAuth != "":
			req.Header.Set("X-Ballast-Trigger-Token", tc.triggerAuth)
		default:
			req.Header.Set("Authorization", "Bearer "+testAPIToken)
		}

		rec := httptest.NewRecorder()
		NewHandler(h.ops, testAPIToken).ServeHTTP(rec, req)

		got := goldenRecord{Status: rec.Code, Body: normalizeGolden(t, rec.Body.Bytes())}
		gotJSON, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatalf("marshaling golden record: %v", err)
		}
		gotJSON = append(gotJSON, '\n')

		goldenPath := filepath.Join("testdata", "golden", tc.name+".json")
		if *updateGolden {
			if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
				t.Fatalf("creating golden dir: %v", err)
			}
			if err := os.WriteFile(goldenPath, gotJSON, 0o644); err != nil {
				t.Fatalf("writing golden file: %v", err)
			}
			return
		}
		want, err := os.ReadFile(goldenPath)
		if err != nil {
			t.Fatalf("reading golden file %s: %v (run with -update to create it)", goldenPath, err)
		}
		if string(want) != string(gotJSON) {
			t.Errorf("response for %s does not match golden file %s:\n--- got ---\n%s--- want ---\n%s", tc.name, goldenPath, gotJSON, want)
		}
	})
}

// withService arranges a destination "local" and a service "svc"
// backed by the "folder" catalog entry.
func withService(t *testing.T, h *harness) {
	t.Helper()
	h.addDestination("local")
	if err := inventory.PutService(h.ops.DB, inventory.Service{
		Name: "svc", CatalogEntry: "folder", Destination: "local", Params: map[string]string{"path": "/x"},
	}); err != nil {
		t.Fatalf("PutService: %v", err)
	}
}

// TestHTTPGolden covers every route at least once (spec §14 step 11
// D4), plus 401/404/409 and the token-shown-once guarantee.
func TestHTTPGolden(t *testing.T) {
	cases := []goldenCase{
		{
			name: "status_empty",
			req:  func(h *harness) *http.Request { return httptest.NewRequest(http.MethodGet, "/api/v1/status", nil) },
		},
		{
			name:    "status_one_service",
			arrange: withService,
			req: func(h *harness) *http.Request {
				return httptest.NewRequest(http.MethodGet, "/api/v1/status?service=svc&verify=1", nil)
			},
		},
		{
			name: "catalog_list",
			req:  func(h *harness) *http.Request { return httptest.NewRequest(http.MethodGet, "/api/v1/catalog", nil) },
		},
		{
			name: "detect_not_configured",
			req:  func(h *harness) *http.Request { return httptest.NewRequest(http.MethodGet, "/api/v1/detect", nil) },
		},
		{
			name:    "detect_ok",
			arrange: func(t *testing.T, h *harness) { h.withStubDocker(t) },
			req:     func(h *harness) *http.Request { return httptest.NewRequest(http.MethodGet, "/api/v1/detect", nil) },
		},
		{
			// preflight never itself 500s (see handler.preflight): with
			// no Runner configured it still returns 200, explaining why
			// in both tool fields.
			name: "preflight_no_runner",
			req:  func(h *harness) *http.Request { return httptest.NewRequest(http.MethodGet, "/api/v1/preflight", nil) },
		},
		{
			name: "services_list_empty",
			req:  func(h *harness) *http.Request { return httptest.NewRequest(http.MethodGet, "/api/v1/services", nil) },
		},
		{
			name:    "services_create",
			arrange: func(t *testing.T, h *harness) { h.addDestination("local") },
			req: func(h *harness) *http.Request {
				body := inventory.Service{Name: "svc", CatalogEntry: "folder", Destination: "local", Params: map[string]string{"path": "/x"}}
				r := httptest.NewRequest(http.MethodPost, "/api/v1/services", jsonBody(t, body))
				r.Header.Set("Content-Type", "application/json")
				return r
			},
		},
		{
			name:    "services_get",
			arrange: withService,
			req: func(h *harness) *http.Request {
				return httptest.NewRequest(http.MethodGet, "/api/v1/services/svc", nil)
			},
		},
		{
			name: "services_get_not_found",
			req: func(h *harness) *http.Request {
				return httptest.NewRequest(http.MethodGet, "/api/v1/services/nope", nil)
			},
		},
		{
			name:    "services_put",
			arrange: withService,
			req: func(h *harness) *http.Request {
				body := inventory.Service{CatalogEntry: "folder", Destination: "local", Params: map[string]string{"path": "/y"}}
				r := httptest.NewRequest(http.MethodPut, "/api/v1/services/svc", jsonBody(t, body))
				r.Header.Set("Content-Type", "application/json")
				return r
			},
		},
		{
			// A per-service retention override must round-trip through
			// PUT (spec §14 step 19: keep_daily/weekly/monthly overrides
			// the destination's own policy).
			name:    "services_put_retention",
			arrange: withService,
			req: func(h *harness) *http.Request {
				body := inventory.Service{
					CatalogEntry: "folder", Destination: "local", Params: map[string]string{"path": "/y"},
					Retention: &inventory.Retention{KeepDaily: 3, KeepWeekly: 0, KeepMonthly: 0},
				}
				r := httptest.NewRequest(http.MethodPut, "/api/v1/services/svc", jsonBody(t, body))
				r.Header.Set("Content-Type", "application/json")
				return r
			},
		},
		{
			// An all-zero retention override keeps nothing, which is
			// never valid (it would delete every backup): the server
			// rejects it with a plain-English 400, not a 500.
			name:    "services_put_retention_invalid",
			arrange: withService,
			req: func(h *harness) *http.Request {
				body := inventory.Service{
					CatalogEntry: "folder", Destination: "local", Params: map[string]string{"path": "/y"},
					Retention: &inventory.Retention{},
				}
				r := httptest.NewRequest(http.MethodPut, "/api/v1/services/svc", jsonBody(t, body))
				r.Header.Set("Content-Type", "application/json")
				return r
			},
		},
		{
			name:    "services_delete",
			arrange: withService,
			req: func(h *harness) *http.Request {
				return httptest.NewRequest(http.MethodDelete, "/api/v1/services/svc", nil)
			},
		},
		{
			name:    "backup_start",
			arrange: withService,
			req: func(h *harness) *http.Request {
				return httptest.NewRequest(http.MethodPost, "/api/v1/services/svc/backup", nil)
			},
		},
		{
			name:    "backup_busy",
			arrange: func(t *testing.T, h *harness) { withService(t, h); h.holdRunLock(t, "svc") },
			req: func(h *harness) *http.Request {
				return httptest.NewRequest(http.MethodPost, "/api/v1/services/svc/backup", nil)
			},
		},
		{
			name:    "snapshots_list",
			arrange: withService,
			req: func(h *harness) *http.Request {
				return httptest.NewRequest(http.MethodGet, "/api/v1/services/svc/snapshots", nil)
			},
		},
		{
			name:    "verify_service",
			arrange: withService,
			req: func(h *harness) *http.Request {
				return httptest.NewRequest(http.MethodPost, "/api/v1/services/svc/verify", nil)
			},
		},
		{
			name: "verify_service_not_found",
			req: func(h *harness) *http.Request {
				return httptest.NewRequest(http.MethodPost, "/api/v1/services/nope/verify", nil)
			},
		},
		{
			name:    "verify_all",
			arrange: withService,
			req: func(h *harness) *http.Request {
				return httptest.NewRequest(http.MethodPost, "/api/v1/verify", nil)
			},
		},
		{
			name:    "restore",
			arrange: withService,
			req: func(h *harness) *http.Request {
				body := restoreRequestBody{To: filepath.Join(h.t.TempDir(), "restore-out")}
				r := httptest.NewRequest(http.MethodPost, "/api/v1/services/svc/restore", jsonBody(t, body))
				r.Header.Set("Content-Type", "application/json")
				return r
			},
		},
		{
			name:    "restore_invalid_no_target",
			arrange: withService,
			req: func(h *harness) *http.Request {
				r := httptest.NewRequest(http.MethodPost, "/api/v1/services/svc/restore", jsonBody(t, restoreRequestBody{}))
				r.Header.Set("Content-Type", "application/json")
				return r
			},
		},
		{
			name:    "schedule_get_not_found",
			arrange: withService,
			req: func(h *harness) *http.Request {
				return httptest.NewRequest(http.MethodGet, "/api/v1/services/svc/schedule", nil)
			},
		},
		{
			name:    "schedule_put",
			arrange: withService,
			req: func(h *harness) *http.Request {
				r := httptest.NewRequest(http.MethodPut, "/api/v1/services/svc/schedule", jsonBody(t, scheduleRequestBody{Cron: "0 3 * * *"}))
				r.Header.Set("Content-Type", "application/json")
				return r
			},
		},
		{
			name: "schedule_get",
			arrange: func(t *testing.T, h *harness) {
				withService(t, h)
				if err := h.ops.SetSchedule(context.Background(), "svc", "0 3 * * *"); err != nil {
					t.Fatalf("SetSchedule: %v", err)
				}
			},
			req: func(h *harness) *http.Request {
				return httptest.NewRequest(http.MethodGet, "/api/v1/services/svc/schedule", nil)
			},
		},
		{
			name: "schedule_enabled_put",
			arrange: func(t *testing.T, h *harness) {
				withService(t, h)
				if err := h.ops.SetSchedule(context.Background(), "svc", "0 3 * * *"); err != nil {
					t.Fatalf("SetSchedule: %v", err)
				}
			},
			req: func(h *harness) *http.Request {
				r := httptest.NewRequest(http.MethodPut, "/api/v1/services/svc/schedule/enabled", jsonBody(t, scheduleEnabledBody{Enabled: false}))
				r.Header.Set("Content-Type", "application/json")
				return r
			},
		},
		{
			name:    "trigger_token_generate",
			arrange: withService,
			req: func(h *harness) *http.Request {
				return httptest.NewRequest(http.MethodPost, "/api/v1/services/svc/trigger-token", nil)
			},
		},
		{
			name: "trigger_bad_token",
			arrange: func(t *testing.T, h *harness) {
				withService(t, h)
				if _, err := h.ops.GenerateTriggerToken(context.Background(), "svc"); err != nil {
					t.Fatalf("GenerateTriggerToken: %v", err)
				}
			},
			req: func(h *harness) *http.Request {
				return httptest.NewRequest(http.MethodPost, "/api/v1/services/svc/trigger", nil)
			},
			triggerAuth: "wrong-token",
		},
		{
			name: "trigger_ok",
			arrange: func(t *testing.T, h *harness) {
				withService(t, h)
				tok, err := h.ops.GenerateTriggerToken(context.Background(), "svc")
				if err != nil {
					t.Fatalf("GenerateTriggerToken: %v", err)
				}
				h.triggerToken = tok
			},
			req: func(h *harness) *http.Request {
				return httptest.NewRequest(http.MethodPost, "/api/v1/services/svc/trigger", nil)
			},
			triggerAuth: "use-harness-token",
		},
		{
			name:    "notifications_get_default",
			arrange: withService,
			req: func(h *harness) *http.Request {
				return httptest.NewRequest(http.MethodGet, "/api/v1/services/svc/notifications", nil)
			},
		},
		{
			name:    "notifications_put",
			arrange: withService,
			req: func(h *harness) *http.Request {
				body := map[string]any{"enabled": false, "max_age_hours": 72, "heartbeat_enabled": true}
				r := httptest.NewRequest(http.MethodPut, "/api/v1/services/svc/notifications", jsonBody(t, body))
				r.Header.Set("Content-Type", "application/json")
				return r
			},
		},
		{
			// Leaving out "enabled" must be a 400, never a silent
			// switch-off of missing-backup alerts.
			name:    "notifications_put_missing_enabled",
			arrange: withService,
			req: func(h *harness) *http.Request {
				body := map[string]any{"heartbeat_enabled": true}
				r := httptest.NewRequest(http.MethodPut, "/api/v1/services/svc/notifications", jsonBody(t, body))
				r.Header.Set("Content-Type", "application/json")
				return r
			},
		},
		{
			name:    "notifications_put_invalid_max_age",
			arrange: withService,
			req: func(h *harness) *http.Request {
				body := map[string]any{"enabled": true, "max_age_hours": 0}
				r := httptest.NewRequest(http.MethodPut, "/api/v1/services/svc/notifications", jsonBody(t, body))
				r.Header.Set("Content-Type", "application/json")
				return r
			},
		},
		{
			name: "notifications_put_unknown_service",
			req: func(h *harness) *http.Request {
				body := map[string]any{"enabled": true}
				r := httptest.NewRequest(http.MethodPut, "/api/v1/services/nope/notifications", jsonBody(t, body))
				r.Header.Set("Content-Type", "application/json")
				return r
			},
		},
		{
			name: "notifications_get_unknown_service",
			req: func(h *harness) *http.Request {
				return httptest.NewRequest(http.MethodGet, "/api/v1/services/nope/notifications", nil)
			},
		},
		{
			name: "notifications_get_after_put",
			arrange: func(t *testing.T, h *harness) {
				withService(t, h)
				maxAge := 72
				err := h.ops.SetNotificationSettings(context.Background(), "svc", notify.Settings{
					Enabled: false, MaxAgeHours: &maxAge, HeartbeatEnabled: true,
				})
				if err != nil {
					t.Fatalf("SetNotificationSettings: %v", err)
				}
			},
			req: func(h *harness) *http.Request {
				return httptest.NewRequest(http.MethodGet, "/api/v1/services/svc/notifications", nil)
			},
		},
		{
			name:    "secrets_put",
			arrange: withService,
			req: func(h *harness) *http.Request {
				body := map[string]any{"value": "hunter2"}
				r := httptest.NewRequest(http.MethodPut, "/api/v1/services/svc/secrets/db_password", jsonBody(t, body))
				r.Header.Set("Content-Type", "application/json")
				return r
			},
		},
		{
			name:    "secrets_put_empty_value",
			arrange: withService,
			req: func(h *harness) *http.Request {
				body := map[string]any{"value": ""}
				r := httptest.NewRequest(http.MethodPut, "/api/v1/services/svc/secrets/db_password", jsonBody(t, body))
				r.Header.Set("Content-Type", "application/json")
				return r
			},
		},
		{
			name: "secrets_put_unknown_service",
			req: func(h *harness) *http.Request {
				body := map[string]any{"value": "hunter2"}
				r := httptest.NewRequest(http.MethodPut, "/api/v1/services/nope/secrets/db_password", jsonBody(t, body))
				r.Header.Set("Content-Type", "application/json")
				return r
			},
		},
		{
			name: "destinations_list_empty",
			req: func(h *harness) *http.Request {
				return httptest.NewRequest(http.MethodGet, "/api/v1/destinations", nil)
			},
		},
		{
			name: "destinations_create",
			req: func(h *harness) *http.Request {
				body := createDestinationBody{Destination: inventory.Destination{Name: "local", Location: "/stub-repo", KeepDaily: 7}}
				r := httptest.NewRequest(http.MethodPost, "/api/v1/destinations", jsonBody(t, body))
				r.Header.Set("Content-Type", "application/json")
				return r
			},
		},
		{
			// Init=false so no restic call is made; this case exists to
			// prove the response never echoes the posted credentials
			// back (spec rule 2), which the golden file itself
			// demonstrates and TestHTTPGoldenDestinationCreateWithCredentialsNoLeak
			// asserts explicitly.
			name: "destinations_create_with_credentials",
			req: func(h *harness) *http.Request {
				body := createDestinationBody{
					Destination: inventory.Destination{
						Name: "aws", Type: backend.S3,
						Settings:  backend.Settings{Endpoint: "s3.amazonaws.com", Bucket: "my-bucket", Region: "us-east-1"},
						KeepDaily: 7,
					},
					Credentials: backend.Credentials{AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "topsecretkey"},
					Init:        false,
				}
				r := httptest.NewRequest(http.MethodPost, "/api/v1/destinations", jsonBody(t, body))
				r.Header.Set("Content-Type", "application/json")
				return r
			},
		},
		{
			name:    "destinations_test",
			arrange: func(t *testing.T, h *harness) { h.addDestination("local") },
			req: func(h *harness) *http.Request {
				return httptest.NewRequest(http.MethodPost, "/api/v1/destinations/local/test", nil)
			},
		},
		{
			name:    "destinations_init",
			arrange: func(t *testing.T, h *harness) { h.addDestination("local") },
			req: func(h *harness) *http.Request {
				return httptest.NewRequest(http.MethodPost, "/api/v1/destinations/local/init", nil)
			},
		},
		{
			name:    "destinations_delete",
			arrange: func(t *testing.T, h *harness) { h.addDestination("local") },
			req: func(h *harness) *http.Request {
				return httptest.NewRequest(http.MethodDelete, "/api/v1/destinations/local", nil)
			},
		},
		{
			name:    "destinations_delete_in_use",
			arrange: withService,
			req: func(h *harness) *http.Request {
				return httptest.NewRequest(http.MethodDelete, "/api/v1/destinations/local", nil)
			},
		},
		{
			name: "docker_hosts_list",
			req: func(h *harness) *http.Request {
				return httptest.NewRequest(http.MethodGet, "/api/v1/docker-hosts", nil)
			},
		},
		{
			name: "docker_hosts_get",
			req: func(h *harness) *http.Request {
				return httptest.NewRequest(http.MethodGet, "/api/v1/docker-hosts/local", nil)
			},
		},
		{
			name: "docker_hosts_get_not_found",
			req: func(h *harness) *http.Request {
				return httptest.NewRequest(http.MethodGet, "/api/v1/docker-hosts/nope", nil)
			},
		},
		{
			// kind remote-ssh with no address credentials and
			// skip_test: true so no connection is attempted; this
			// case exists to prove the response never echoes the
			// posted credentials back (spec rule 2).
			name: "docker_hosts_create_skip_test",
			req: func(h *harness) *http.Request {
				body := createDockerHostBody{
					Host:     dockerhost.Host{Name: "rpi", Kind: dockerhost.RemoteSSH, Address: "ballast@192.168.1.20"},
					SkipTest: true,
				}
				r := httptest.NewRequest(http.MethodPost, "/api/v1/docker-hosts", jsonBody(t, body))
				r.Header.Set("Content-Type", "application/json")
				return r
			},
		},
		{
			// A real (fake) SSH private key, still with skip_test so
			// no connection is made, exercising credential
			// persistence; the golden file demonstrates the key never
			// appears in the response and
			// TestDockerHostCreateWithCredentialsNoLeak asserts it
			// explicitly.
			name: "docker_hosts_create_with_credentials",
			req: func(h *harness) *http.Request {
				body := createDockerHostBody{
					Host:        dockerhost.Host{Name: "rpi", Kind: dockerhost.RemoteSSH, Address: "ballast@192.168.1.20"},
					Credentials: dockerhost.Credentials{SSHPrivateKey: fakeEd25519PrivateKey},
					SkipTest:    true,
				}
				r := httptest.NewRequest(http.MethodPost, "/api/v1/docker-hosts", jsonBody(t, body))
				r.Header.Set("Content-Type", "application/json")
				return r
			},
		},
		{
			name: "docker_hosts_delete",
			arrange: func(t *testing.T, h *harness) {
				if _, err := h.ops.SetupDockerHost(context.Background(), DockerHostSetup{
					Host:     dockerhost.Host{Name: "rpi", Kind: dockerhost.RemoteSSH, Address: "ballast@192.168.1.20"},
					SkipTest: true,
				}); err != nil {
					t.Fatalf("SetupDockerHost: %v", err)
				}
			},
			req: func(h *harness) *http.Request {
				return httptest.NewRequest(http.MethodDelete, "/api/v1/docker-hosts/rpi", nil)
			},
		},
		{
			// The built-in local host can never be removed (400, not
			// 204): spec §17.2.
			name: "docker_hosts_delete_local",
			req: func(h *harness) *http.Request {
				return httptest.NewRequest(http.MethodDelete, "/api/v1/docker-hosts/local", nil)
			},
		},
		{
			name:     "unauthorized",
			req:      func(h *harness) *http.Request { return httptest.NewRequest(http.MethodGet, "/api/v1/status", nil) },
			skipAuth: true,
		},
		{
			name:    "bad_token",
			req:     func(h *harness) *http.Request { return httptest.NewRequest(http.MethodGet, "/api/v1/status", nil) },
			badAuth: true,
		},
	}
	for _, tc := range cases {
		runGolden(t, tc)
	}
}

// TestDestinationsCreateWithCredentialsNoLeak makes the same request
// as the "destinations_create_with_credentials" golden case directly
// (not through the golden-file comparison) and asserts explicitly that
// the secret access key never appears in the response body — the
// golden file passing already proves this by construction, but this
// spells it out (spec rule 2: secrets never appear in an API
// response).
func TestDestinationsCreateWithCredentialsNoLeak(t *testing.T) {
	h := newHarness(t)
	body := createDestinationBody{
		Destination: inventory.Destination{
			Name: "aws", Type: backend.S3,
			Settings:  backend.Settings{Endpoint: "s3.amazonaws.com", Bucket: "my-bucket", Region: "us-east-1"},
			KeepDaily: 7,
		},
		Credentials: backend.Credentials{AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "topsecretkey"},
		Init:        false,
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/destinations", jsonBody(t, body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testAPIToken)

	rec := httptest.NewRecorder()
	NewHandler(h.ops, testAPIToken).ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "topsecretkey") {
		t.Errorf("response body leaks the secret access key: %s", rec.Body.String())
	}
}

// TestDockerHostCreateWithCredentialsNoLeak makes the same request as
// the "docker_hosts_create_with_credentials" golden case directly (not
// through the golden-file comparison) and asserts explicitly that the
// SSH private key never appears in the response body (spec rule 2).
func TestDockerHostCreateWithCredentialsNoLeak(t *testing.T) {
	h := newHarness(t)
	body := createDockerHostBody{
		Host:        dockerhost.Host{Name: "rpi", Kind: dockerhost.RemoteSSH, Address: "ballast@192.168.1.20"},
		Credentials: dockerhost.Credentials{SSHPrivateKey: fakeEd25519PrivateKey},
		SkipTest:    true,
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/docker-hosts", jsonBody(t, body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testAPIToken)

	rec := httptest.NewRecorder()
	NewHandler(h.ops, testAPIToken).ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "fake-key-contents-for-testing") || strings.Contains(rec.Body.String(), fakeEd25519PrivateKey) {
		t.Errorf("response body leaks the SSH private key: %s", rec.Body.String())
	}
}

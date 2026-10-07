package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/springledev/ballast/internal/backend"
	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/repohealth"
	"github.com/springledev/ballast/internal/store"
)

func TestRepoHealthEndpoints(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/b.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := inventory.PutDestination(st.DB(), inventory.Destination{Name: "nas", Type: backend.Local, Settings: backend.Settings{Path: "/srv/b"}, PasswordCredential: "pw", KeepDaily: 7}); err != nil {
		t.Fatal(err)
	}
	ops := &Ops{DB: st.DB(), RepoHealth: &repohealth.Checker{DB: st.DB()}}
	h := NewHandler(ops, "tok")
	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer tok")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	for _, tc := range []struct {
		name, method, path, body string
		status                   int
		contains                 string
	}{
		{"list", "GET", "/api/v1/destinations/health", "", 200, `"interval_hours":168`},
		{"get", "GET", "/api/v1/destinations/nas/health", "", 200, `"light":"grey"`},
		{"get unknown", "GET", "/api/v1/destinations/ghost/health", "", 404, ""},
		{"configure", "PUT", "/api/v1/destinations/nas/health", `{"enabled":true,"interval_hours":24,"subset":"10%"}`, 200, `"subset":"10%"`},
		{"bad subset", "PUT", "/api/v1/destinations/nas/health", `{"enabled":true,"interval_hours":24,"subset":"lots"}`, 400, "not a valid share"},
		{"bad interval", "PUT", "/api/v1/destinations/nas/health", `{"enabled":true,"interval_hours":0}`, 400, "at least 1"},
		{"unknown field", "PUT", "/api/v1/destinations/nas/health", `{"nope":1}`, 400, ""},
		{"check without repo wiring", "POST", "/api/v1/destinations/nas/health/check", "", 200, `"state":"amber"`},
	} {
		rec := do(tc.method, tc.path, tc.body)
		if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.contains) {
			t.Errorf("%s: %d %s, want %d containing %q", tc.name, rec.Code, rec.Body.String(), tc.status, tc.contains)
		}
	}
	// Not wired at all: a loud error, not a panic.
	ops.RepoHealth = nil
	if rec := do("GET", "/api/v1/destinations/health", ""); rec.Code != http.StatusInternalServerError {
		t.Errorf("unwired: %d", rec.Code)
	}
}

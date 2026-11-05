package api

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/springledev/ballast/internal/store"
)

func openSessionStore(t *testing.T, path string) *store.Store {
	t.Helper()
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func sessionStatus(h http.Handler, c *http.Cookie) int {
	req := httptest.NewRequest("GET", "/api/v1/auth/session", nil)
	if c != nil {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestSessionsSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "b.db")
	s1 := openSessionStore(t, path)
	h1 := NewAuthedHandler(&Ops{}, NewAuth("k", nil, "").WithStore(s1))
	cookie := login(t, h1, "k", "1.1.1.1").Result().Cookies()[0]
	if got := sessionStatus(h1, cookie); got != 200 {
		t.Fatalf("before restart = %d", got)
	}
	s1.Close()

	s2 := openSessionStore(t, path)
	h2 := NewAuthedHandler(&Ops{}, NewAuth("k", nil, "").WithStore(s2))
	if got := sessionStatus(h2, cookie); got != 200 {
		t.Fatalf("after restart = %d, want 200", got)
	}
}

func TestPersistentSessionRejections(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(t *testing.T, a *Auth, h http.Handler, c *http.Cookie) *http.Cookie
	}{
		{"logout", func(t *testing.T, a *Auth, h http.Handler, c *http.Cookie) *http.Cookie {
			req := httptest.NewRequest("POST", "/api/v1/auth/logout", nil)
			req.AddCookie(c)
			h.ServeHTTP(httptest.NewRecorder(), req)
			return c
		}},
		{"unknown", func(t *testing.T, a *Auth, h http.Handler, c *http.Cookie) *http.Cookie {
			return &http.Cookie{Name: SessionCookie, Value: strings.Repeat("ab", 32)}
		}},
		{"tampered", func(t *testing.T, a *Auth, h http.Handler, c *http.Cookie) *http.Cookie {
			v := c.Value[:len(c.Value)-1] + "x"
			return &http.Cookie{Name: SessionCookie, Value: v}
		}},
		{"expired", func(t *testing.T, a *Auth, h http.Handler, c *http.Cookie) *http.Cookie {
			a.now = func() time.Time { return time.Now().Add(sessionLifetime + time.Hour) }
			return c
		}},
		{"rotate", func(t *testing.T, a *Auth, h http.Handler, c *http.Cookie) *http.Cookie {
			if _, err := a.Rotate(); err != nil {
				t.Fatal(err)
			}
			return c
		}},
		{"sign out everywhere", func(t *testing.T, a *Auth, h http.Handler, c *http.Cookie) *http.Cookie {
			req := httptest.NewRequest("POST", "/api/v1/auth/sign-out-everywhere", nil)
			req.Header.Set("Authorization", "Bearer k")
			a.signOutEverywhere(httptest.NewRecorder(), req)
			return c
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openSessionStore(t, filepath.Join(t.TempDir(), "b.db"))
			a := NewAuth("k", nil, "").WithStore(s)
			h := NewAuthedHandler(&Ops{}, a)
			c := login(t, h, "k", "1.1.1.1").Result().Cookies()[0]
			other := login(t, h, "k", "1.1.1.2").Result().Cookies()[0]
			use := tc.mutate(t, a, h, c)
			if got := sessionStatus(h, use); got != 401 {
				t.Fatalf("status = %d, want 401", got)
			}
			if tc.name == "expired" {
				n, err := s.CountSessions()
				if err != nil || n != 1 {
					t.Errorf("rows after expired lookup = %d, %v; want 1 (the looked-up one pruned, the other left for login to sweep)", n, err)
				}
			}
			if tc.name == "logout" {
				if got := sessionStatus(h, other); got != 200 {
					t.Errorf("other session after logout = %d, want 200", got)
				}
			}
		})
	}
}

func TestLoginPrunesExpiredSessions(t *testing.T) {
	s := openSessionStore(t, filepath.Join(t.TempDir(), "b.db"))
	a := NewAuth("k", nil, "").WithStore(s)
	h := NewAuthedHandler(&Ops{}, a)
	login(t, h, "k", "1.1.1.1")
	login(t, h, "k", "1.1.1.2")
	a.now = func() time.Time { return time.Now().Add(sessionLifetime + time.Hour) }
	login(t, h, "k", "1.1.1.3")
	if n, _ := s.CountSessions(); n != 1 {
		t.Fatalf("rows = %d, want 1 (expired ones pruned at login)", n)
	}
}

func TestRawSessionTokenNeverStored(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	path := filepath.Join(t.TempDir(), "b.db")
	s := openSessionStore(t, path)
	h := NewAuthedHandler(&Ops{}, NewAuth("k", nil, "").WithStore(s))
	rec := login(t, h, "k", "1.1.1.1")
	raw := rec.Result().Cookies()[0].Value
	if len(raw) < 32 {
		t.Fatalf("cookie value too short: %q", raw)
	}
	if strings.Contains(rec.Body.String(), raw) || strings.Contains(logs.String(), raw) {
		t.Fatal("raw session token appears in the response body or logs")
	}
	// Flush the WAL so the main file holds everything, then scan every file.
	if _, err := s.DB().Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(path + "*")
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte(raw)) {
			t.Errorf("raw session token found in %s", f)
		}
	}
	if n, _ := s.CountSessions(); n != 1 {
		t.Fatalf("expected one stored session, got %d", n)
	}
}

// breakSessionDeletes makes every DELETE on sessions fail, to prove the
// auth code fails closed.
func breakSessionDeletes(t *testing.T, s *store.Store) {
	t.Helper()
	if _, err := s.DB().Exec(`CREATE TRIGGER no_session_delete BEFORE DELETE ON sessions BEGIN SELECT RAISE(ABORT, 'disk trouble'); END`); err != nil {
		t.Fatal(err)
	}
}

func TestRotateFailsClosedWhenSessionsCannotBeCleared(t *testing.T) {
	s := openSessionStore(t, filepath.Join(t.TempDir(), "b.db"))
	saved := ""
	a := NewAuth("old", func(tok string) error { saved = tok; return nil }, "").WithStore(s)
	h := NewAuthedHandler(&Ops{}, a)
	cookie := login(t, h, "old", "1.1.1.1").Result().Cookies()[0]
	breakSessionDeletes(t, s)

	req := httptest.NewRequest("POST", "/api/v1/auth/rotate", nil)
	req.Header.Set("Authorization", "Bearer old")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 500 || saved != "" {
		t.Fatalf("rotate = %d, saved %q; want 500 and nothing saved", rec.Code, saved)
	}
	if !a.tokenMatches("old") {
		t.Error("old key should still work when rotation fails")
	}
	if got := sessionStatus(h, cookie); got != 200 {
		t.Errorf("existing session = %d; rotation failed so it should be untouched", got)
	}
	if strings.Contains(rec.Body.String(), "could not save the new access key") {
		t.Errorf("misleading message: %s", rec.Body.String())
	}
}

func TestSignOutEverywhereReportsFailure(t *testing.T) {
	s := openSessionStore(t, filepath.Join(t.TempDir(), "b.db"))
	a := NewAuth("k", nil, "").WithStore(s)
	h := NewAuthedHandler(&Ops{}, a)
	login(t, h, "k", "1.1.1.1")
	breakSessionDeletes(t, s)
	rec := httptest.NewRecorder()
	a.signOutEverywhere(rec, httptest.NewRequest("POST", "/", nil))
	if rec.Code != 500 {
		t.Fatalf("code = %d, want 500", rec.Code)
	}
}

func TestLogoutFailureStillRevokesSession(t *testing.T) {
	s := openSessionStore(t, filepath.Join(t.TempDir(), "b.db"))
	a := NewAuth("k", nil, "").WithStore(s)
	h := NewAuthedHandler(&Ops{}, a)
	cookie := login(t, h, "k", "1.1.1.1").Result().Cookies()[0]
	breakSessionDeletes(t, s)

	req := httptest.NewRequest("POST", "/api/v1/auth/logout", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), "could not sign you out completely") {
		t.Fatalf("logout = %d %s", rec.Code, rec.Body.String())
	}
	cleared := false
	for _, c := range rec.Result().Cookies() {
		if c.Name == SessionCookie && c.Value == "" {
			cleared = true
		}
	}
	if !cleared {
		t.Error("cookie should still be cleared")
	}
	if got := sessionStatus(h, cookie); got != 401 {
		t.Fatalf("copied cookie after failed logout = %d, want 401", got)
	}
}

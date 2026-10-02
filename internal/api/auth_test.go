package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestEveryProtectedRouteRejectsAnonymous reads the route table out of
// http.go, so a newly added route is covered automatically.
func TestEveryProtectedRouteRejectsAnonymous(t *testing.T) {
	src, err := os.ReadFile("http.go")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`protected\.HandleFunc\("([A-Z]+) (/api/v1/[^"]*)"`)
	routes := re.FindAllStringSubmatch(string(src), -1)
	if len(routes) < 50 {
		t.Fatalf("found only %d routes; the route-table pattern is out of date", len(routes))
	}
	h := NewHandler(&Ops{}, "secret")
	param := regexp.MustCompile(`\{[^}]*\}`)
	for _, m := range routes {
		path := param.ReplaceAllString(m[2], "x")
		for name, setup := range map[string]func(*http.Request){
			"no credentials": func(*http.Request) {},
			"wrong token":    func(r *http.Request) { r.Header.Set("Authorization", "Bearer nope") },
			"bogus session":  func(r *http.Request) { r.AddCookie(&http.Cookie{Name: SessionCookie, Value: "nope"}) },
		} {
			req := httptest.NewRequest(m[1], path, nil)
			setup(req)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s %s (%s) = %d, want 401", m[1], path, name, rec.Code)
			}
		}
	}
}

func login(t *testing.T, h http.Handler, token, ip string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/v1/auth/login", strings.NewReader(`{"token":"`+token+`"}`))
	req.RemoteAddr = ip + ":1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestLoginSessionLogout(t *testing.T) {
	h := NewHandler(&Ops{}, "secret")
	if rec := login(t, h, "wrong", "1.1.1.1"); rec.Code != 401 || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("bad login = %d with cookies %v", rec.Code, rec.Result().Cookies())
	}
	rec := login(t, h, "secret", "1.1.1.1")
	if rec.Code != 200 {
		t.Fatalf("login = %d", rec.Code)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].Secure || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie = %+v", cookies)
	}

	get := func(c *http.Cookie, proto string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/api/v1/auth/session", nil)
		req.AddCookie(c)
		if proto != "" {
			req.Header.Set("X-Forwarded-Proto", proto)
		}
		r := httptest.NewRecorder()
		h.ServeHTTP(r, req)
		return r
	}
	if r := get(cookies[0], ""); r.Code != 200 {
		t.Fatalf("session = %d", r.Code)
	}

	// Logout kills the session.
	req := httptest.NewRequest("POST", "/api/v1/auth/logout", nil)
	req.AddCookie(cookies[0])
	h.ServeHTTP(httptest.NewRecorder(), req)
	if r := get(cookies[0], ""); r.Code != 401 {
		t.Fatalf("after logout = %d, want 401", r.Code)
	}
}

func TestSecureCookieBehindProxy(t *testing.T) {
	h := NewHandler(&Ops{}, "secret")
	req := httptest.NewRequest("POST", "/api/v1/auth/login", strings.NewReader(`{"token":"secret"}`))
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if c := rec.Result().Cookies(); len(c) != 1 || !c[0].Secure {
		t.Fatalf("cookie behind TLS proxy = %+v, want Secure", c)
	}
}

func TestLoginRateLimit(t *testing.T) {
	h := NewHandler(&Ops{}, "secret")
	for i := 0; i < loginMaxFails; i++ {
		if rec := login(t, h, "bad", "2.2.2.2"); rec.Code != 401 {
			t.Fatalf("attempt %d = %d", i, rec.Code)
		}
	}
	// Even the right key is refused while locked out; other clients are not.
	if rec := login(t, h, "secret", "2.2.2.2"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("locked out = %d, want 429", rec.Code)
	}
	if rec := login(t, h, "secret", "3.3.3.3"); rec.Code != 200 {
		t.Fatalf("other client = %d, want 200", rec.Code)
	}
}

func TestLoginRateLimitExpires(t *testing.T) {
	a := NewAuth("secret", nil, "")
	now := time.Now()
	a.now = func() time.Time { return now }
	for i := 0; i < loginMaxFails; i++ {
		a.limited("ip", true)
	}
	if !a.limited("ip", false) {
		t.Fatal("should be limited")
	}
	now = now.Add(loginWindow + time.Second)
	if a.limited("ip", false) {
		t.Fatal("limit should expire")
	}
}

func TestRotateSignsEveryoneOut(t *testing.T) {
	var saved string
	a := NewAuth("old", func(tok string) error { saved = tok; return nil }, "")
	h := NewAuthedHandler(&Ops{}, a)
	cookie := login(t, h, "old", "1.1.1.1").Result().Cookies()[0]

	req := httptest.NewRequest("POST", "/api/v1/auth/rotate", nil)
	req.Header.Set("Authorization", "Bearer old")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || saved == "" || !strings.Contains(rec.Body.String(), saved) {
		t.Fatalf("rotate = %d %s (saved %q)", rec.Code, rec.Body.String(), saved)
	}
	for name, setup := range map[string]func(*http.Request){
		"old key":    func(r *http.Request) { r.Header.Set("Authorization", "Bearer old") },
		"old cookie": func(r *http.Request) { r.AddCookie(cookie) },
	} {
		req := httptest.NewRequest("GET", "/api/v1/auth/session", nil)
		setup(req)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 401 {
			t.Errorf("%s after rotate = %d, want 401", name, rec.Code)
		}
	}
	req = httptest.NewRequest("GET", "/api/v1/auth/session", nil)
	req.Header.Set("Authorization", "Bearer "+saved)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("new key = %d", rec.Code)
	}
}

func TestExposureWarning(t *testing.T) {
	for _, tc := range []struct {
		listen, proto string
		want          bool
	}{
		{"127.0.0.1:8080", "", false},
		{"0.0.0.0:8080", "", true},
		{"0.0.0.0:8080", "https", false},
		{"[::1]:8080", "", false},
	} {
		h := NewAuthedHandler(&Ops{}, NewAuth("k", nil, tc.listen))
		req := httptest.NewRequest("GET", "/api/v1/auth/session", nil)
		req.Header.Set("Authorization", "Bearer k")
		if tc.proto != "" {
			req.Header.Set("X-Forwarded-Proto", tc.proto)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if got := strings.Contains(rec.Body.String(), `"insecure_exposure":true`); got != tc.want {
			t.Errorf("%+v exposure = %v, want %v", tc, got, tc.want)
		}
	}
}

func TestHealthNeedsNoAuth(t *testing.T) {
	rec := httptest.NewRecorder()
	NewHandler(&Ops{}, "k").ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/health", nil))
	if rec.Code != 200 {
		t.Fatalf("health = %d", rec.Code)
	}
}

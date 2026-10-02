package api

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// bearerPrefix is the standard HTTP Authorization scheme Ballast's
// API token uses.
const bearerPrefix = "Bearer "

const (
	// SessionCookie is the name of the HttpOnly cookie the web UI holds
	// after logging in with the access key.
	SessionCookie = "ballast_session"

	sessionLifetime = 30 * 24 * time.Hour
	loginWindow     = time.Minute
	loginMaxFails   = 5
)

// Auth guards the API: one access key (the API token), accepted either
// as `Authorization: Bearer <key>` (CLI, scripts) or through a session
// cookie handed out by the login endpoint (web UI). It supports
// rotating the key, which also signs every browser out. Single token,
// single user — there is no RBAC.
type Auth struct {
	mu       sync.RWMutex
	token    string
	sessions map[string]time.Time // session id -> expiry
	fails    map[string][]time.Time
	persist  func(newToken string) error
	listen   string
	now      func() time.Time
}

// NewAuth builds an Auth for token. persist, when non-nil, stores a
// rotated token (the root-only credential file) before it takes
// effect. listen is the address the server listens on, used only for
// the exposure warning.
func NewAuth(token string, persist func(string) error, listen string) *Auth {
	return &Auth{
		token:    token,
		sessions: map[string]time.Time{},
		fails:    map[string][]time.Time{},
		persist:  persist,
		listen:   listen,
		now:      time.Now,
	}
}

// GenerateToken returns a fresh random access key (256 bits, hex).
func GenerateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (a *Auth) tokenMatches(given string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.token == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(given), []byte(a.token)) == 1
}

func (a *Auth) sessionValid(id string) bool {
	if id == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.sessions[id]
	if !ok {
		return false
	}
	if !a.now().Before(exp) {
		delete(a.sessions, id)
		return false
	}
	return true
}

// authorized reports whether r carries the key or a live session.
func (a *Auth) authorized(r *http.Request) bool {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, bearerPrefix) {
		return a.tokenMatches(strings.TrimPrefix(h, bearerPrefix))
	}
	if c, err := r.Cookie(SessionCookie); err == nil {
		return a.sessionValid(c.Value)
	}
	return false
}

// middleware requires valid credentials on every request reaching next.
func (a *Auth) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.authorized(r) {
			writeErrorMsg(w, http.StatusUnauthorized, "missing or invalid API token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// limited reports whether ip has used up its login attempts, and
// records a failure when record is true.
func (a *Auth) limited(ip string, record bool) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	var recent []time.Time
	for _, t := range a.fails[ip] {
		if now.Sub(t) < loginWindow {
			recent = append(recent, t)
		}
	}
	if record {
		recent = append(recent, now)
	}
	if len(recent) == 0 {
		delete(a.fails, ip)
	} else {
		a.fails[ip] = recent
	}
	return len(recent) >= loginMaxFails
}

// secureRequest reports whether the browser reached us over HTTPS,
// directly or through a TLS-terminating proxy.
func secureRequest(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (a *Auth) setCookie(w http.ResponseWriter, r *http.Request, id string, exp time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie, Value: id, Path: "/",
		Expires: exp, HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: secureRequest(r),
	})
}

func (a *Auth) login(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if a.limited(ip, false) {
		w.Header().Set("Retry-After", "60")
		writeErrorMsg(w, http.StatusTooManyRequests, "too many sign-in attempts; wait a minute and try again")
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil || !a.tokenMatches(strings.TrimSpace(body.Token)) {
		a.limited(ip, true)
		writeErrorMsg(w, http.StatusUnauthorized, "that access key didn't work")
		return
	}
	idb := make([]byte, 32)
	if _, err := rand.Read(idb); err != nil {
		writeErrorMsg(w, http.StatusInternalServerError, "could not start a session")
		return
	}
	id := hex.EncodeToString(idb)
	exp := a.now().Add(sessionLifetime)
	a.mu.Lock()
	a.sessions[id] = exp
	a.mu.Unlock()
	a.setCookie(w, r, id, exp)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *Auth) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(SessionCookie); err == nil {
		a.mu.Lock()
		delete(a.sessions, c.Value)
		a.mu.Unlock()
	}
	a.setCookie(w, r, "", time.Unix(0, 0))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// Rotate replaces the access key with a fresh one, stores it, and
// signs every browser out. The new key is returned once.
func (a *Auth) Rotate() (string, error) {
	tok, err := GenerateToken()
	if err != nil {
		return "", err
	}
	if a.persist != nil {
		if err := a.persist(tok); err != nil {
			return "", err
		}
	}
	a.mu.Lock()
	a.token = tok
	a.sessions = map[string]time.Time{}
	a.mu.Unlock()
	return tok, nil
}

func (a *Auth) rotate(w http.ResponseWriter, r *http.Request) {
	tok, err := a.Rotate()
	if err != nil {
		writeErrorMsg(w, http.StatusInternalServerError, "could not save the new access key: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"token": tok})
}

// signOutEverywhere ends every browser session without changing the key.
func (a *Auth) signOutEverywhere(w http.ResponseWriter, _ *http.Request) {
	a.mu.Lock()
	a.sessions = map[string]time.Time{}
	a.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// exposedWithoutTLS reports whether the server listens beyond this
// machine and the request did not arrive over HTTPS.
func (a *Auth) exposedWithoutTLS(r *http.Request) bool {
	return !ListenIsLoopback(a.listen) && a.listen != "" && !secureRequest(r)
}

// ListenIsLoopback reports whether addr (host:port) only accepts
// connections from this machine.
func ListenIsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (a *Auth) session(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true, "insecure_exposure": a.exposedWithoutTLS(r)})
}

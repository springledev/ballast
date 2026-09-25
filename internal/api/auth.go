package api

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// bearerPrefix is the standard HTTP Authorization scheme Ballast's
// API token uses.
const bearerPrefix = "Bearer "

// withAuth requires `Authorization: Bearer <apiToken>` on every
// request reaching next. NewHandler mounts this only over the routes
// that need it — POST .../trigger authenticates itself with a
// per-service trigger token instead (spec §12's reserved externally-
// triggered interface — X-Ballast-Trigger-Token, checked by the
// trigger handler via Ops.Trigger) and is registered outside this
// wrapper entirely, by routing rather than a path check here.
// apiToken is compared in constant time so response timing cannot
// help guess it.
func withAuth(next http.Handler, apiToken string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !validBearer(r.Header.Get("Authorization"), apiToken) {
			writeErrorMsg(w, http.StatusUnauthorized, "missing or invalid API token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// validBearer reports whether header carries the configured token as
// a Bearer credential. An empty configured token never matches
// anything (a server with no token configured refuses every request,
// rather than accepting all of them).
func validBearer(header, apiToken string) bool {
	if apiToken == "" {
		return false
	}
	if !strings.HasPrefix(header, bearerPrefix) {
		return false
	}
	given := strings.TrimPrefix(header, bearerPrefix)
	return subtle.ConstantTimeCompare([]byte(given), []byte(apiToken)) == 1
}

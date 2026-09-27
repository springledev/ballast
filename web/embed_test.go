package web

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerServesIndexPage(t *testing.T) {
	h, err := Handler()
	if err != nil {
		t.Fatalf("Handler() error: %v", err)
	}
	req, err := http.NewRequest(http.MethodGet, "/", nil)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /index.html: got status %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Ballast") {
		t.Errorf("embedded page does not mention Ballast; body: %s", rec.Body.String())
	}
}

// TestEmbedsRealBuild guards against a stale or stub dist/ being
// committed: the real Vue build always produces at least one script
// under assets/, which a hand-written stub index.html never has.
func TestEmbedsRealBuild(t *testing.T) {
	sub, err := FS()
	if err != nil {
		t.Fatalf("FS() error: %v", err)
	}
	entries, err := fs.ReadDir(sub, "assets")
	if err != nil {
		t.Fatalf("reading embedded assets dir: %v (did you run `make web`?)", err)
	}
	if len(entries) == 0 {
		t.Error("embedded assets/ directory is empty")
	}
}

func TestFSRoundTrip(t *testing.T) {
	sub, err := FS()
	if err != nil {
		t.Fatalf("FS() error: %v", err)
	}
	data, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		t.Fatalf("reading embedded index.html: %v", err)
	}
	if len(data) == 0 {
		t.Error("embedded index.html is empty")
	}
}

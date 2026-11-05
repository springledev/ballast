package web

import (
	"encoding/json"
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

// TestServesIcons checks the browser and home-screen icons are in the
// build, with content types browsers accept even on a machine that has
// no system MIME table.
func TestServesIcons(t *testing.T) {
	h, err := Handler()
	if err != nil {
		t.Fatalf("Handler() error: %v", err)
	}
	tests := []struct{ path, contentType string }{
		{"/favicon.ico", "image/"}, // image/x-icon or image/vnd.microsoft.icon, by system
		{"/favicon.svg", "image/svg+xml"},
		{"/apple-touch-icon.png", "image/png"},
		{"/icon-192.png", "image/png"},
		{"/icon-512.png", "image/png"},
		{"/icon-maskable-512.png", "image/png"},
		{"/manifest.json", "application/json"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tt.path, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("got status %d, want 200", rec.Code)
			}
			if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, tt.contentType) {
				t.Errorf("got Content-Type %q, want %q", got, tt.contentType)
			}
		})
	}
}

// TestPagePointsAtIcons checks index.html links the icons with relative
// paths, so they still load when a reverse proxy serves Ballast under a
// subpath.
func TestPagePointsAtIcons(t *testing.T) {
	sub, err := FS()
	if err != nil {
		t.Fatalf("FS() error: %v", err)
	}
	data, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		t.Fatalf("reading embedded index.html: %v", err)
	}
	for _, want := range []string{
		`href="./favicon.ico"`,
		`href="./favicon.svg"`,
		`href="./apple-touch-icon.png"`,
		`href="./manifest.json"`,
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("index.html does not contain %s", want)
		}
	}
}

// TestManifestIconsExist checks every icon the manifest names is in the build.
func TestManifestIconsExist(t *testing.T) {
	sub, err := FS()
	if err != nil {
		t.Fatalf("FS() error: %v", err)
	}
	data, err := fs.ReadFile(sub, "manifest.json")
	if err != nil {
		t.Fatalf("reading embedded manifest.json: %v", err)
	}
	var manifest struct {
		Icons []struct {
			Src string `json:"src"`
		} `json:"icons"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("parsing manifest.json: %v", err)
	}
	if len(manifest.Icons) == 0 {
		t.Fatal("manifest.json lists no icons")
	}
	for _, icon := range manifest.Icons {
		if _, err := fs.Stat(sub, icon.Src); err != nil {
			t.Errorf("manifest icon %s: %v", icon.Src, err)
		}
	}
}

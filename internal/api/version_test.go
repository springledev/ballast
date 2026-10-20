package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestVersionEndpointReportsBuild checks that GET /api/v1/version
// returns the version the Ops was given, and requires the key like
// every other protected route (spec §21.5).
func TestVersionEndpointReportsBuild(t *testing.T) {
	h := NewHandler(&Ops{Version: "35dad2f"}, "secret")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/version", nil)
	req.Header.Set("Authorization", "Bearer secret")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Version != "35dad2f" {
		t.Errorf("version = %q, want 35dad2f", got.Version)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/version", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("without key status = %d, want 401", rec.Code)
	}
}

// TestDoctorShowsBallastVersion checks that doctor names the running
// version as its first line, so a support request says what build it is.
func TestDoctorShowsBallastVersion(t *testing.T) {
	h := newHarness(t)
	h.ops.Version = "v0.5-35dad2f"
	rep, err := h.ops.Doctor(t.Context())
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	for _, c := range rep.Checks {
		if c.Area == "ballast" && c.Name == "version" {
			if c.Status != DoctorOK || c.Detail != "Ballast v0.5-35dad2f is running." {
				t.Errorf("version check = %+v", c)
			}
			return
		}
	}
	t.Fatal("doctor has no ballast version check")
}

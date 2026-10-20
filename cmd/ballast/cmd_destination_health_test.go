package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/springledev/ballast/internal/repohealth"
)

// TestHealthSummary covers the one-line wording shared by
// `destination list`, `destination health` and `status`.
func TestHealthSummary(t *testing.T) {
	at := time.Date(2026, 10, 18, 3, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		st   repohealth.Status
		want []string
	}{
		{"never checked", repohealth.Status{State: repohealth.StateUnchecked, Light: "grey"}, []string{"grey", "never checked"}},
		{"checked and ok", repohealth.Status{State: repohealth.StateOK, Light: "green", LastChecked: at, LastOK: at}, []string{"green", "last checked " + at.Local().Format("2006-01-02 15:04"), "last OK " + at.Local().Format("2006-01-02 15:04")}},
		{"failed after ok", repohealth.Status{State: repohealth.StateFailed, Light: "red", LastChecked: at, LastOK: at.Add(-48 * time.Hour)}, []string{"red", "last OK " + at.Add(-48*time.Hour).Local().Format("2006-01-02 15:04")}},
		{"failed never ok", repohealth.Status{State: repohealth.StateFailed, Light: "red", LastChecked: at}, []string{"red", "never passed"}},
		{"checks off", repohealth.Status{State: repohealth.StateUnchecked, Light: "grey", Enabled: false}, []string{"never checked"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := healthSummary(tc.st)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("healthSummary = %q, want it to contain %q", got, w)
				}
			}
		})
	}
}

// TestDestinationHealthCommand covers J7: a never-checked destination
// says so, on stdout, in text and JSON, and list/status show it too.
func TestDestinationHealthCommand(t *testing.T) {
	cliEnv(t, &spyRunner{})
	seedTwoServices(t)

	stdout, stderr, err := execCLISplit(t, "destination", "health")
	if err != nil {
		t.Fatalf("destination health: %v (stderr %q)", err, stderr)
	}
	if !strings.Contains(stdout, "local") || !strings.Contains(stdout, "never checked") {
		t.Errorf("stdout = %q, want the destination and \"never checked\"", stdout)
	}

	stdout, _, err = execCLISplit(t, "destination", "health", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got []repohealth.Status
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("--json output is not JSON: %v (%q)", err, stdout)
	}
	if len(got) != 1 || got[0].Destination != "local" || got[0].Light != "grey" {
		t.Errorf("json = %+v, want one grey destination named local", got)
	}

	stdout, _, err = execCLISplit(t, "destination", "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "never checked") {
		t.Errorf("destination list = %q, want the health summary", stdout)
	}

	stdout, _, err = execCLISplit(t, "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "local") || !strings.Contains(stdout, "never checked") {
		t.Errorf("status = %q, want a destinations section with health", stdout)
	}
	stdout, _, _ = execCLISplit(t, "status", "--json")
	if err := json.Unmarshal([]byte(stdout), &[]map[string]any{}); err != nil {
		t.Errorf("status --json must stay a plain list of services: %v", err)
	}

	if _, _, err := execCLISplit(t, "destination", "health", "extra"); err == nil {
		t.Error("destination health accepted a stray argument")
	}
}

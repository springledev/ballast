package pipeline

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/springledev/ballast/internal/engine"
	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/proxmox"
)

// configPVE answers the read-only endpoints a config snapshot uses.
// hideGuest makes the guest list show a guest whose config the
// snapshot cannot have (the list grows between the two reads).
func configPVE(t *testing.T, listCalls *int, growAfterFirst bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("config snapshot made a %s request to %s; it must only read", r.Method, r.URL.Path)
		}
		reply := func(v any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": v}) }
		p := strings.TrimPrefix(r.URL.Path, "/api2/json")
		switch {
		case p == "/cluster/resources":
			*listCalls++
			guests := []map[string]any{{"node": "pve", "vmid": 100, "name": "web", "type": "lxc"}}
			if growAfterFirst && *listCalls > 1 {
				guests = append(guests, map[string]any{"node": "pve", "vmid": 101, "name": "new", "type": "qemu"})
			}
			reply(guests)
		case p == "/nodes":
			reply([]map[string]any{{"node": "pve"}})
		case p == "/storage":
			reply([]map[string]any{{"storage": "backup", "password": "hunter2"}})
		case strings.HasSuffix(p, "/config"):
			reply(map[string]any{"cores": 2, "cipassword": "topsecret"})
		default:
			reply([]any{})
		}
	})
}

func TestRunConfigSnapshot(t *testing.T) {
	tests := []struct {
		name      string
		grow      bool
		wantState engine.State
	}{
		{name: "verified snapshot goes green", wantState: engine.SUCCESS},
		{name: "a guest the snapshot missed fails the run", grow: true, wantState: engine.FAILED},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			calls := 0
			srv := httptest.NewServer(configPVE(t, &calls, tt.grow))
			t.Cleanup(srv.Close)
			if err := os.WriteFile(filepath.Join(h.credDir, proxmox.DefaultCredentialName), []byte("secret-uuid\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := inventory.PutProxmoxConnection(h.db, proxmox.Connection{Host: srv.URL, TokenID: "ballast@pve!backup", Credential: proxmox.DefaultCredentialName}); err != nil {
				t.Fatal(err)
			}
			p := h.pipeline()
			p.Proxmox = proxmox.Resolver{CredDirs: []string{h.credDir}, HTTPClient: srv.Client()}
			if err := inventory.PutService(h.db, inventory.Service{Name: "pve-config", Kind: inventory.KindProxmoxConfig, Destination: "local"}); err != nil {
				t.Fatal(err)
			}
			state, err := p.Run(context.Background(), "pve-config")
			if state != tt.wantState {
				t.Fatalf("state = %s (%v), want %s", state, err, tt.wantState)
			}
			runs, _ := engine.RecentRuns(h.db, "pve-config", 1)
			tr := transitionsOf(t, h.db, runs[0].ID)
			if tt.wantState == engine.SUCCESS && !containsInOrder(tr, string(engine.VERIFYING), string(engine.SUCCESS)) {
				t.Errorf("transitions %v do not pass VERIFYING before SUCCESS", tr)
			}
			recs, _ := engine.VerificationResults(h.db, runs[0].ID)
			var snap *engine.VerificationRecord
			for i := range recs {
				if recs[i].Method == configSnapshotMethod {
					snap = &recs[i]
				}
			}
			if snap == nil || snap.Passed != (tt.wantState == engine.SUCCESS) {
				t.Errorf("verification records = %+v", recs)
			}
			log, _ := os.ReadFile(h.resticLog)
			if !strings.Contains(string(log), "proxmox-guests.json") {
				t.Errorf("restic was not given the snapshot files:\n%s", log)
			}
			for _, r := range recs {
				if strings.Contains(r.Detail, "hunter2") || strings.Contains(r.Detail, "topsecret") {
					t.Errorf("secret in verification detail: %s", r.Detail)
				}
			}
		})
	}
}

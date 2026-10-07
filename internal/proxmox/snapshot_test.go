package proxmox

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func snapshotServer(t *testing.T, denyFirewall bool) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reply := func(v any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": v}) }
		p := strings.TrimPrefix(r.URL.Path, "/api2/json")
		switch {
		case p == "/cluster/resources":
			reply([]map[string]any{{"node": "pve", "vmid": 100, "name": "web", "type": "lxc", "status": "running"}, {"node": "pve", "vmid": 101, "name": "vm", "type": "qemu"}})
		case p == "/nodes":
			reply([]map[string]any{{"node": "pve", "status": "online"}})
		case p == "/storage":
			reply([]map[string]any{{"storage": "backup", "type": "nfs", "password": "hunter2"}})
		case strings.HasSuffix(p, "/config"):
			reply(map[string]any{"cores": 2, "cipassword": "topsecret", "net0": "virtio"})
		case strings.Contains(p, "/firewall/") && denyFirewall:
			http.Error(w, "no", http.StatusForbidden)
		case p == "/access/users":
			reply([]map[string]any{{"userid": "root@pam", "keys": "abc", "tfa": "x"}})
		default:
			reply([]any{})
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{Host: srv.URL, TokenID: "a@pve!b", TokenSecret: "s", HTTPClient: srv.Client()}
}

func TestSnapshotConfig(t *testing.T) {
	tests := []struct {
		name        string
		denyFW      bool
		dropGuest   bool
		wantPass    bool
		wantSkipped bool
		wantDetail  string
	}{
		{name: "complete snapshot verifies", wantPass: true},
		{name: "unreadable firewall is skipped, not fatal", denyFW: true, wantPass: true, wantSkipped: true},
		{name: "a guest missing from the files fails", dropGuest: true, wantDetail: "101"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cl := snapshotServer(t, tt.denyFW)
			s, err := cl.SnapshotConfig(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			all := string(s.Files[SnapshotGuests]) + string(s.Files[SnapshotCluster]) + string(s.Files[SnapshotAccess])
			for _, secret := range []string{"hunter2", "topsecret", `"keys"`, `"tfa"`} {
				if strings.Contains(all, secret) {
					t.Errorf("snapshot contains %s", secret)
				}
			}
			if s.Removed < 4 {
				t.Errorf("Removed = %d, want at least 4", s.Removed)
			}
			if (len(s.Skipped) > 0) != tt.wantSkipped {
				t.Errorf("Skipped = %v", s.Skipped)
			}
			if tt.dropGuest {
				s.Files[SnapshotGuests] = []byte(`[{"node":"pve","vmid":100,"config":{"cores":1}}]`)
			}
			paths, err := s.WriteTo(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			res := VerifySnapshotFiles(paths, s.Guests)
			if res.Passed != tt.wantPass || !strings.Contains(res.Detail, tt.wantDetail) {
				t.Errorf("verify = %+v", res)
			}
		})
	}
}

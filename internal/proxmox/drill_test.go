package proxmox

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	kp "github.com/springledev/keel/proxmox"
)

func TestCheckGuestConfig(t *testing.T) {
	tests := []struct {
		name, conf, vol, wantErr string
	}{
		{"lxc ok", "arch: amd64\nrootfs: local:100/vm-100-disk-0.raw,size=8G\n", "local:backup/vzdump-lxc-100-x.tar.zst", ""},
		{"lxc no rootfs", "arch: amd64\n", "local:backup/vzdump-lxc-100-x.tar.zst", "root disk"},
		{"qemu ok", "memory: 2048\nscsi0: local-lvm:vm-101-disk-0\n", "local:backup/vzdump-qemu-101-x.vma.zst", ""},
		{"qemu snapshot disk ignored", "memory: 2048\n[snap]\nscsi0: x\n", "local:backup/vzdump-qemu-101-x.vma.zst", "no disks"},
		{"qemu no disk", "memory: 2048\n", "local:backup/vzdump-qemu-101-x.vma.zst", "no disks"},
		{"empty", "# only a comment\n", "local:backup/vzdump-lxc-100-x.tar.zst", "empty"},
		{"unknown kind", "a: b\n", "local:backup/other.tar", "not a container or VM"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkGuestConfig(tt.conf, tt.vol)
			if tt.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestCombineLayersIndependent(t *testing.T) {
	ok := LayerResult{Passed: true}
	bad := LayerResult{Service: "db", Passed: false, Detail: "restore failed"}
	tests := []struct {
		name   string
		config LayerResult
		guest  []LayerResult
		want   bool
	}{
		{"all pass", ok, []LayerResult{ok}, true},
		{"config only", ok, nil, true},
		{"config fails", LayerResult{}, []LayerResult{ok}, false},
		{"in-guest fails", ok, []LayerResult{ok, bad}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gv := Combine(tt.config, tt.guest...)
			if gv.Verified != tt.want {
				t.Errorf("Verified = %v, want %v", gv.Verified, tt.want)
			}
			if len(gv.Layers) != 1+len(tt.guest) {
				t.Errorf("layers = %d, want every layer reported", len(gv.Layers))
			}
		})
	}
}

func TestVerifyGuestConfig(t *testing.T) {
	const vol = "local:backup/vzdump-lxc-100-a.tar.zst"
	tests := []struct {
		name     string
		size     int64
		conf     string
		confCode int
		vol      string
		want     bool
		detail   string
	}{
		{"pass", 10, "rootfs: x\n", 200, vol, true, "read back correctly"},
		{"empty file", 0, "rootfs: x\n", 200, vol, false, "empty"},
		{"config unreadable", 10, "", 500, vol, false, "could not be read"},
		{"bad config", 10, "arch: amd64\n", 200, vol, false, "root disk"},
		{"dump gone", 10, "rootfs: x\n", 200, "local:backup/vzdump-lxc-100-zzz.tar.zst", false, "no longer on"},
		{"no volume", 10, "", 200, "", false, "no backup file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/content"):
					_ = json.NewEncoder(w).Encode(map[string]any{"data": []DumpVolume{{Volume: vol, VMID: 100, Size: tt.size}}})
				case strings.HasSuffix(r.URL.Path, "/extractconfig"):
					if tt.confCode != 200 {
						http.Error(w, "boom", tt.confCode)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"data": tt.conf})
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			cl := &Client{Client: &kp.Client{Host: srv.URL, TokenID: "u@pam!t", TokenSecret: "s", HTTPClient: srv.Client()}}
			got := cl.VerifyGuestConfig(context.Background(), GuestBackupResult{Node: "pve", VMID: 100, Storage: "local", Volume: tt.vol})
			if got.Passed != tt.want || !strings.Contains(got.Detail, tt.detail) {
				t.Errorf("got %+v, want passed=%v detail containing %q", got, tt.want, tt.detail)
			}
		})
	}
}

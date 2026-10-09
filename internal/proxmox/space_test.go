package proxmox

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCheckStorageRoom(t *testing.T) {
	const gib = int64(1 << 30)
	tests := []struct {
		name     string
		st       StorageStatus
		last     int64
		wantWarn bool
		wantErr  bool
	}{
		{"no earlier dump", StorageStatus{Total: 100 * gib, Used: 99 * gib, Avail: gib}, 0, false, false},
		{"plenty of room", StorageStatus{Total: 100 * gib, Used: 10 * gib, Avail: 90 * gib}, 5 * gib, false, false},
		{"passes 80 percent", StorageStatus{Total: 100 * gib, Used: 78 * gib, Avail: 22 * gib}, 5 * gib, true, false},
		{"does not fit", StorageStatus{Total: 100 * gib, Used: 98 * gib, Avail: 2 * gib}, 5 * gib, false, true},
		{"unknown total", StorageStatus{}, 5 * gib, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			warn, err := CheckStorageRoom(tt.st, "backup", tt.last)
			if (warn != "") != tt.wantWarn || (err != nil) != tt.wantErr {
				t.Errorf("warn=%q err=%v, want warn=%v err=%v", warn, err, tt.wantWarn, tt.wantErr)
			}
		})
	}
}

func TestStorageStatusAndCheckAccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api2/json/cluster/resources":
			_, _ = w.Write([]byte(`{"data":[{"node":"pve1","vmid":100,"name":"a","status":"running","type":"lxc"}]}`))
		case "/api2/json/nodes/pve1/storage/ok/status":
			_, _ = w.Write([]byte(`{"data":{"total":100,"used":40,"avail":60}}`))
		default:
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	defer srv.Close()
	cl := &Client{Host: srv.URL, TokenID: "x@pve!y", TokenSecret: "z", HTTPClient: srv.Client()}

	st, err := cl.StorageStatus(context.Background(), "pve1", "ok")
	if err != nil || st.Total != 100 || st.Used != 40 || st.Avail != 60 {
		t.Fatalf("StorageStatus = %+v, %v", st, err)
	}
	if ac := cl.CheckAccess(context.Background(), "ok"); ac.Guests != 1 || !ac.StorageRead {
		t.Errorf("CheckAccess ok = %+v", ac)
	}
	if ac := cl.CheckAccess(context.Background(), "denied"); ac.StorageErr == nil || ac.StorageRead {
		t.Errorf("CheckAccess denied = %+v, want a storage error", ac)
	}
}

func TestFormatBytes(t *testing.T) {
	if got := FormatBytes(3 << 30); !strings.HasPrefix(got, "3.0 GiB") {
		t.Errorf("FormatBytes = %q", got)
	}
}

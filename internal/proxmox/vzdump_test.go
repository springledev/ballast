package proxmox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	kp "github.com/springledev/keel/proxmox"
)

func TestGuestBackupOptionsValidate(t *testing.T) {
	tests := []struct {
		name     string
		opts     GuestBackupOptions
		wantErr  bool
		errText  string
		wantMode string
	}{
		{
			name:     "valid with defaults",
			opts:     GuestBackupOptions{Node: "pve1", VMID: 100, Storage: "local"},
			wantErr:  false,
			wantMode: ModeSnapshot,
		},
		{
			name:     "valid with explicit mode",
			opts:     GuestBackupOptions{Node: "pve1", VMID: 100, Storage: "local", Mode: ModeSuspend},
			wantErr:  false,
			wantMode: ModeSuspend,
		},
		{
			name:    "missing node",
			opts:    GuestBackupOptions{VMID: 100, Storage: "local"},
			wantErr: true,
			errText: "node and guest VMID are required",
		},
		{
			name:    "missing vmid",
			opts:    GuestBackupOptions{Node: "pve1", Storage: "local"},
			wantErr: true,
			errText: "node and guest VMID are required",
		},
		{
			name:    "vmid zero",
			opts:    GuestBackupOptions{Node: "pve1", VMID: 0, Storage: "local"},
			wantErr: true,
			errText: "node and guest VMID are required",
		},
		{
			name:    "missing storage",
			opts:    GuestBackupOptions{Node: "pve1", VMID: 100},
			wantErr: true,
			errText: "choose the Proxmox storage",
		},
		{
			name:    "empty storage",
			opts:    GuestBackupOptions{Node: "pve1", VMID: 100, Storage: "   "},
			wantErr: true,
			errText: "choose the Proxmox storage",
		},
		{
			name:    "bad mode",
			opts:    GuestBackupOptions{Node: "pve1", VMID: 100, Storage: "local", Mode: "invalid"},
			wantErr: true,
			errText: "unknown backup mode",
		},
		{
			name:    "negative keep-last",
			opts:    GuestBackupOptions{Node: "pve1", VMID: 100, Storage: "local", KeepLast: -1},
			wantErr: true,
			errText: "keep-last must not be negative",
		},
		{
			name:     "keep-last zero is valid",
			opts:     GuestBackupOptions{Node: "pve1", VMID: 100, Storage: "local", KeepLast: 0},
			wantErr:  false,
			wantMode: ModeSnapshot,
		},
		{
			name:     "mode stop",
			opts:     GuestBackupOptions{Node: "pve1", VMID: 100, Storage: "local", Mode: ModeStop},
			wantErr:  false,
			wantMode: ModeStop,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.opts.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && !strings.Contains(err.Error(), tt.errText) {
				t.Errorf("Validate() error = %v, want containing %q", err, tt.errText)
			}
			if !tt.wantErr && tt.opts.Mode != tt.wantMode {
				t.Errorf("Mode = %q, want %q", tt.opts.Mode, tt.wantMode)
			}
		})
	}
}

func TestClientBackupGuestHappyPath(t *testing.T) {
	const node = "pve1"
	const storage = "local"
	const vmid = 100

	initialDumps := []DumpVolume{
		{Volume: "local:backup/vzdump-lxc-100-old1.tar.zst", CTime: 1, Size: 1000, VMID: 100},
		{Volume: "local:backup/vzdump-lxc-100-old2.tar.zst", CTime: 2, Size: 2000, VMID: 100},
		{Volume: "local:backup/vzdump-lxc-100-old3.tar.zst", CTime: 3, Size: 3000, VMID: 100},
	}

	newVolume := "local:backup/vzdump-lxc-100-new.tar.zst"
	newCTime := int64(4)
	newSize := int64(4000)

	var taskPollCount int
	var deletedVolumes []string
	var taskUPID string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check DELETE requests first since path overlaps with GET
		if r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/storage/") && strings.Contains(r.URL.Path, "/content/") {
			deletedVolumes = append(deletedVolumes, r.URL.Path)
			data, _ := json.Marshal(map[string]interface{}{"data": ""})
			w.Header().Set("Content-Type", "application/json")
			w.Write(data)
			return
		}

		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/storage/"+storage+"/content") {
			// ListDumps call
			if !strings.Contains(r.URL.RawQuery, "content=backup") {
				t.Errorf("ListDumps: query = %q, want content=backup", r.URL.RawQuery)
			}

			dumps := initialDumps
			// After the task completes, return dumps including the new volume
			if taskUPID != "" && taskPollCount > 0 {
				dumps = []DumpVolume{
					{Volume: newVolume, CTime: newCTime, Size: newSize, VMID: vmid},
					{Volume: "local:backup/vzdump-lxc-100-old3.tar.zst", CTime: 3, Size: 3000, VMID: 100},
					{Volume: "local:backup/vzdump-lxc-100-old2.tar.zst", CTime: 2, Size: 2000, VMID: 100},
					{Volume: "local:backup/vzdump-lxc-100-old1.tar.zst", CTime: 1, Size: 1000, VMID: 100},
				}
			}

			data, _ := json.Marshal(map[string]interface{}{"data": dumps})
			w.Header().Set("Content-Type", "application/json")
			w.Write(data)
			return
		}

		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/vzdump") {
			// StartVzdump call
			if err := r.ParseForm(); err != nil {
				t.Fatalf("ParseForm: %v", err)
			}
			if got := r.Form.Get("vmid"); got != fmt.Sprintf("%d", vmid) {
				t.Errorf("StartVzdump vmid = %q, want %d", got, vmid)
			}
			if got := r.Form.Get("mode"); got != ModeSnapshot {
				t.Errorf("StartVzdump mode = %q, want %q", got, ModeSnapshot)
			}
			if got := r.Form.Get("storage"); got != storage {
				t.Errorf("StartVzdump storage = %q, want %q", got, storage)
			}
			if got := r.Form.Get("compress"); got != "zstd" {
				t.Errorf("StartVzdump compress = %q, want zstd", got)
			}

			taskUPID = "UPID:pve1:12345:1234567890"
			data, _ := json.Marshal(map[string]interface{}{"data": taskUPID})
			w.Header().Set("Content-Type", "application/json")
			w.Write(data)
			return
		}

		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/tasks/") {
			// TaskStatus call
			taskPollCount++
			status := TaskStatus{Status: "stopped", ExitStatus: "OK"}
			if taskPollCount == 1 {
				status.Status = "running"
			}

			data, _ := json.Marshal(map[string]interface{}{"data": status})
			w.Header().Set("Content-Type", "application/json")
			w.Write(data)
			return
		}

		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("not found"))
	}))
	defer srv.Close()

	cl := &Client{Client: &kp.Client{Host: srv.URL, TokenID: "x@pve!y", TokenSecret: "z", HTTPClient: srv.Client()}}

	res, err := cl.BackupGuest(context.Background(), GuestBackupOptions{
		Node:     node,
		VMID:     vmid,
		Storage:  storage,
		Mode:     ModeSnapshot,
		KeepLast: 2,
	}, 1*time.Millisecond)

	if err != nil {
		t.Fatalf("BackupGuest() error: %v", err)
	}

	if res.Node != node {
		t.Errorf("Node = %q, want %q", res.Node, node)
	}
	if res.VMID != vmid {
		t.Errorf("VMID = %d, want %d", res.VMID, vmid)
	}
	if res.Storage != storage {
		t.Errorf("Storage = %q, want %q", res.Storage, storage)
	}
	if res.Mode != ModeSnapshot {
		t.Errorf("Mode = %q, want %q", res.Mode, ModeSnapshot)
	}
	if res.UPID == "" {
		t.Errorf("UPID = %q, want non-empty", res.UPID)
	}
	if res.Volume != newVolume {
		t.Errorf("Volume = %q, want %q", res.Volume, newVolume)
	}
	if res.Size != newSize {
		t.Errorf("Size = %d, want %d", res.Size, newSize)
	}
	if res.PruneError != "" {
		t.Errorf("PruneError = %q, want empty", res.PruneError)
	}

	if len(res.Pruned) != 2 {
		t.Errorf("len(Pruned) = %d, want 2: %v", len(res.Pruned), res.Pruned)
	}
	expectedPruned := map[string]bool{
		"local:backup/vzdump-lxc-100-old1.tar.zst": true,
		"local:backup/vzdump-lxc-100-old2.tar.zst": true,
	}
	for _, p := range res.Pruned {
		if !expectedPruned[p] {
			t.Errorf("Pruned contains %q, unexpected", p)
		}
	}

	if len(deletedVolumes) != 2 {
		t.Errorf("len(deletedVolumes) = %d, want 2", len(deletedVolumes))
	}
}

func TestClientBackupGuestTaskFails(t *testing.T) {
	const node = "pve1"
	const storage = "local"
	const vmid = 100

	initialDumps := []DumpVolume{
		{Volume: "local:backup/vzdump-lxc-100-old.tar.zst", CTime: 1, Size: 1000, VMID: 100},
	}

	var taskUPID string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/storage/"+storage+"/content") {
			data, _ := json.Marshal(map[string]interface{}{"data": initialDumps})
			w.Header().Set("Content-Type", "application/json")
			w.Write(data)
			return
		}
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/vzdump") {
			taskUPID = "UPID:pve1:12345:1234567890"
			data, _ := json.Marshal(map[string]interface{}{"data": taskUPID})
			w.Header().Set("Content-Type", "application/json")
			w.Write(data)
			return
		}
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/tasks/") {
			status := TaskStatus{Status: "stopped", ExitStatus: "job errors"}
			data, _ := json.Marshal(map[string]interface{}{"data": status})
			w.Header().Set("Content-Type", "application/json")
			w.Write(data)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	cl := &Client{Client: &kp.Client{Host: srv.URL, TokenID: "x@pve!y", TokenSecret: "z", HTTPClient: srv.Client()}}

	res, err := cl.BackupGuest(context.Background(), GuestBackupOptions{
		Node:     node,
		VMID:     vmid,
		Storage:  storage,
		KeepLast: 2,
	}, 1*time.Millisecond)

	if err == nil {
		t.Fatalf("BackupGuest() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "job errors") {
		t.Errorf("error = %v, want containing 'job errors'", err)
	}
	if res.UPID == "" {
		t.Errorf("UPID = %q, want non-empty", res.UPID)
	}
}

func TestClientBackupGuestPruneDeleteReturns500(t *testing.T) {
	const node = "pve1"
	const storage = "local"
	const vmid = 100

	initialDumps := []DumpVolume{
		{Volume: "local:backup/vzdump-lxc-100-old1.tar.zst", CTime: 1, Size: 1000, VMID: 100},
		{Volume: "local:backup/vzdump-lxc-100-old2.tar.zst", CTime: 2, Size: 2000, VMID: 100},
	}

	newVolume := "local:backup/vzdump-lxc-100-new.tar.zst"

	var deleteCallCount int
	var taskUPID string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/storage/") && strings.Contains(r.URL.Path, "/content/") {
			deleteCallCount++
			if deleteCallCount == 1 {
				// First DELETE succeeds
				data, _ := json.Marshal(map[string]interface{}{"data": ""})
				w.Header().Set("Content-Type", "application/json")
				w.Write(data)
			} else {
				// Second DELETE returns 500
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte("internal server error"))
			}
			return
		}

		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/storage/"+storage+"/content") {
			dumps := initialDumps
			if taskUPID != "" {
				dumps = []DumpVolume{
					{Volume: newVolume, CTime: 3, Size: 3000, VMID: vmid},
					{Volume: "local:backup/vzdump-lxc-100-old2.tar.zst", CTime: 2, Size: 2000, VMID: 100},
					{Volume: "local:backup/vzdump-lxc-100-old1.tar.zst", CTime: 1, Size: 1000, VMID: 100},
				}
			}
			data, _ := json.Marshal(map[string]interface{}{"data": dumps})
			w.Header().Set("Content-Type", "application/json")
			w.Write(data)
			return
		}

		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/vzdump") {
			taskUPID = "UPID:pve1:12345:1234567890"
			data, _ := json.Marshal(map[string]interface{}{"data": taskUPID})
			w.Header().Set("Content-Type", "application/json")
			w.Write(data)
			return
		}

		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/tasks/") {
			status := TaskStatus{Status: "stopped", ExitStatus: "OK"}
			data, _ := json.Marshal(map[string]interface{}{"data": status})
			w.Header().Set("Content-Type", "application/json")
			w.Write(data)
			return
		}

		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	cl := &Client{Client: &kp.Client{Host: srv.URL, TokenID: "x@pve!y", TokenSecret: "z", HTTPClient: srv.Client()}}

	res, err := cl.BackupGuest(context.Background(), GuestBackupOptions{
		Node:     node,
		VMID:     vmid,
		Storage:  storage,
		KeepLast: 1,
	}, 1*time.Millisecond)

	// BackupGuest should return nil error (backup succeeded)
	if err != nil {
		t.Fatalf("BackupGuest() error = %v, want nil (prune failure doesn't mask backup)", err)
	}

	// Volume should be set (backup succeeded)
	if res.Volume != newVolume {
		t.Errorf("Volume = %q, want %q", res.Volume, newVolume)
	}

	// PruneError should be non-empty
	if res.PruneError == "" {
		t.Errorf("PruneError = %q, want non-empty (failure isolation)", res.PruneError)
	}
	if !strings.Contains(res.PruneError, "HTTP 500") {
		t.Errorf("PruneError = %q, want containing 'HTTP 500'", res.PruneError)
	}

	// One volume should be pruned successfully
	if len(res.Pruned) != 1 {
		t.Errorf("len(Pruned) = %d, want 1", len(res.Pruned))
	}
}

func TestClientBackupGuestKeepLastZero(t *testing.T) {
	const node = "pve1"
	const storage = "local"
	const vmid = 100

	initialDumps := []DumpVolume{
		{Volume: "local:backup/vzdump-lxc-100-old1.tar.zst", CTime: 1, Size: 1000, VMID: 100},
		{Volume: "local:backup/vzdump-lxc-100-old2.tar.zst", CTime: 2, Size: 2000, VMID: 100},
		{Volume: "local:backup/vzdump-lxc-100-old3.tar.zst", CTime: 3, Size: 3000, VMID: 100},
	}

	newVolume := "local:backup/vzdump-lxc-100-new.tar.zst"

	var deleteCallCount int
	var taskUPID string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleteCallCount++
			w.WriteHeader(http.StatusNotFound)
			return
		}

		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/storage/"+storage+"/content") {
			dumps := initialDumps
			if taskUPID != "" {
				dumps = []DumpVolume{
					{Volume: newVolume, CTime: 4, Size: 4000, VMID: vmid},
					{Volume: "local:backup/vzdump-lxc-100-old3.tar.zst", CTime: 3, Size: 3000, VMID: 100},
					{Volume: "local:backup/vzdump-lxc-100-old2.tar.zst", CTime: 2, Size: 2000, VMID: 100},
					{Volume: "local:backup/vzdump-lxc-100-old1.tar.zst", CTime: 1, Size: 1000, VMID: 100},
				}
			}
			data, _ := json.Marshal(map[string]interface{}{"data": dumps})
			w.Header().Set("Content-Type", "application/json")
			w.Write(data)
			return
		}

		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/vzdump") {
			taskUPID = "UPID:pve1:12345:1234567890"
			data, _ := json.Marshal(map[string]interface{}{"data": taskUPID})
			w.Header().Set("Content-Type", "application/json")
			w.Write(data)
			return
		}

		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/tasks/") {
			status := TaskStatus{Status: "stopped", ExitStatus: "OK"}
			data, _ := json.Marshal(map[string]interface{}{"data": status})
			w.Header().Set("Content-Type", "application/json")
			w.Write(data)
			return
		}

		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	cl := &Client{Client: &kp.Client{Host: srv.URL, TokenID: "x@pve!y", TokenSecret: "z", HTTPClient: srv.Client()}}

	res, err := cl.BackupGuest(context.Background(), GuestBackupOptions{
		Node:     node,
		VMID:     vmid,
		Storage:  storage,
		KeepLast: 0,
	}, 1*time.Millisecond)

	if err != nil {
		t.Fatalf("BackupGuest() error: %v", err)
	}

	if res.Volume != newVolume {
		t.Errorf("Volume = %q, want %q", res.Volume, newVolume)
	}

	if deleteCallCount != 0 {
		t.Errorf("DELETE called %d times, want 0 for KeepLast=0", deleteCallCount)
	}

	if len(res.Pruned) != 0 {
		t.Errorf("len(Pruned) = %d, want 0", len(res.Pruned))
	}
}

func TestClientBackupGuestNoNewVolumeAppears(t *testing.T) {
	const node = "pve1"
	const storage = "local"
	const vmid = 100

	initialDumps := []DumpVolume{
		{Volume: "local:backup/vzdump-lxc-100-old.tar.zst", CTime: 1, Size: 1000, VMID: 100},
	}

	var taskUPID string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/storage/"+storage+"/content") {
			// Return same dumps before and after (no new volume)
			data, _ := json.Marshal(map[string]interface{}{"data": initialDumps})
			w.Header().Set("Content-Type", "application/json")
			w.Write(data)
			return
		}

		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/vzdump") {
			taskUPID = "UPID:pve1:12345:1234567890"
			data, _ := json.Marshal(map[string]interface{}{"data": taskUPID})
			w.Header().Set("Content-Type", "application/json")
			w.Write(data)
			return
		}

		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/tasks/") {
			status := TaskStatus{Status: "stopped", ExitStatus: "OK"}
			data, _ := json.Marshal(map[string]interface{}{"data": status})
			w.Header().Set("Content-Type", "application/json")
			w.Write(data)
			return
		}

		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	cl := &Client{Client: &kp.Client{Host: srv.URL, TokenID: "x@pve!y", TokenSecret: "z", HTTPClient: srv.Client()}}

	_, err := cl.BackupGuest(context.Background(), GuestBackupOptions{
		Node:    node,
		VMID:    vmid,
		Storage: storage,
	}, 1*time.Millisecond)

	if err == nil {
		t.Fatalf("BackupGuest() error = nil, want error about no new volume")
	}
	if !strings.Contains(err.Error(), "no new backup file appeared") {
		t.Errorf("error = %v, want containing 'no new backup file appeared'", err)
	}
}

func TestClientBackupGuestContextCancellation(t *testing.T) {
	const node = "pve1"
	const storage = "local"
	const vmid = 100

	initialDumps := []DumpVolume{
		{Volume: "local:backup/vzdump-lxc-100-old.tar.zst", CTime: 1, Size: 1000, VMID: 100},
	}

	var taskStatusCallCount atomic.Int32
	var taskUPID string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/storage/"+storage+"/content") {
			data, _ := json.Marshal(map[string]interface{}{"data": initialDumps})
			w.Header().Set("Content-Type", "application/json")
			w.Write(data)
			return
		}

		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/vzdump") {
			taskUPID = "UPID:pve1:12345:1234567890"
			data, _ := json.Marshal(map[string]interface{}{"data": taskUPID})
			w.Header().Set("Content-Type", "application/json")
			w.Write(data)
			return
		}

		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/tasks/") {
			taskStatusCallCount.Add(1)
			// Always return "running" to keep polling until context is cancelled
			status := TaskStatus{Status: "running"}
			data, _ := json.Marshal(map[string]interface{}{"data": status})
			w.Header().Set("Content-Type", "application/json")
			w.Write(data)
			return
		}

		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	cl := &Client{Client: &kp.Client{Host: srv.URL, TokenID: "x@pve!y", TokenSecret: "z", HTTPClient: srv.Client()}}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	_, err := cl.BackupGuest(ctx, GuestBackupOptions{
		Node:    node,
		VMID:    vmid,
		Storage: storage,
	}, 1*time.Millisecond)

	if err == nil {
		t.Fatalf("BackupGuest() error = nil, want context cancellation error")
	}
	if !strings.Contains(err.Error(), "context deadline exceeded") && !strings.Contains(err.Error(), "stopped waiting") {
		t.Errorf("error = %v, want containing context deadline or stopped waiting", err)
	}

	if taskStatusCallCount.Load() == 0 {
		t.Errorf("task status was never checked, context cancelled too early")
	}
}

func TestClientBackupGuestListDumpsError(t *testing.T) {
	const node = "pve1"
	const storage = "local"
	const vmid = 100

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/storage/"+storage+"/content") {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("error listing dumps"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	cl := &Client{Client: &kp.Client{Host: srv.URL, TokenID: "x@pve!y", TokenSecret: "z", HTTPClient: srv.Client()}}

	_, err := cl.BackupGuest(context.Background(), GuestBackupOptions{
		Node:    node,
		VMID:    vmid,
		Storage: storage,
	}, 1*time.Millisecond)

	if err == nil {
		t.Fatalf("BackupGuest() error = nil, want error from ListDumps")
	}
	if !strings.Contains(err.Error(), "listing existing backups") {
		t.Errorf("error = %v, want containing 'listing existing backups'", err)
	}
}

func TestClientBackupGuestStartVzdumpError(t *testing.T) {
	const node = "pve1"
	const storage = "local"
	const vmid = 100

	initialDumps := []DumpVolume{
		{Volume: "local:backup/vzdump-lxc-100-old.tar.zst", CTime: 1, Size: 1000, VMID: 100},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/storage/"+storage+"/content") {
			data, _ := json.Marshal(map[string]interface{}{"data": initialDumps})
			w.Header().Set("Content-Type", "application/json")
			w.Write(data)
			return
		}
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/vzdump") {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("error starting backup"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	cl := &Client{Client: &kp.Client{Host: srv.URL, TokenID: "x@pve!y", TokenSecret: "z", HTTPClient: srv.Client()}}

	_, err := cl.BackupGuest(context.Background(), GuestBackupOptions{
		Node:    node,
		VMID:    vmid,
		Storage: storage,
	}, 1*time.Millisecond)

	if err == nil {
		t.Fatalf("BackupGuest() error = nil, want error from StartVzdump")
	}
	if !strings.Contains(err.Error(), "starting the backup") {
		t.Errorf("error = %v, want containing 'starting the backup'", err)
	}
}

func TestClientBackupGuestValidationError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	cl := &Client{Client: &kp.Client{Host: srv.URL, TokenID: "x@pve!y", TokenSecret: "z", HTTPClient: srv.Client()}}

	// Invalid options (missing storage)
	_, err := cl.BackupGuest(context.Background(), GuestBackupOptions{
		Node: "pve1",
		VMID: 100,
	}, 1*time.Millisecond)

	if err == nil {
		t.Fatalf("BackupGuest() error = nil, want validation error")
	}
	if !strings.Contains(err.Error(), "choose the Proxmox storage") {
		t.Errorf("error = %v, want containing validation message", err)
	}
}

func TestTaskStatusAndWaitTask(t *testing.T) {
	const node = "pve1"
	const upid = "UPID:pve1:12345:1234567890"

	var pollCount int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/tasks/") {
			pollCount++
			status := TaskStatus{Status: "running"}
			if pollCount >= 2 {
				status = TaskStatus{Status: "stopped", ExitStatus: "OK"}
			}
			data, _ := json.Marshal(map[string]interface{}{"data": status})
			w.Header().Set("Content-Type", "application/json")
			w.Write(data)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	cl := &Client{Client: &kp.Client{Host: srv.URL, TokenID: "x@pve!y", TokenSecret: "z", HTTPClient: srv.Client()}}

	err := cl.WaitTask(context.Background(), node, upid, 1*time.Millisecond)

	if err != nil {
		t.Fatalf("WaitTask() error: %v", err)
	}
	if pollCount < 2 {
		t.Errorf("pollCount = %d, want at least 2", pollCount)
	}
}

func TestListDumps(t *testing.T) {
	const node = "pve1"
	const storage = "local"
	const vmid = 100

	// Mix of dumps for different vmids, and unordered ctimes
	allDumps := []DumpVolume{
		{Volume: "local:backup/vzdump-lxc-100-a.tar.zst", CTime: 1000, Size: 1000, VMID: 100},
		{Volume: "local:backup/vzdump-lxc-101-a.tar.zst", CTime: 2000, Size: 2000, VMID: 101},
		{Volume: "local:backup/vzdump-lxc-100-b.tar.zst", CTime: 3000, Size: 3000, VMID: 100},
		{Volume: "local:backup/vzdump-lxc-100-c.tar.zst", CTime: 2000, Size: 2500, VMID: 100},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/storage/"+storage+"/content") {
			data, _ := json.Marshal(map[string]interface{}{"data": allDumps})
			w.Header().Set("Content-Type", "application/json")
			w.Write(data)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	cl := &Client{Client: &kp.Client{Host: srv.URL, TokenID: "x@pve!y", TokenSecret: "z", HTTPClient: srv.Client()}}

	dumps, err := cl.ListDumps(context.Background(), node, storage, vmid)

	if err != nil {
		t.Fatalf("ListDumps() error: %v", err)
	}

	if len(dumps) != 3 {
		t.Fatalf("len(dumps) = %d, want 3", len(dumps))
	}

	// Should be sorted newest first (by CTime descending)
	if dumps[0].CTime != 3000 || dumps[1].CTime != 2000 || dumps[2].CTime != 1000 {
		t.Errorf("CTime ordering = [%d, %d, %d], want [3000, 2000, 1000]", dumps[0].CTime, dumps[1].CTime, dumps[2].CTime)
	}

	for _, d := range dumps {
		if d.VMID != vmid {
			t.Errorf("dump has VMID = %d, want %d", d.VMID, vmid)
		}
	}
}

func TestDeleteDumpURLEscaping(t *testing.T) {
	const node = "pve1"
	const storage = "local"
	// Use a volume ID that requires URL escaping: contains colons, slashes, hyphens
	volume := "local:backup/vzdump-lxc-100-2026_09_28-03_00_00.tar.zst"

	var receivedPath string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			receivedPath = r.URL.EscapedPath()
			w.WriteHeader(http.StatusOK)
			data, _ := json.Marshal(map[string]interface{}{"data": ""})
			w.Header().Set("Content-Type", "application/json")
			w.Write(data)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	cl := &Client{Client: &kp.Client{Host: srv.URL, TokenID: "x@pve!y", TokenSecret: "z", HTTPClient: srv.Client()}}

	err := cl.DeleteDump(context.Background(), node, storage, volume, 1*time.Millisecond)

	if err != nil {
		t.Fatalf("DeleteDump() error: %v", err)
	}

	// Verify the path contains escaped forward slashes
	if !strings.Contains(receivedPath, "%2F") {
		t.Errorf("receivedPath = %q, want containing %%2F (escaped slash)", receivedPath)
	}

	// Unescape and verify the volume is in the path
	unescaped, _ := url.PathUnescape(receivedPath)
	if !strings.Contains(unescaped, volume) {
		t.Errorf("unescaped path = %q, want containing %q", unescaped, volume)
	}
}

func TestPruneOwnedNeverTouchesForeignDumps(t *testing.T) {
	dumps := []DumpVolume{
		{Volume: "s:backup/vzdump-lxc-100-d.tar.zst", CTime: 4, Size: 1, VMID: 100},
		{Volume: "s:backup/vzdump-lxc-100-c.tar.zst", CTime: 3, Size: 1, VMID: 100}, // foreign
		{Volume: "s:backup/vzdump-lxc-100-b.tar.zst", CTime: 2, Size: 1, VMID: 100},
		{Volume: "s:backup/vzdump-lxc-100-a.tar.zst", CTime: 1, Size: 1, VMID: 100},
	}
	var deleted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleted = append(deleted, r.URL.Path[strings.Index(r.URL.Path, "/content/")+len("/content/"):])
			_, _ = w.Write([]byte(`{"data":""}`))
			return
		}
		b, _ := json.Marshal(map[string]any{"data": dumps})
		_, _ = w.Write(b)
	}))
	defer srv.Close()
	cl := &Client{Client: &kp.Client{Host: srv.URL, TokenID: "u@pve!t", TokenSecret: "x", HTTPClient: srv.Client()}}
	owned := []string{"s:backup/vzdump-lxc-100-d.tar.zst", "s:backup/vzdump-lxc-100-b.tar.zst", "s:backup/vzdump-lxc-100-a.tar.zst", "s:backup/vzdump-lxc-100-vanished.tar.zst"}
	pruned, gone, err := cl.PruneOwned(context.Background(), "pve", "s", 100, owned, 1, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(pruned) != 2 || pruned[0] != "s:backup/vzdump-lxc-100-b.tar.zst" || pruned[1] != "s:backup/vzdump-lxc-100-a.tar.zst" {
		t.Errorf("pruned = %v, want the two oldest owned dumps", pruned)
	}
	if len(gone) != 1 || !strings.Contains(gone[0], "vanished") {
		t.Errorf("gone = %v", gone)
	}
	for _, d := range deleted {
		if strings.Contains(d, "-c.") {
			t.Errorf("deleted the foreign dump %s", d)
		}
	}
	if p, _, _ := cl.PruneOwned(context.Background(), "pve", "s", 100, owned, 0, time.Millisecond); p != nil {
		t.Errorf("keep-last 0 must prune nothing, got %v", p)
	}
}

// TestCreateDump_CancelStopsTask: when the run is stopped while
// Proxmox is still making the dump, the task is stopped too (DELETE on
// the task), on a fresh context; if that fails, the result says so.
func TestCreateDump_CancelStopsTask(t *testing.T) {
	for _, tc := range []struct {
		name       string
		deleteCode int
		wantStop   bool
	}{
		{"task stopped", http.StatusOK, true},
		{"task could not be stopped", http.StatusInternalServerError, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const upid = "UPID:pve1:12345:1234567890"
			var deleted atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/content"):
					w.Write([]byte(`{"data":[]}`))
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/vzdump"):
					w.Write([]byte(`{"data":"` + upid + `"}`))
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/status"):
					w.Write([]byte(`{"data":{"status":"running"}}`))
				case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/tasks/"+upid):
					deleted.Add(1)
					w.WriteHeader(tc.deleteCode)
					w.Write([]byte(`{"data":null}`))
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()
			cl := &Client{Client: &kp.Client{Host: srv.URL, TokenID: "x@pve!y", TokenSecret: "z", HTTPClient: srv.Client()}}
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			res, err := cl.CreateDump(ctx, GuestBackupOptions{Node: "pve1", VMID: 100, Storage: "local"}, time.Millisecond)
			if err == nil {
				t.Fatal("CreateDump = nil error, want the cancellation")
			}
			if deleted.Load() != 1 {
				t.Fatalf("task stop requests = %d, want 1", deleted.Load())
			}
			if res.TaskStopped != tc.wantStop || (res.TaskLeftRunning == "") != tc.wantStop {
				t.Errorf("TaskStopped=%v TaskLeftRunning=%q", res.TaskStopped, res.TaskLeftRunning)
			}
			if strings.Contains(res.TaskLeftRunning, "z") && strings.Contains(res.TaskLeftRunning, "PVEAPIToken") {
				t.Errorf("token leaked: %q", res.TaskLeftRunning)
			}
		})
	}
}

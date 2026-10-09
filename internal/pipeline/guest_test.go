package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/springledev/ballast/internal/engine"
	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/proxmox"
)

// fakePVE is a small stateful Proxmox API: vzdump adds a dump, the
// content listing shows every dump, deleting removes one.
type fakePVE struct {
	mu          sync.Mutex
	dumps       []proxmox.DumpVolume
	deleted     []string
	next        int64
	config      string
	configFails bool
	size        int64
	storage     *proxmox.StorageStatus // reply to storage-status reads when set
}

func newFakePVE(foreign ...string) *fakePVE {
	f := &fakePVE{next: 100, config: "arch: amd64\nrootfs: local:100/vm-100-disk-0.raw,size=8G\n", size: 4096}
	for i, v := range foreign {
		f.dumps = append(f.dumps, proxmox.DumpVolume{Volume: v, CTime: int64(i + 1), Size: 1, VMID: 100})
	}
	return f
}

func (f *fakePVE) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		reply := func(v any) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"data": v})
		}
		p := r.URL.Path
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(p, "/vzdump"):
			f.next++
			f.dumps = append([]proxmox.DumpVolume{{Volume: fmt.Sprintf("backup:backup/vzdump-lxc-100-%d.tar.zst", f.next), CTime: f.next, Size: f.size, VMID: 100}}, f.dumps...)
			reply("UPID:pve:1")
		case f.storage != nil && strings.Contains(p, "/storage/") && strings.HasSuffix(p, "/status"):
			reply(f.storage)
		case strings.HasSuffix(p, "/status"):
			reply(map[string]string{"status": "stopped", "exitstatus": "OK"})
		case r.Method == http.MethodDelete:
			vol := p[strings.Index(p, "/content/")+len("/content/"):]
			f.deleted = append(f.deleted, vol)
			kept := f.dumps[:0]
			for _, d := range f.dumps {
				if d.Volume != vol {
					kept = append(kept, d)
				}
			}
			f.dumps = kept
			reply("")
		case strings.HasSuffix(p, "/extractconfig"):
			if f.configFails {
				http.Error(w, `{"errors":"cannot read"}`, http.StatusInternalServerError)
				return
			}
			reply(f.config)
		case r.Method == http.MethodGet && strings.Contains(p, "/content"):
			reply(f.dumps)
		default:
			t.Errorf("unexpected Proxmox request %s %s", r.Method, p)
			http.NotFound(w, r)
		}
	})
}

func (h *testHarness) guestPipeline(t *testing.T, f *fakePVE) *Pipeline {
	t.Helper()
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	if err := os.WriteFile(filepath.Join(h.credDir, proxmox.DefaultCredentialName), []byte("secret-uuid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := inventory.PutProxmoxConnection(h.db, proxmox.Connection{Host: srv.URL, TokenID: "ballast@pve!backup", Credential: proxmox.DefaultCredentialName}); err != nil {
		t.Fatal(err)
	}
	p := h.pipeline()
	p.Proxmox = proxmox.Resolver{CredDirs: []string{h.credDir}, HTTPClient: srv.Client()}
	p.ProxmoxPoll = 1
	return p
}

func putGuest(t *testing.T, h *testHarness, g inventory.GuestJob, dest string) {
	t.Helper()
	if err := inventory.PutService(h.db, inventory.Service{
		Name: "pve-ct100", Kind: inventory.KindProxmoxGuest, ProxmoxNode: "pve", ProxmoxVMID: 100,
		Guest: &g, Destination: dest,
	}); err != nil {
		t.Fatalf("PutService: %v", err)
	}
}

func TestRunGuestJob(t *testing.T) {
	foreign := "backup:backup/vzdump-lxc-100-handmade.tar.zst"
	tests := []struct {
		name        string
		setup       func(f *fakePVE)
		runs        int
		keepLast    int
		wantState   engine.State
		wantDeleted int
	}{
		{name: "verified dump goes green", runs: 1, wantState: engine.SUCCESS},
		{name: "keep-last prunes only dumps Ballast made", runs: 3, keepLast: 1, wantState: engine.SUCCESS, wantDeleted: 2},
		{name: "unreadable guest settings fail the run and prune nothing", setup: func(f *fakePVE) { f.configFails = true }, runs: 1, keepLast: 1, wantState: engine.FAILED},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			f := newFakePVE(foreign)
			if tt.setup != nil {
				tt.setup(f)
			}
			p := h.guestPipeline(t, f)
			putGuest(t, h, inventory.GuestJob{Storage: "backup", KeepLast: tt.keepLast}, "")
			var state engine.State
			for i := 0; i < tt.runs; i++ {
				state, _ = p.Run(context.Background(), "pve-ct100")
			}
			if state != tt.wantState {
				t.Fatalf("state = %s, want %s", state, tt.wantState)
			}
			if len(f.deleted) != tt.wantDeleted {
				t.Errorf("deleted %v, want %d deletion(s)", f.deleted, tt.wantDeleted)
			}
			for _, d := range f.deleted {
				if strings.Contains(d, "handmade") {
					t.Errorf("deleted a dump Ballast did not make: %s", d)
				}
			}
			runs, err := engine.RecentRuns(h.db, "pve-ct100", 1)
			if err != nil || len(runs) != 1 {
				t.Fatalf("RecentRuns: %v %v", runs, err)
			}
			tr := transitionsOf(t, h.db, runs[0].ID)
			if tt.wantState == engine.SUCCESS && !containsInOrder(tr, string(engine.VERIFYING), string(engine.SUCCESS)) {
				t.Errorf("transitions %v do not pass VERIFYING before SUCCESS", tr)
			}
			recs, _ := engine.VerificationResults(h.db, runs[0].ID)
			if len(recs) != 1 || recs[0].Method != guestConfigMethod || recs[0].Passed != (tt.wantState == engine.SUCCESS) {
				t.Errorf("verification records = %+v", recs)
			}
		})
	}
}

func TestRunGuestJobCopiesDumpToDestination(t *testing.T) {
	h := newHarness(t)
	f := newFakePVE()
	p := h.guestPipeline(t, f)
	copyDir := t.TempDir()
	// The fake's first dump will be vzdump-lxc-100-101.tar.zst, 4096 bytes.
	if err := os.MkdirAll(filepath.Join(copyDir, "dump"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(copyDir, "dump", "vzdump-lxc-100-101.tar.zst"), make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	putGuest(t, h, inventory.GuestJob{Storage: "backup", CopyPath: copyDir}, "local")
	state, err := p.Run(context.Background(), "pve-ct100")
	if state != engine.SUCCESS {
		t.Fatalf("state = %s (%v), want SUCCESS", state, err)
	}
	log, _ := os.ReadFile(h.resticLog)
	if !strings.Contains(string(log), "vzdump-lxc-100-101.tar.zst") {
		t.Errorf("restic was not given the dump file:\n%s", log)
	}
	runs, _ := engine.RecentRuns(h.db, "pve-ct100", 1)
	recs, _ := engine.VerificationResults(h.db, runs[0].ID)
	methods := map[string]bool{}
	for _, r := range recs {
		methods[r.Method] = r.Passed
	}
	if !methods[guestConfigMethod] || !methods[readBackMethod] {
		t.Errorf("verification methods = %v, want guest-config and restic-read-back passing", methods)
	}
	targets, _ := engine.RunTargets(h.db, runs[0].ID)
	if len(targets) != 1 || targets[0].State != engine.SUCCESS {
		t.Errorf("targets = %+v", targets)
	}
}

func TestRunGuestJobCopyFailureIsRedButDumpIsKept(t *testing.T) {
	h := newHarness(t)
	f := newFakePVE()
	p := h.guestPipeline(t, f)
	putGuest(t, h, inventory.GuestJob{Storage: "backup", CopyPath: t.TempDir()}, "local") // dump file never appears there
	state, err := p.Run(context.Background(), "pve-ct100")
	if state != engine.FAILED || err == nil || !strings.Contains(err.Error(), "was not found") {
		t.Fatalf("state = %s, err = %v; want FAILED naming the missing file", state, err)
	}
	if len(f.deleted) != 0 || len(f.dumps) != 1 {
		t.Errorf("the dump must be left alone: deleted %v, dumps %d", f.deleted, len(f.dumps))
	}
}

func TestRunGuestJobNotifiesOnFailure(t *testing.T) {
	h := newHarness(t)
	f := newFakePVE()
	f.configFails = true
	p := h.guestPipeline(t, f)
	n := &stubNotifier{}
	p.Notifier = n
	putGuest(t, h, inventory.GuestJob{Storage: "backup"}, "")
	if state, _ := p.Run(context.Background(), "pve-ct100"); state != engine.FAILED {
		t.Fatalf("state = %s, want FAILED", state)
	}
	if n.jobFailures+n.verificationFails != 1 {
		t.Errorf("failure notifications = %d job + %d verification, want exactly one", n.jobFailures, n.verificationFails)
	}
}

func TestRunGuestJobChecksStorageRoomBeforeDump(t *testing.T) {
	tests := []struct {
		name      string
		storage   proxmox.StorageStatus
		wantState engine.State
		wantDumps int // dumps on the storage after the second run
	}{
		{"roomy storage runs normally", proxmox.StorageStatus{Total: 1 << 30, Used: 1 << 20, Avail: 1<<30 - 1<<20}, engine.SUCCESS, 2},
		{"over 80 percent warns but still verifies", proxmox.StorageStatus{Total: 10000, Used: 8000, Avail: 2000 + 4096}, engine.SUCCESS, 2},
		{"no room fails before vzdump starts", proxmox.StorageStatus{Total: 10000, Used: 9990, Avail: 10}, engine.FAILED, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			f := newFakePVE()
			p := h.guestPipeline(t, f)
			putGuest(t, h, inventory.GuestJob{Storage: "backup"}, "")
			if st, _ := p.Run(context.Background(), "pve-ct100"); st != engine.SUCCESS {
				t.Fatalf("first run = %s, want SUCCESS", st)
			}
			f.mu.Lock()
			f.storage = &tt.storage
			f.mu.Unlock()
			state, err := p.Run(context.Background(), "pve-ct100")
			if state != tt.wantState {
				t.Fatalf("second run = %s (%v), want %s", state, err, tt.wantState)
			}
			if len(f.dumps) != tt.wantDumps {
				t.Errorf("%d dump(s) on storage, want %d", len(f.dumps), tt.wantDumps)
			}
			if tt.wantState == engine.FAILED && (err == nil || !strings.Contains(err.Error(), "would fill it")) {
				t.Errorf("error = %v, want a plain-English 'would fill it' message", err)
			}
			if tt.wantState == engine.SUCCESS {
				runs, _ := engine.RecentRuns(h.db, "pve-ct100", 1)
				if tr := transitionsOf(t, h.db, runs[0].ID); !containsInOrder(tr, string(engine.VERIFYING), string(engine.SUCCESS)) {
					t.Errorf("transitions %v do not pass VERIFYING before SUCCESS", tr)
				}
			}
		})
	}
}

// TestRunGuestJobKeepLastAppliesToCopy checks that a job's keep-last
// also trims its copy, and that an explicit copy retention wins.
func TestRunGuestJobKeepLastAppliesToCopy(t *testing.T) {
	for _, tt := range []struct {
		name      string
		retention *inventory.Retention
		want      string
	}{
		{"job keep-last covers the copy", nil, "--keep-last 2"},
		{"copy's own retention wins", &inventory.Retention{KeepLast: 5}, "--keep-last 5"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			f := newFakePVE()
			p := h.guestPipeline(t, f)
			copyDir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(copyDir, "dump"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(copyDir, "dump", "vzdump-lxc-100-101.tar.zst"), make([]byte, 4096), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := inventory.PutService(h.db, inventory.Service{
				Name: "pve-ct100", Kind: inventory.KindProxmoxGuest, ProxmoxNode: "pve", ProxmoxVMID: 100,
				Guest:       &inventory.GuestJob{Storage: "backup", CopyPath: copyDir, KeepLast: 2},
				Destination: "local", Retention: tt.retention,
			}); err != nil {
				t.Fatal(err)
			}
			if state, err := p.Run(context.Background(), "pve-ct100"); state != engine.SUCCESS {
				t.Fatalf("state = %s (%v)", state, err)
			}
			log, _ := os.ReadFile(h.resticLog)
			if !strings.Contains(string(log), tt.want) {
				t.Errorf("restic forget missing %q:\n%s", tt.want, log)
			}
		})
	}
}

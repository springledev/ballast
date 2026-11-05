package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/springledev/ballast/internal/engine"
	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/proxmox"
	"github.com/springledev/keel/secret"
)

// fakeProxmoxServer answers /api2/json/version and
// /api2/json/cluster/resources like a real Proxmox host, checking the
// Authorization header carries the expected token.
func fakeProxmoxServer(t *testing.T, wantAuth string) *httptest.Server {
	t.Helper()
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != wantAuth {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api2/json/version":
			_, _ = w.Write([]byte(`{"data":{"version":"8.2.1"}}`))
		case "/api2/json/cluster/resources":
			_, _ = w.Write([]byte(`{"data":[
				{"node":"pve1","vmid":100,"name":"immich","status":"running","type":"lxc"},
				{"node":"pve1","vmid":101,"name":"win","status":"stopped","type":"qemu"}
			]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestSetupProxmoxConnection(t *testing.T) {
	h := newHarness(t)
	h.ops.Redact = secret.NewSet()
	h.ops.Proxmox = proxmox.Resolver{CredDirs: []string{h.credDir}, Redact: h.ops.Redact}
	srv := fakeProxmoxServer(t, "PVEAPIToken=ballast@pve!backup=s3cr3t")
	defer srv.Close()

	res, err := h.ops.SetupProxmoxConnection(context.Background(), ProxmoxConnectionSetup{
		Connection:  proxmox.Connection{Host: srv.URL, TokenID: "ballast@pve!backup", Insecure: true},
		Credentials: proxmox.Credentials{TokenSecret: "s3cr3t"},
	})
	if err != nil {
		t.Fatalf("SetupProxmoxConnection: %v", err)
	}
	if res.Test == nil || res.Test.Version != "8.2.1" {
		t.Fatalf("Test = %+v, want version 8.2.1", res.Test)
	}
	if res.Connection.Credential != proxmox.DefaultCredentialName {
		t.Errorf("Credential = %q, want %q", res.Connection.Credential, proxmox.DefaultCredentialName)
	}

	// The saved connection round-trips.
	got, err := h.ops.GetProxmoxConnection(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Host != res.Connection.Host || got.TokenID != "ballast@pve!backup" {
		t.Fatalf("GetProxmoxConnection = %+v", got)
	}

	// The token secret is redacted everywhere.
	if h.ops.redact("leaked: s3cr3t") == "leaked: s3cr3t" {
		t.Error("token secret was not registered for redaction")
	}
}

func TestSetupProxmoxConnectionFailedTestLeavesNoTraceOfNewCredential(t *testing.T) {
	h := newHarness(t)
	h.ops.Proxmox = proxmox.Resolver{CredDirs: []string{h.credDir}}
	srv := fakeProxmoxServer(t, "PVEAPIToken=ballast@pve!backup=right-secret")
	defer srv.Close()

	_, err := h.ops.SetupProxmoxConnection(context.Background(), ProxmoxConnectionSetup{
		Connection:  proxmox.Connection{Host: srv.URL, TokenID: "ballast@pve!backup", Insecure: true},
		Credentials: proxmox.Credentials{TokenSecret: "wrong-secret"},
	})
	if err == nil {
		t.Fatal("expected an error from a rejected token")
	}
	if _, err := h.ops.GetProxmoxConnection(context.Background()); !errors.Is(err, inventory.ErrNotFound) {
		t.Fatalf("GetProxmoxConnection after failed setup = %v, want ErrNotFound", err)
	}
}

func TestListProxmoxGuests(t *testing.T) {
	h := newHarness(t)
	h.ops.Proxmox = proxmox.Resolver{CredDirs: []string{h.credDir}}
	srv := fakeProxmoxServer(t, "PVEAPIToken=ballast@pve!backup=s3cr3t")
	defer srv.Close()

	if _, err := h.ops.SetupProxmoxConnection(context.Background(), ProxmoxConnectionSetup{
		Connection:  proxmox.Connection{Host: srv.URL, TokenID: "ballast@pve!backup", Insecure: true},
		Credentials: proxmox.Credentials{TokenSecret: "s3cr3t"},
	}); err != nil {
		t.Fatal(err)
	}

	guests, err := h.ops.ListProxmoxGuests(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(guests) != 2 || guests[0].Name != "immich" || !guests[0].IsLXC() {
		t.Fatalf("guests = %+v", guests)
	}
}

func TestDeleteProxmoxConnectionRemovesCredentialFile(t *testing.T) {
	h := newHarness(t)
	h.ops.Proxmox = proxmox.Resolver{CredDirs: []string{h.credDir}}
	srv := fakeProxmoxServer(t, "PVEAPIToken=ballast@pve!backup=s3cr3t")
	defer srv.Close()

	if _, err := h.ops.SetupProxmoxConnection(context.Background(), ProxmoxConnectionSetup{
		Connection:  proxmox.Connection{Host: srv.URL, TokenID: "ballast@pve!backup", Insecure: true},
		Credentials: proxmox.Credentials{TokenSecret: "s3cr3t"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.ops.DeleteProxmoxConnection(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ops.GetProxmoxConnection(context.Background()); !errors.Is(err, inventory.ErrNotFound) {
		t.Fatalf("GetProxmoxConnection after delete = %v, want ErrNotFound", err)
	}
}

// TestProxmoxConnectAndAttachServiceToGuestEndToEnd exercises spec §14
// step 24's full acceptance criterion end to end, exactly through the
// same Ops layer the CLI and the web UI call: connect to a Proxmox
// host with an API token, list its guests, and attach a catalog
// service to the guest the listing reported — proving the "per-guest
// catalog-service attach" the step promises actually threads the real
// guest identity through, not just a hand-typed one.
func TestProxmoxConnectAndAttachServiceToGuestEndToEnd(t *testing.T) {
	h := newHarness(t)
	h.ops.Redact = secret.NewSet()
	h.ops.Proxmox = proxmox.Resolver{CredDirs: []string{h.credDir}, Redact: h.ops.Redact}
	h.addDestination("d")
	srv := fakeProxmoxServer(t, "PVEAPIToken=ballast@pve!backup=s3cr3t")
	defer srv.Close()

	// 1. Connect (spec: "API token setup guide").
	setupRes, err := h.ops.SetupProxmoxConnection(context.Background(), ProxmoxConnectionSetup{
		Connection:  proxmox.Connection{Host: srv.URL, TokenID: "ballast@pve!backup", Insecure: true},
		Credentials: proxmox.Credentials{TokenSecret: "s3cr3t"},
	})
	if err != nil {
		t.Fatalf("SetupProxmoxConnection: %v", err)
	}
	if setupRes.Test == nil || setupRes.Test.Version == "" {
		t.Fatalf("expected a connection test to have run: %+v", setupRes)
	}

	// 2. List guests (spec: "guest list").
	guests, err := h.ops.ListProxmoxGuests(context.Background())
	if err != nil {
		t.Fatalf("ListProxmoxGuests: %v", err)
	}
	lxc := -1
	for i, g := range guests {
		if g.IsLXC() {
			lxc = i
			break
		}
	}
	if lxc < 0 {
		t.Fatalf("fake Proxmox server reported no LXC guest: %+v", guests)
	}
	guest := guests[lxc]

	// 3. Attach a catalog service to that guest (spec: "per-guest
	// catalog-service attach"), through the same PutService the
	// `service add`/`PUT /api/v1/services/{name}` paths use.
	svc := inventory.Service{
		Name: "immich", CatalogEntry: "folder", Destination: "d",
		Params:      map[string]string{"path": "/data"},
		ProxmoxNode: guest.Node, ProxmoxVMID: guest.VMID,
	}
	if err := h.ops.PutService(context.Background(), svc); err != nil {
		t.Fatalf("PutService: %v", err)
	}

	// 4. The attachment round-trips exactly as attached — the guest's
	// own node/VMID from the live listing, not a placeholder.
	got, err := h.ops.GetService(context.Background(), "immich")
	if err != nil {
		t.Fatalf("GetService: %v", err)
	}
	if got.ProxmoxNode != guest.Node || got.ProxmoxVMID != guest.VMID {
		t.Fatalf("attached guest = node %q vmid %d, want node %q vmid %d", got.ProxmoxNode, got.ProxmoxVMID, guest.Node, guest.VMID)
	}
}

// --- HTTP golden tests -------------------------------------------------

func TestHTTPProxmoxConnectionLifecycle(t *testing.T) {
	h := newHarness(t)
	h.ops.Proxmox = proxmox.Resolver{CredDirs: []string{h.credDir}}
	srv := fakeProxmoxServer(t, "PVEAPIToken=ballast@pve!backup=s3cr3t")
	defer srv.Close()
	mux := NewHandler(h.ops, testAPIToken)

	do := func(method, path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		var req *http.Request
		if body != nil {
			req = httptest.NewRequest(method, path, jsonBody(t, body))
		} else {
			req = httptest.NewRequest(method, path, nil)
		}
		req.Header.Set("Authorization", "Bearer "+testAPIToken)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		return rr
	}

	rr := do(http.MethodPut, "/api/v1/proxmox", map[string]any{
		"host":     srv.URL,
		"token_id": "ballast@pve!backup",
		"insecure": true,
		"credentials": map[string]string{
			"token_secret": "s3cr3t",
		},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT /api/v1/proxmox = %d: %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "s3cr3t") {
		t.Fatalf("response leaked the token secret: %s", rr.Body.String())
	}

	rr = do(http.MethodGet, "/api/v1/proxmox", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/proxmox = %d: %s", rr.Code, rr.Body.String())
	}

	rr = do(http.MethodPost, "/api/v1/proxmox/test", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /api/v1/proxmox/test = %d: %s", rr.Code, rr.Body.String())
	}

	rr = do(http.MethodGet, "/api/v1/proxmox/guests", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/proxmox/guests = %d: %s", rr.Code, rr.Body.String())
	}
	var guests []proxmox.Guest
	if err := json.Unmarshal(rr.Body.Bytes(), &guests); err != nil {
		t.Fatal(err)
	}
	if len(guests) != 2 {
		t.Fatalf("guests = %+v", guests)
	}

	rr = do(http.MethodDelete, "/api/v1/proxmox", nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("DELETE /api/v1/proxmox = %d: %s", rr.Code, rr.Body.String())
	}

	rr = do(http.MethodGet, "/api/v1/proxmox", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("GET /api/v1/proxmox after delete = %d, want 404", rr.Code)
	}
}

func TestBackupProxmoxGuest(t *testing.T) {
	h := newHarness(t)
	h.ops.Redact = secret.NewSet()
	h.ops.Proxmox = proxmox.Resolver{CredDirs: []string{h.credDir}, Redact: h.ops.Redact}

	const upid = "UPID:pve1:0001:0002:0003:vzdump:100:ballast@pve!backup:"
	dumps := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api2/json/version":
			_, _ = w.Write([]byte(`{"data":{"version":"8.2.1"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api2/json/nodes/pve1/vzdump":
			_ = r.ParseForm()
			if r.PostForm.Get("vmid") != "100" || r.PostForm.Get("storage") != "local" || r.PostForm.Get("mode") != "snapshot" {
				t.Errorf("vzdump form = %v", r.PostForm)
			}
			dumps++
			_, _ = w.Write([]byte(`{"data":"` + upid + `"}`))
		case strings.HasSuffix(r.URL.Path, "/status"):
			_, _ = w.Write([]byte(`{"data":{"status":"stopped","exitstatus":"OK"}}`))
		case strings.HasSuffix(r.URL.Path, "/vzdump/extractconfig"):
			_, _ = w.Write([]byte(`{"data":"arch: amd64\nrootfs: local:100/x.raw,size=8G\n"}`))
		case strings.HasSuffix(r.URL.Path, "/storage/local/content"):
			if dumps == 0 {
				_, _ = w.Write([]byte(`{"data":[]}`))
				return
			}
			list := `{"volid":"local:backup/vzdump-lxc-100-new.tar.zst","ctime":10,"size":2048,"vmid":100}`
			if dumps > 1 {
				list = `{"volid":"local:backup/vzdump-lxc-100-newer.tar.zst","ctime":20,"size":2048,"vmid":100},` + list
			}
			_, _ = w.Write([]byte(`{"data":[` + list + `]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	if _, err := h.ops.SetupProxmoxConnection(context.Background(), ProxmoxConnectionSetup{
		Connection:  proxmox.Connection{Host: srv.URL, TokenID: "ballast@pve!backup", Insecure: true},
		Credentials: proxmox.Credentials{TokenSecret: "s3cr3t"},
		SkipTest:    true,
	}); err != nil {
		t.Fatal(err)
	}

	res, err := h.ops.BackupProxmoxGuest(context.Background(), proxmox.GuestBackupOptions{Node: "pve1", VMID: 100, Storage: "local"})
	if err != nil {
		t.Fatalf("BackupProxmoxGuest: %v", err)
	}
	if res.Volume != "local:backup/vzdump-lxc-100-new.tar.zst" || res.UPID != upid || res.Size != 2048 {
		t.Errorf("result = %+v", res)
	}
	if res.Verification == nil || !res.Verification.Verified || len(res.Verification.Layers) != 1 {
		t.Errorf("verification = %+v, want the guest-config layer passed", res.Verification)
	}

	// A service attached to the guest that has never run fails its own
	// in-guest layer, and so the whole guest is not verified — while the
	// guest-config layer still reports its own pass.
	h.addDestination("d")
	if err := h.ops.PutService(context.Background(), inventory.Service{
		Name: "db", CatalogEntry: "folder", Destination: "d",
		Params:      map[string]string{"path": "/data"},
		ProxmoxNode: "pve1", ProxmoxVMID: 100,
	}); err != nil {
		t.Fatalf("PutService: %v", err)
	}
	res, err = h.ops.BackupProxmoxGuest(context.Background(), proxmox.GuestBackupOptions{Node: "pve1", VMID: 100, Storage: "local"})
	if err != nil {
		t.Fatalf("BackupProxmoxGuest with attached service: %v", err)
	}
	if v := res.Verification; v == nil || v.Verified || len(v.Layers) != 2 || !v.Layers[0].Passed || v.Layers[1].Passed {
		t.Errorf("verification = %+v, want config passed, in-guest failed, not verified", v)
	}

	// Missing storage is a validation error, before any network call.
	_, err = h.ops.BackupProxmoxGuest(context.Background(), proxmox.GuestBackupOptions{Node: "pve1", VMID: 100})
	if !errors.Is(err, ErrValidation) {
		t.Errorf("err = %v, want ErrValidation", err)
	}
}

func TestInGuestLayers(t *testing.T) {
	tests := []struct {
		name  string
		state string // "" = never ran
		want  bool
	}{
		{"never ran", "", false},
		{"verified success", "SUCCESS", true},
		{"amber unverified", "SUCCESS_UNVERIFIED", false},
		{"failed", "FAILED", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.addDestination("d")
			if err := h.ops.PutService(context.Background(), inventory.Service{
				Name: "db", CatalogEntry: "folder", Destination: "d",
				Params:      map[string]string{"path": "/data"},
				ProxmoxNode: "pve1", ProxmoxVMID: 100,
			}); err != nil {
				t.Fatal(err)
			}
			if tt.state != "" {
				if _, err := h.ops.DB.Exec(`INSERT INTO jobs (service, created_at) VALUES ('db', '2026-01-01T00:00:00Z')`); err != nil {
					t.Fatal(err)
				}
				if _, err := h.ops.DB.Exec(`INSERT INTO runs (job_id, state, started_at, finished_at) VALUES (1, ?, '2026-01-01T00:00:00Z', '2026-01-01T00:01:00Z')`, tt.state); err != nil {
					t.Fatal(err)
				}
			}
			layers, err := h.ops.inGuestLayers("pve1", 100)
			if err != nil || len(layers) != 1 || layers[0].Passed != tt.want {
				t.Fatalf("layers = %+v, err = %v, want passed=%v", layers, err, tt.want)
			}
		})
	}
}

func TestEditGuestJob(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.ops.Redact = secret.NewSet()
	h.ops.Proxmox = proxmox.Resolver{CredDirs: []string{h.credDir}, Redact: h.ops.Redact}
	h.addDestination("local")
	h.addDestination("b2")
	if _, err := h.ops.SetupProxmoxConnection(ctx, ProxmoxConnectionSetup{
		Connection:  proxmox.Connection{Host: "https://pve.invalid:8006", TokenID: "ballast@pve!backup"},
		Credentials: proxmox.Credentials{TokenSecret: "s3cr3t"}, SkipTest: true,
	}); err != nil {
		t.Fatal(err)
	}
	job := inventory.Service{
		Name: "pve-vm100", Kind: inventory.KindProxmoxGuest, ProxmoxNode: "pve", ProxmoxVMID: 100,
		Guest: &inventory.GuestJob{Storage: "nas", KeepLast: 2},
	}
	if err := h.ops.PutService(ctx, job); err != nil {
		t.Fatal(err)
	}
	finishVerifiedRun(t, h, "pve-vm100")
	defer requireVerifiedRunKept(t, h, "pve-vm100")
	intp := func(i int) *int { return &i }
	strp := func(s string) *string { return &s }

	tests := []struct {
		name    string
		edit    GuestJobEdit
		wantErr bool
		check   func(t *testing.T, s inventory.Service)
	}{
		{name: "keep-last only", edit: GuestJobEdit{KeepLast: intp(5)}, check: func(t *testing.T, s inventory.Service) {
			if s.Guest.KeepLast != 5 || s.Guest.Storage != "nas" {
				t.Errorf("guest = %+v", s.Guest)
			}
		}},
		{name: "negative keep-last", edit: GuestJobEdit{KeepLast: intp(-1)}, wantErr: true},
		{name: "bad cron", edit: GuestJobEdit{Schedule: strp("nope")}, wantErr: true},
		{name: "unknown destination", edit: GuestJobEdit{CopyTo: strp("missing"), CopyPath: strp("/mnt/x")}, wantErr: true},
		{name: "schedule and copy", edit: GuestJobEdit{Schedule: strp("0 4 * * 0"), CopyTo: strp("b2"), CopyPath: strp("/mnt/x")}, check: func(t *testing.T, s inventory.Service) {
			if s.Destination != "b2" || s.Guest.CopyPath != "/mnt/x" || s.Guest.KeepLast != 5 {
				t.Errorf("service = %+v / %+v", s, s.Guest)
			}
			j, err := h.ops.GetSchedule(ctx, "pve-vm100")
			if err != nil || j.Cron != "0 4 * * 0" {
				t.Errorf("schedule = %+v, %v", j, err)
			}
		}},
		{name: "stop copying", edit: GuestJobEdit{CopyTo: strp("")}, check: func(t *testing.T, s inventory.Service) {
			if s.Destination != "" || len(s.Targets) != 0 {
				t.Errorf("still copying: %+v", s)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := h.ops.EditGuestJob(ctx, "pve-vm100", tt.edit)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.check != nil {
				got, err := h.ops.GetService(ctx, "pve-vm100")
				if err != nil {
					t.Fatal(err)
				}
				tt.check(t, got)
			}
		})
	}
	if _, err := h.ops.EditGuestJob(ctx, "nope", GuestJobEdit{KeepLast: intp(1)}); err == nil {
		t.Error("editing a missing job should fail")
	}
}

func TestSetPrimaryTarget(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.addDestination("local")
	h.addDestination("b2")
	h.addDestination("other")
	svc := inventory.Service{
		Name: "immich", CatalogEntry: "folder", Destination: "local",
		Params:  map[string]string{"path": "/data"},
		Targets: []inventory.Target{{Destination: "local"}, {Destination: "b2", Parts: []string{"library"}}},
	}
	if err := h.ops.PutService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	finishVerifiedRun(t, h, "immich")
	defer requireVerifiedRunKept(t, h, "immich")
	if _, err := h.ops.SetPrimaryTarget(ctx, "immich", "other"); err == nil {
		t.Error("a destination the service does not use cannot become main")
	}
	if _, err := h.ops.SetPrimaryTarget(ctx, "missing", "b2"); err == nil {
		t.Error("missing service should fail")
	}
	if _, err := h.ops.SetPrimaryTarget(ctx, "immich", "b2"); err != nil {
		t.Fatal(err)
	}
	got, err := h.ops.GetService(ctx, "immich")
	if err != nil {
		t.Fatal(err)
	}
	if got.Destination != "b2" || len(got.Targets) != 2 || got.Targets[0].Destination != "b2" || got.Targets[1].Destination != "local" {
		t.Errorf("targets = %+v destination = %q", got.Targets, got.Destination)
	}
	if len(got.Targets[0].Parts) != 1 || got.Targets[0].Parts[0] != "library" {
		t.Errorf("parts lost: %+v", got.Targets[0])
	}
}

func TestHTTPPrimaryTargetAndJobEdit(t *testing.T) {
	h := newHarness(t)
	h.addDestination("local")
	h.addDestination("b2")
	if err := h.ops.PutService(context.Background(), inventory.Service{
		Name: "s", CatalogEntry: "folder", Destination: "local", Params: map[string]string{"path": "/d"},
		Targets: []inventory.Target{{Destination: "local"}, {Destination: "b2"}},
	}); err != nil {
		t.Fatal(err)
	}
	mux := NewHandler(h.ops, testAPIToken)
	do := func(method, path string, body any) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, jsonBody(t, body))
		req.Header.Set("Authorization", "Bearer "+testAPIToken)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		return rr
	}
	if rr := do(http.MethodPut, "/api/v1/services/s/primary-target", map[string]string{"destination": "b2"}); rr.Code != http.StatusOK {
		t.Fatalf("primary-target = %d: %s", rr.Code, rr.Body.String())
	}
	if got, _ := h.ops.GetService(context.Background(), "s"); got.Destination != "b2" {
		t.Errorf("destination = %q", got.Destination)
	}
	if rr := do(http.MethodPatch, "/api/v1/proxmox/jobs/s", map[string]int{"keep_last": 2}); rr.Code == http.StatusOK {
		t.Errorf("editing a non-guest service as a job should fail: %s", rr.Body.String())
	}
}

// finishVerifiedRun walks a run for service through VERIFYING to SUCCESS.
func finishVerifiedRun(t *testing.T, h *harness, service string) {
	t.Helper()
	m, err := engine.StartRun(h.ops.DB, service)
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range []engine.State{engine.PREPARING, engine.DUMPING, engine.UPLOADING, engine.VERIFYING, engine.SUCCESS} {
		if err := m.Transition(st, "test"); err != nil {
			t.Fatalf("transition to %s: %v", st, err)
		}
	}
}

// requireVerifiedRunKept fails unless service's latest run is still SUCCESS.
func requireVerifiedRunKept(t *testing.T, h *harness, service string) {
	t.Helper()
	runs, err := engine.RecentRuns(h.ops.DB, service, 1)
	if err != nil || len(runs) != 1 || runs[0].State != engine.SUCCESS {
		t.Fatalf("run history lost after edit: %+v, %v", runs, err)
	}
}

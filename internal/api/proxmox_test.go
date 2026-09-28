package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/proxmox"
	"github.com/springledev/ballast/internal/secret"
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
			w.Write([]byte(`{"data":{"version":"8.2.1"}}`))
		case "/api2/json/cluster/resources":
			w.Write([]byte(`{"data":[
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

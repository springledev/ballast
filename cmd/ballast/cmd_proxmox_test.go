package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeProxmoxServer answers a Proxmox API version + guest list call,
// checking the Authorization header carries the expected token.
func fakeProxmoxServer(t *testing.T, wantAuth string) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != wantAuth {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api2/json/version":
			w.Write([]byte(`{"data":{"version":"8.2.1"}}`))
		case "/api2/json/cluster/resources":
			w.Write([]byte(`{"data":[{"node":"pve1","vmid":100,"name":"immich","status":"running","type":"lxc"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestProxmoxConnectTestGuestsRemove(t *testing.T) {
	cliEnv(t, &spyRunner{})
	srv := fakeProxmoxServer(t, "PVEAPIToken=ballast@pve!backup=s3cr3t")

	out, err := execCLI(t, "proxmox", "connect",
		"--host", srv.URL, "--token-id", "ballast@pve!backup", "--token-secret", "s3cr3t", "--insecure")
	if err != nil {
		t.Fatalf("proxmox connect: %v (output %q)", err, out)
	}
	if !strings.Contains(out, "configured") || !strings.Contains(out, "8.2.1") {
		t.Errorf("output = %q, want it to confirm and report the version", out)
	}
	if strings.Contains(out, "s3cr3t") {
		t.Errorf("output leaks the token secret: %q", out)
	}

	out, err = execCLI(t, "proxmox", "test")
	if err != nil {
		t.Fatalf("proxmox test: %v (output %q)", err, out)
	}
	if !strings.Contains(out, "8.2.1") {
		t.Errorf("output = %q, want the version", out)
	}

	out, err = execCLI(t, "proxmox", "guests")
	if err != nil {
		t.Fatalf("proxmox guests: %v (output %q)", err, out)
	}
	if !strings.Contains(out, "immich") || !strings.Contains(out, "lxc") {
		t.Errorf("output = %q, want the guest listed", out)
	}

	if _, err := execCLI(t, "proxmox", "remove"); err != nil {
		t.Fatalf("proxmox remove: %v", err)
	}
	if _, err := execCLI(t, "proxmox", "test"); err == nil {
		t.Fatal("proxmox test after remove: want an error")
	}
}

func TestProxmoxConnectNoTest(t *testing.T) {
	cliEnv(t, &spyRunner{})

	out, err := execCLI(t, "proxmox", "connect",
		"--host", "10.0.0.9", "--token-id", "ballast@pve!backup", "--token-secret", "s3cr3t", "--no-test")
	if err != nil {
		t.Fatalf("proxmox connect --no-test: %v (output %q)", err, out)
	}
	if !strings.Contains(out, "not tested") {
		t.Errorf("output = %q, want it to say the connection was not tested", out)
	}
}

func TestProxmoxConnectTokenSecretFileNeverLeaks(t *testing.T) {
	cliEnv(t, &spyRunner{})
	secretFile := filepath.Join(t.TempDir(), "token.txt")
	if err := os.WriteFile(secretFile, []byte("very-secret-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := execCLI(t, "proxmox", "connect",
		"--host", "10.0.0.9", "--token-id", "ballast@pve!backup", "--token-secret-file", secretFile, "--no-test")
	if err != nil {
		t.Fatalf("proxmox connect: %v (output %q)", err, out)
	}
	if strings.Contains(out, "very-secret-value") {
		t.Errorf("output leaks the token secret: %q", out)
	}
}

func TestProxmoxConnectRequiresHostAndTokenID(t *testing.T) {
	cliEnv(t, &spyRunner{})
	if _, err := execCLI(t, "proxmox", "connect", "--token-id", "ballast@pve!backup"); err == nil {
		t.Fatal("proxmox connect without --host: want an error")
	}
	if _, err := execCLI(t, "proxmox", "connect", "--host", "10.0.0.9"); err == nil {
		t.Fatal("proxmox connect without --token-id: want an error")
	}
}

func TestServiceAddProxmoxGuestAttachment(t *testing.T) {
	withStubRestic(t)
	cliEnv(t, &spyRunner{})

	if _, err := execCLI(t, "destination", "add", "local", "--location", "/srv/backups"); err != nil {
		t.Fatalf("destination add: %v", err)
	}
	out, err := execCLI(t, "service", "add", "svc", "--catalog", "folder", "--destination", "local",
		"--param", "path=/tmp", "--proxmox-node", "pve1", "--proxmox-vmid", "100")
	if err != nil {
		t.Fatalf("service add: %v (output %q)", err, out)
	}
	if !strings.Contains(out, "pve1") || !strings.Contains(out, "100") {
		t.Errorf("output = %q, want it to mention the Proxmox guest", out)
	}
}

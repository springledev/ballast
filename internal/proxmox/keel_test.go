package proxmox

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	kp "github.com/springledev/keel/proxmox"
	"github.com/springledev/keel/secret"
)

// TestDefaultCredentialNameUnchanged pins the credential file name
// existing installs already have on disk.
func TestDefaultCredentialNameUnchanged(t *testing.T) {
	if DefaultCredentialName != "ballast-proxmox-token" {
		t.Fatalf("DefaultCredentialName = %q, want ballast-proxmox-token", DefaultCredentialName)
	}
	if want := []byte("ballast-proxmox-token"); string(want) != kp.DefaultCredentialName(Product) {
		t.Fatalf("keel's name for %q = %q, want %q", Product, kp.DefaultCredentialName(Product), want)
	}
}

func TestNormalizeConnectionMessages(t *testing.T) {
	tests := []struct {
		name string
		in   Connection
		want string
	}{
		{"user in address", Connection{Host: "https://root@pve.local", TokenID: "ballast@pve!backup"}, "don't put a user in the address; Ballast connects with an API token"},
		{"empty token", Connection{Host: "pve.local"}, "the API token ID is required, e.g. ballast@pve!backup"},
		{"http scheme", Connection{Host: "http://pve.local", TokenID: "ballast@pve!backup"}, "the Proxmox API is reached over HTTPS; got \"http://pve.local\""},
		{"ok", Connection{Host: "pve.local", TokenID: " ballast@pve!backup "}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := tt.in
			err := NormalizeConnection(&c)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				if c.Host != "https://pve.local:8006" || c.TokenID != "ballast@pve!backup" {
					t.Fatalf("normalized = %+v", c)
				}
				return
			}
			if err == nil || err.Error() != tt.want {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestRejectedTokenMessage(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
		}))
		cl := &Client{Client: &kp.Client{Host: srv.URL, TokenID: "x@pve!y", TokenSecret: "tok-secret", HTTPClient: srv.Client()}}
		ctx := context.Background()
		_, errTest := cl.Test(ctx)
		_, errList := cl.ListGuests(ctx)
		_, errStore := cl.StorageStatus(ctx, "pve1", "local")
		srv.Close()
		want := fmt.Sprintf("the Proxmox API token was rejected (HTTP %d) — check the token ID and secret, and that the token has the VM.Backup permission on /vms", code)
		for _, err := range []error{errTest, errList} {
			if err == nil || err.Error() != want {
				t.Errorf("HTTP %d: err = %v, want %q", code, err, want)
			}
		}
		for _, err := range []error{errTest, errList, errStore} {
			if err == nil || !strings.Contains(err.Error(), "the VM.Backup permission on /vms") || !strings.Contains(err.Error(), "(HTTP ") {
				t.Errorf("HTTP %d: err = %v, want Ballast's VM.Backup hint", code, err)
			}
			if err != nil && strings.Contains(err.Error(), "tok-secret") {
				t.Errorf("error leaks the token secret: %v", err)
			}
		}
	}
}

func TestResolverClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "PVEAPIToken=ballast@pve!backup=secret-123" {
			t.Errorf("Authorization header = %q", got)
		}
		_, _ = w.Write([]byte(`{"data":{"version":"8.2.1"}}`))
	}))
	defer srv.Close()
	dir := t.TempDir()
	data, _, err := RenderCredentials(Credentials{TokenSecret: "secret-123"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, DefaultCredentialName), data, 0o600); err != nil {
		t.Fatal(err)
	}
	red := secret.NewSet()
	cl, err := Resolver{CredDirs: []string{dir}, Redact: red, HTTPClient: srv.Client()}.Client(Connection{Host: srv.URL, TokenID: "ballast@pve!backup", Credential: DefaultCredentialName})
	if err != nil {
		t.Fatalf("Client: %v", err)
	}
	res, err := cl.Test(context.Background())
	if err != nil || res.Version != "8.2.1" {
		t.Fatalf("Test() = %+v, %v", res, err)
	}
	if got := red.Redact("secret-123"); strings.Contains(got, "secret-123") {
		t.Errorf("resolver did not register the token secret for redaction: %q", got)
	}
}

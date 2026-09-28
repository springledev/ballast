package proxmox

import (
	"strings"
	"testing"
)

func TestConnectionNormalize(t *testing.T) {
	tests := []struct {
		name        string
		conn        Connection
		wantHost    string
		wantTokenID string
		wantErr     string
	}{
		{"bare host", Connection{Host: "192.168.1.10", TokenID: "ballast@pve!backup"}, "https://192.168.1.10:8006", "ballast@pve!backup", ""},
		{"host with port", Connection{Host: "pve.local:8443", TokenID: "ballast@pve!backup"}, "https://pve.local:8443", "ballast@pve!backup", ""},
		{"https url", Connection{Host: "https://pve.local:8006", TokenID: "ballast@pve!backup"}, "https://pve.local:8006", "ballast@pve!backup", ""},
		{"trims spaces", Connection{Host: "  pve.local  ", TokenID: " ballast@pve!backup "}, "https://pve.local:8006", "ballast@pve!backup", ""},
		{"empty host", Connection{TokenID: "ballast@pve!backup"}, "", "", "address is required"},
		{"http scheme", Connection{Host: "http://pve.local", TokenID: "ballast@pve!backup"}, "", "", "reached over HTTPS"},
		{"user in address", Connection{Host: "https://root@pve.local", TokenID: "ballast@pve!backup"}, "", "", "don't put a user"},
		{"path in address", Connection{Host: "https://pve.local/api2", TokenID: "ballast@pve!backup"}, "", "", "nothing after it"},
		{"empty token", Connection{Host: "pve.local"}, "", "", "token ID is required"},
		{"bad token format", Connection{Host: "pve.local", TokenID: "not-a-token"}, "", "", "doesn't look like a Proxmox API token"},
		{"token missing bang", Connection{Host: "pve.local", TokenID: "ballast@pve"}, "", "", "doesn't look like a Proxmox API token"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := tt.conn
			err := c.Normalize()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Normalize() error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Normalize() unexpected error: %v", err)
			}
			if c.Host != tt.wantHost {
				t.Errorf("Host = %q, want %q", c.Host, tt.wantHost)
			}
			if c.TokenID != tt.wantTokenID {
				t.Errorf("TokenID = %q, want %q", c.TokenID, tt.wantTokenID)
			}
		})
	}
}

func TestRenderCredentials(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		_, _, err := RenderCredentials(Credentials{})
		if err != ErrNoCredentials {
			t.Fatalf("err = %v, want ErrNoCredentials", err)
		}
	})
	t.Run("multiline rejected", func(t *testing.T) {
		_, _, err := RenderCredentials(Credentials{TokenSecret: "abc\ndef"})
		if err == nil || !strings.Contains(err.Error(), "single line") {
			t.Fatalf("err = %v, want single-line complaint", err)
		}
	})
	t.Run("valid", func(t *testing.T) {
		data, secrets, err := RenderCredentials(Credentials{TokenSecret: "  abc-123  "})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if string(data) != "abc-123\n" {
			t.Errorf("data = %q, want %q", data, "abc-123\n")
		}
		if len(secrets) != 1 || secrets[0] != "abc-123" {
			t.Errorf("secrets = %v, want [abc-123]", secrets)
		}
	})
}

func TestGuestIsLXC(t *testing.T) {
	if !(Guest{Type: "lxc"}).IsLXC() {
		t.Error("lxc guest should report IsLXC")
	}
	if (Guest{Type: "qemu"}).IsLXC() {
		t.Error("qemu guest should not report IsLXC")
	}
}

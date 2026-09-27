package dockerhost

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestNormalize(t *testing.T) {
	tests := []struct {
		name    string
		host    Host
		want    string
		wantErr string
	}{
		{"local default socket", Host{Kind: Local}, "", ""},
		{"local socket path", Host{Kind: Local, Address: "/run/docker.sock"}, "unix:///run/docker.sock", ""},
		{"local unix url", Host{Kind: Local, Address: "unix:///run//docker.sock"}, "unix:///run/docker.sock", ""},
		{"local relative path", Host{Kind: Local, Address: "docker.sock"}, "", "socket path"},
		{"local with credential", Host{Kind: Local, Credential: "x"}, "", "does not use credentials"},
		{"ssh short form", Host{Kind: RemoteSSH, Address: "ballast@nas"}, "ssh://ballast@nas", ""},
		{"ssh with port", Host{Kind: RemoteSSH, Address: "ssh://ballast@10.0.0.2:2222"}, "ssh://ballast@10.0.0.2:2222", ""},
		{"ssh trims spaces", Host{Kind: RemoteSSH, Address: "  ballast@nas "}, "ssh://ballast@nas", ""},
		{"ssh ipv6", Host{Kind: RemoteSSH, Address: "ssh://me@[fd00::2]:22"}, "ssh://me@[fd00::2]:22", ""},
		{"ssh empty", Host{Kind: RemoteSSH}, "", "enter the machine"},
		{"ssh no user", Host{Kind: RemoteSSH, Address: "nas"}, "", "include the user"},
		{"ssh password", Host{Kind: RemoteSSH, Address: "me:hunter2@nas"}, "", "don't put a password"},
		{"ssh path", Host{Kind: RemoteSSH, Address: "ssh://me@nas/x"}, "", "nothing after it"},
		{"ssh bad port", Host{Kind: RemoteSSH, Address: "ssh://me@nas:99999"}, "", "not a valid port"},
		{"ssh option injection", Host{Kind: RemoteSSH, Address: "ssh://me@-oProxyCommand=x"}, "", "not valid"},
		{"ssh wrong scheme", Host{Kind: RemoteSSH, Address: "tcp://me@nas"}, "", "not an SSH address"},
		{"tls default port", Host{Kind: RemoteTLS, Address: "10.0.0.3", Credential: "c"}, "tcp://10.0.0.3:2376", ""},
		{"tls port", Host{Kind: RemoteTLS, Address: "media:12376", Credential: "c"}, "tcp://media:12376", ""},
		{"tls url", Host{Kind: RemoteTLS, Address: "tcp://media:2376", Credential: "c"}, "tcp://media:2376", ""},
		{"tls no credential", Host{Kind: RemoteTLS, Address: "media"}, "", "needs its CA"},
		{"tls user", Host{Kind: RemoteTLS, Address: "tcp://me@media", Credential: "c"}, "", "no user part"},
		{"tls empty", Host{Kind: RemoteTLS, Credential: "c"}, "", "enter the machine"},
		{"bad kind", Host{Kind: "ftp"}, "", "unknown kind"},
		{"credential path", Host{Kind: RemoteSSH, Address: "a@b", Credential: "../key"}, "", "plain file name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := tt.host
			h.Name = "h"
			err := h.Normalize()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Normalize() error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Normalize() error = %v", err)
			}
			if h.Address != tt.want {
				t.Errorf("Address = %q, want %q", h.Address, tt.want)
			}
		})
	}
}

// testPEMs returns a self-signed CA certificate and an unencrypted EC
// private key, PEM-encoded.
func testPEMs(t *testing.T) (cert, key string) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kder}))
}

const testSSHKey = "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW\nQyNTUxOQAAACDtestkeytestkeytestkeytestkeytestkeytestkeyAAAA\n-----END OPENSSH PRIVATE KEY-----\n"

func TestRenderCredentials(t *testing.T) {
	cert, key := testPEMs(t)
	tests := []struct {
		name      string
		kind      Kind
		creds     Credentials
		wantFiles []string
		wantErr   string
	}{
		{"ssh key", RemoteSSH, Credentials{SSHPrivateKey: testSSHKey}, []string{"c"}, ""},
		{"ssh with tls fields", RemoteSSH, Credentials{SSHPrivateKey: testSSHKey, CACert: cert}, nil, "don't apply to an SSH host"},
		{"ssh not a key", RemoteSSH, Credentials{SSHPrivateKey: "hello"}, nil, "private key file's full contents"},
		{"tls complete", RemoteTLS, Credentials{CACert: cert, ClientCert: cert, ClientKey: key}, []string{"c.ca.pem", "c.cert.pem", "c.key.pem"}, ""},
		{"tls ssh key", RemoteTLS, Credentials{SSHPrivateKey: testSSHKey}, nil, "doesn't apply to a TLS host"},
		{"tls missing ca", RemoteTLS, Credentials{ClientCert: cert, ClientKey: key}, nil, "CA certificate (ca.pem) is missing"},
		{"tls key as cert", RemoteTLS, Credentials{CACert: key, ClientCert: cert, ClientKey: key}, nil, "is it the key?"},
		{"tls garbage cert", RemoteTLS, Credentials{CACert: "nope", ClientCert: cert, ClientKey: key}, nil, "full contents"},
		{"tls cert as key", RemoteTLS, Credentials{CACert: cert, ClientCert: cert, ClientKey: cert}, nil, "key.pem file's full contents"},
		{"tls encrypted key", RemoteTLS, Credentials{CACert: cert, ClientCert: cert, ClientKey: "-----BEGIN ENCRYPTED PRIVATE KEY-----\nAAAA\n-----END ENCRYPTED PRIVATE KEY-----"}, nil, "passphrase"},
		{"local", Local, Credentials{SSHPrivateKey: testSSHKey}, nil, "does not take credentials"},
		{"empty", RemoteSSH, Credentials{}, nil, "no credentials"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files, secrets, err := RenderCredentials(tt.kind, "c", tt.creds)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("error = %v", err)
			}
			var names []string
			for _, f := range files {
				names = append(names, f.Name)
				if !strings.HasSuffix(string(f.Data), "-----\n") {
					t.Errorf("%s does not end with a PEM footer and newline", f.Name)
				}
			}
			if !reflect.DeepEqual(names, tt.wantFiles) {
				t.Errorf("files = %v, want %v", names, tt.wantFiles)
			}
			if len(secrets) != 1 || !strings.Contains(string(files[len(files)-1].Data), secrets[0]) {
				t.Errorf("secrets = %d values, want the key file's contents", len(secrets))
			}
		})
	}
}

func TestDockerCommand(t *testing.T) {
	var zero Docker
	c := zero.Command(nil, "ps")
	if c.Name != "docker" || !reflect.DeepEqual(c.Args, []string{"ps"}) || c.Env != nil {
		t.Errorf("zero Docker command = %+v, want plain docker ps", c)
	}
	if zero.Name() != LocalName {
		t.Errorf("zero Docker name = %q, want local", zero.Name())
	}
	d := Docker{HostName: "nas", bin: "/stub", global: []string{"--host", "ssh://a@b"}, env: []string{"A=1"}}
	c = d.Command([]string{"B=2"}, "exec", "x")
	if c.Name != "/stub" || !reflect.DeepEqual(c.Args, []string{"--host", "ssh://a@b", "exec", "x"}) || !reflect.DeepEqual(c.Env, []string{"A=1", "B=2"}) {
		t.Errorf("command = %+v", c)
	}
	// Building a command must never alias the handle's own slices.
	c.Args[0] = "mutated"
	c.Env[0] = "mutated"
	if d.global[0] != "--host" || d.env[0] != "A=1" {
		t.Error("Command aliased the handle's global args or env")
	}
}

// Package dockerhost models the Docker daemons Ballast talks to (spec
// §17.2): the local socket, a remote daemon over SSH, or a remote
// daemon over TCP with TLS client certificates. Every Docker operation
// Ballast performs — discovery (`docker ps`), dumps (`docker exec`),
// stop/start, verification drills — is built from a Docker handle
// resolved for one named host, so nothing ever silently assumes the
// local daemon.
//
// Ballast does not create `docker context` entries: those live in the
// service user's ~/.docker and are shared, mutable global state. It
// passes the equivalent connection flags (-H, --tlsverify, ...) on
// every call instead, which is exactly what a context expands to and
// keeps each host's settings in Ballast's own database and credential
// store.
package dockerhost

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/springledev/ballast/internal/backend"
	"github.com/springledev/ballast/internal/runner"
)

// Kind is how Ballast reaches a Docker daemon.
type Kind string

// The supported host kinds.
const (
	// Local is the Docker daemon on the machine Ballast runs on,
	// through its Unix socket.
	Local Kind = "local"
	// RemoteSSH is a daemon on another machine, reached with
	// `ssh://user@host` (the docker CLI runs `docker system
	// dial-stdio` over SSH). Preferred for homelabs: no certificates.
	RemoteSSH Kind = "remote-ssh"
	// RemoteTLS is a daemon listening on TCP (normally port 2376)
	// that requires TLS client certificates.
	RemoteTLS Kind = "remote-tls"
)

// LocalName is the name of the built-in local host. It always exists
// (migration 008 creates it) and is what a service uses when no host
// is chosen.
const LocalName = "local"

// DefaultTLSPort is the Docker daemon's conventional TLS port.
const DefaultTLSPort = "2376"

// Valid reports whether k is a known kind.
func (k Kind) Valid() bool {
	switch k {
	case Local, RemoteSSH, RemoteTLS:
		return true
	}
	return false
}

// Host is one configured Docker host. It holds no secrets: an SSH key
// or TLS client key lives in root-only credential files, referenced by
// name (Credential) only.
type Host struct {
	Name string `json:"name"`
	Kind Kind   `json:"kind"`
	// Address is where the daemon is. local: empty (Docker's default
	// socket, honouring DOCKER_HOST in Ballast's environment) or a
	// unix:// socket path; remote-ssh: ssh://user@host[:port];
	// remote-tls: tcp://host:port. Normalize fills in the canonical
	// form from shorter spellings (user@host, host, host:port).
	Address string `json:"address"`
	// Credential is the base name of the host's credential file(s).
	// remote-ssh: the private key file (optional — without one, the
	// Ballast service user's own SSH keys and agent are used).
	// remote-tls: required; the files are Credential+".ca.pem",
	// ".cert.pem" and ".key.pem" (see TLSFiles).
	Credential string `json:"credential,omitempty"`
}

// DefaultCredentialName is the credential base name a host gets when
// its credentials are saved through Ballast.
func DefaultCredentialName(host string) string {
	return "ballast-dockerhost." + host
}

// TLSFiles returns the CA certificate, client certificate and client
// key credential file names for a remote-tls credential base name.
func TLSFiles(credential string) (ca, cert, key string) {
	return credential + ".ca.pem", credential + ".cert.pem", credential + ".key.pem"
}

// Normalize validates h's kind and address and rewrites Address into
// its canonical form. It does not validate Name (callers use
// inventory.ValidateName) and does not touch the credential store.
func (h *Host) Normalize() error {
	if !h.Kind.Valid() {
		return fmt.Errorf("docker host %s: unknown kind %q (use local, remote-ssh or remote-tls)", h.Name, h.Kind)
	}
	if h.Credential != "" && filepath.Base(h.Credential) != h.Credential {
		return fmt.Errorf("docker host %s: credential file name %q must be a plain file name (no path separators)", h.Name, h.Credential)
	}
	addr, err := normalizeAddress(h.Kind, strings.TrimSpace(h.Address))
	if err != nil {
		return fmt.Errorf("docker host %s: %w", h.Name, err)
	}
	h.Address = addr
	switch h.Kind {
	case Local:
		if h.Credential != "" {
			return fmt.Errorf("docker host %s: a local host does not use credentials", h.Name)
		}
	case RemoteTLS:
		if h.Credential == "" {
			return fmt.Errorf("docker host %s: a TLS host needs its CA certificate, client certificate and client key", h.Name)
		}
	}
	return nil
}

func normalizeAddress(k Kind, a string) (string, error) {
	if strings.ContainsAny(a, " \t\r\n") {
		return "", fmt.Errorf("the address %q contains spaces", a)
	}
	switch k {
	case Local:
		if a == "" {
			return "", nil
		}
		p := strings.TrimPrefix(a, "unix://")
		if !filepath.IsAbs(p) {
			return "", fmt.Errorf("a local host's address must be empty (the default socket) or a socket path such as /var/run/docker.sock")
		}
		return "unix://" + filepath.Clean(p), nil
	case RemoteSSH:
		if a == "" {
			return "", fmt.Errorf("enter the machine to connect to, such as ballast@192.168.1.20")
		}
		if !strings.Contains(a, "://") {
			a = "ssh://" + a
		}
		u, err := url.Parse(a)
		if err != nil || u.Scheme != "ssh" {
			return "", fmt.Errorf("%q is not an SSH address; use user@host or ssh://user@host:port", a)
		}
		if err := checkHostPort(u); err != nil {
			return "", err
		}
		if _, hasPass := u.User.Password(); hasPass {
			return "", fmt.Errorf("don't put a password in the address; Ballast connects with an SSH key")
		}
		if u.User == nil || u.User.Username() == "" {
			return "", fmt.Errorf("include the user to log in as, such as ballast@%s", u.Host)
		}
		return "ssh://" + u.User.Username() + "@" + u.Host, nil
	case RemoteTLS:
		if a == "" {
			return "", fmt.Errorf("enter the machine to connect to, such as 192.168.1.20 (port %s is assumed)", DefaultTLSPort)
		}
		if !strings.Contains(a, "://") {
			a = "tcp://" + a
		}
		u, err := url.Parse(a)
		if err != nil || (u.Scheme != "tcp" && u.Scheme != "https") {
			return "", fmt.Errorf("%q is not a TCP address; use host, host:port or tcp://host:port", a)
		}
		if u.User != nil {
			return "", fmt.Errorf("a TLS address has no user part; the client certificate identifies Ballast")
		}
		if u.Port() == "" {
			u.Host = net.JoinHostPort(u.Hostname(), DefaultTLSPort)
		}
		if err := checkHostPort(u); err != nil {
			return "", err
		}
		return "tcp://" + u.Host, nil
	}
	return "", fmt.Errorf("unknown kind %q", k)
}

func checkHostPort(u *url.URL) error {
	if u.Hostname() == "" {
		return fmt.Errorf("the address %q has no host name", u.String())
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("the address %q should be just the host (and port), with nothing after it", u.String())
	}
	if strings.HasPrefix(u.Hostname(), "-") {
		return fmt.Errorf("the host name %q is not valid", u.Hostname())
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("the port %q is not a valid port number", p)
		}
	}
	return nil
}

// Credentials is what a user pastes into a Docker host's credential
// form. It is write-only: it only ever reaches the credential files
// RenderCredentials produces, never the database or an API response.
type Credentials struct {
	// SSHPrivateKey is a remote-ssh host's private key (unencrypted).
	SSHPrivateKey string `json:"ssh_private_key,omitempty"`
	// CACert, ClientCert and ClientKey are a remote-tls host's PEM
	// files (ca.pem, cert.pem, key.pem in Docker's documentation).
	CACert     string `json:"ca_cert,omitempty"`
	ClientCert string `json:"client_cert,omitempty"`
	ClientKey  string `json:"client_key,omitempty"`
}

// Empty reports whether no credential field was filled in.
func (c Credentials) Empty() bool {
	return c.SSHPrivateKey == "" && c.CACert == "" && c.ClientCert == "" && c.ClientKey == ""
}

// ErrNoCredentials is returned by RenderCredentials when c is empty.
var ErrNoCredentials = errors.New("no credentials given")

// File is one credential file to write: a plain file name and its
// contents.
type File struct {
	Name string
	Data []byte
}

// RenderCredentials validates c for a host of kind k whose credential
// base name is credential, and returns the files to write plus the
// secret values the caller must register for redaction first.
func RenderCredentials(k Kind, credential string, c Credentials) ([]File, []string, error) {
	if c.Empty() {
		return nil, nil, ErrNoCredentials
	}
	switch k {
	case RemoteSSH:
		if c.CACert != "" || c.ClientCert != "" || c.ClientKey != "" {
			return nil, nil, fmt.Errorf("TLS certificates don't apply to an SSH host; it uses an SSH private key")
		}
		data, secrets, err := backend.RenderCredentials(backend.SSHCredential, backend.Credentials{SSHPrivateKey: c.SSHPrivateKey})
		if err != nil {
			return nil, nil, err
		}
		return []File{{Name: credential, Data: data}}, secrets, nil
	case RemoteTLS:
		if c.SSHPrivateKey != "" {
			return nil, nil, fmt.Errorf("an SSH key doesn't apply to a TLS host; it uses a CA certificate, client certificate and client key")
		}
		ca, err := pemCert("CA certificate (ca.pem)", c.CACert)
		if err != nil {
			return nil, nil, err
		}
		cert, err := pemCert("client certificate (cert.pem)", c.ClientCert)
		if err != nil {
			return nil, nil, err
		}
		key, err := pemKey(c.ClientKey)
		if err != nil {
			return nil, nil, err
		}
		caName, certName, keyName := TLSFiles(credential)
		return []File{
			{Name: caName, Data: ca},
			{Name: certName, Data: cert},
			{Name: keyName, Data: key},
		}, []string{strings.TrimSpace(string(key))}, nil
	}
	return nil, nil, fmt.Errorf("a local Docker host does not take credentials")
}

func pemCert(what, s string) ([]byte, error) {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
	if s == "" {
		return nil, fmt.Errorf("the %s is missing", what)
	}
	rest := []byte(s)
	n := 0
	for {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			break
		}
		if b.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("the %s must contain only certificates (found a %q block — is it the key?)", what, b.Type)
		}
		if _, err := x509.ParseCertificate(b.Bytes); err != nil {
			return nil, fmt.Errorf("the %s could not be read: %w", what, err)
		}
		n++
	}
	if n == 0 || strings.TrimSpace(string(rest)) != "" {
		return nil, fmt.Errorf("the %s must be the file's full contents, from -----BEGIN CERTIFICATE----- to -----END CERTIFICATE-----", what)
	}
	return []byte(s + "\n"), nil
}

func pemKey(s string) ([]byte, error) {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
	if s == "" {
		return nil, fmt.Errorf("the client key (key.pem) is missing")
	}
	b, rest := pem.Decode([]byte(s))
	if b == nil || !strings.HasSuffix(b.Type, "PRIVATE KEY") || strings.TrimSpace(string(rest)) != "" {
		return nil, fmt.Errorf("the client key must be the key.pem file's full contents, from -----BEGIN ... PRIVATE KEY----- to the matching END line")
	}
	// PKCS#8 "ENCRYPTED PRIVATE KEY", or legacy PEM encryption headers.
	if strings.Contains(b.Type, "ENCRYPTED") || strings.Contains(b.Headers["Proc-Type"], "ENCRYPTED") {
		return nil, fmt.Errorf("the client key is protected by a passphrase; Ballast runs unattended and needs a key without one")
	}
	return []byte(s + "\n"), nil
}

// Docker is a ready-to-run docker CLI handle for one host: every
// command built from it carries that host's connection flags and
// environment. The zero value is the local daemon with Ballast's
// ambient Docker settings and "docker" from $PATH — what every caller
// got before Docker hosts existed.
type Docker struct {
	// HostName names the host, for error messages ("" = local).
	HostName string
	bin      string
	global   []string
	env      []string
}

// NewLocal returns the local-host handle using bin ("" = "docker").
// Tests use it to point commands at a stub.
func NewLocal(bin string) Docker { return Docker{bin: bin} }

// Bin is the docker program this handle runs.
func (d Docker) Bin() string {
	if d.bin == "" {
		return "docker"
	}
	return d.bin
}

// Name is the host's name for messages: "local" for the zero value.
func (d Docker) Name() string {
	if d.HostName == "" {
		return LocalName
	}
	return d.HostName
}

// Command returns a docker command running args against this host.
// env is appended after the host's own environment.
func (d Docker) Command(env []string, args ...string) runner.Command {
	a := make([]string, 0, len(d.global)+len(args))
	a = append(append(a, d.global...), args...)
	var e []string
	if len(d.env)+len(env) > 0 {
		e = make([]string, 0, len(d.env)+len(env))
		e = append(append(e, d.env...), env...)
	}
	return runner.Command{Name: d.Bin(), Args: a, Env: e}
}

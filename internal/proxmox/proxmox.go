// Package proxmox talks to one Proxmox VE host's REST API (spec
// §17.3): Ballast authenticates with an API token — never SSH — and
// lists the host's guests (VMs and LXCs). Orchestrating backups
// (vzdump) and verification drills are separate, later steps (spec
// §14 steps 25–26); this package only connects and reads inventory
// (spec §14 step 24, "connection + inventory").
package proxmox

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
)

// DefaultPort is the Proxmox API's conventional HTTPS port.
const DefaultPort = "8006"

// tokenIDPattern matches a Proxmox API token ID: "user@realm!tokenname".
var tokenIDPattern = regexp.MustCompile(`^[^@!\s]+@[^@!\s]+![^\s]+$`)

// Connection is Ballast's one configured Proxmox host (spec §17: "one
// Proxmox host plus its Docker hosts"). It holds no secret: the API
// token's secret half lives in a root-only credential file,
// referenced by name (Credential) only — the same pattern
// destinations and Docker hosts use.
type Connection struct {
	// Host is the API base address, e.g. "https://192.168.1.10:8006".
	// Normalize fills in "https://" and the default port from shorter
	// spellings (a bare host, or host:port).
	Host string `json:"host"`
	// TokenID is the API token's public half: "user@realm!tokenname",
	// e.g. "ballast@pve!backup".
	TokenID string `json:"token_id"`
	// Credential is the name of the root-only credential file holding
	// the token's secret (a UUID Proxmox generates for the token).
	Credential string `json:"credential,omitempty"`
	// Insecure skips TLS certificate verification. Proxmox ships a
	// self-signed certificate by default, so this is common in a
	// homelab — but skipping verification means a network attacker
	// able to intercept the connection could steal the API token, so
	// it defaults off; trusting Proxmox's own CA certificate instead
	// is safer where practical.
	Insecure bool `json:"insecure"`
}

// DefaultCredentialName is the credential file name a connection's
// token secret gets when saved through Ballast.
const DefaultCredentialName = "ballast-proxmox-token"

// Normalize validates c and rewrites Host and TokenID into their
// canonical form. It does not touch the credential store.
func (c *Connection) Normalize() error {
	h := strings.TrimSpace(c.Host)
	if h == "" {
		return fmt.Errorf("the Proxmox host address is required, e.g. https://192.168.1.10:8006")
	}
	if !strings.Contains(h, "://") {
		h = "https://" + h
	}
	u, err := url.Parse(h)
	if err != nil {
		return fmt.Errorf("%q is not a valid address: %w", c.Host, err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("the Proxmox API is reached over HTTPS; got %q", c.Host)
	}
	if u.User != nil {
		return fmt.Errorf("don't put a user in the address; Ballast connects with an API token")
	}
	if u.Hostname() == "" {
		return fmt.Errorf("the address %q has no host name", h)
	}
	if p := u.Path; p != "" && p != "/" {
		return fmt.Errorf("the address %q should be just the host and port, with nothing after it", h)
	}
	port := u.Port()
	if port == "" {
		port = DefaultPort
	}
	c.Host = "https://" + net.JoinHostPort(u.Hostname(), port)

	tok := strings.TrimSpace(c.TokenID)
	if tok == "" {
		return fmt.Errorf("the API token ID is required, e.g. ballast@pve!backup")
	}
	if !tokenIDPattern.MatchString(tok) {
		return fmt.Errorf("%q doesn't look like a Proxmox API token ID; it should look like user@realm!tokenname", tok)
	}
	c.TokenID = tok
	return nil
}

// Credentials is what a user pastes into the Proxmox connection form.
// It is write-only: it only ever reaches the credential file
// RenderCredentials produces, never the database or an API response.
type Credentials struct {
	// TokenSecret is the API token's secret half (a UUID Proxmox
	// generates when the token is created; shown once, there).
	TokenSecret string `json:"token_secret,omitempty"`
}

// Empty reports whether no credential was given.
func (c Credentials) Empty() bool { return strings.TrimSpace(c.TokenSecret) == "" }

// ErrNoCredentials is returned by RenderCredentials when c is empty.
var ErrNoCredentials = errors.New("no credentials given")

// RenderCredentials validates c and returns the credential file's
// contents plus the secret value the caller must register for
// redaction before it is written anywhere.
func RenderCredentials(c Credentials) ([]byte, []string, error) {
	secretVal := strings.TrimSpace(c.TokenSecret)
	if secretVal == "" {
		return nil, nil, ErrNoCredentials
	}
	if strings.ContainsAny(secretVal, "\n\r") {
		return nil, nil, fmt.Errorf("the API token secret must be a single line")
	}
	return []byte(secretVal + "\n"), []string{secretVal}, nil
}

// Guest is one VM or LXC container on the Proxmox host.
type Guest struct {
	Node string `json:"node"`
	VMID int    `json:"vmid"`
	// Type is "qemu" (a VM) or "lxc" (a container).
	Type   string `json:"type"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

// IsLXC reports whether g is a bare-metal-style LXC container, as
// opposed to a full VM. A service attached to an LXC guest normally
// uses the pct-exec dump transport (spec §4.2); a service attached to
// a VM running Docker instead uses that VM's DockerHost (spec §17.2).
func (g Guest) IsLXC() bool { return g.Type == "lxc" }

// TestResult is a successful connection test's findings.
type TestResult struct {
	// Version is the Proxmox VE API version string.
	Version string `json:"version"`
}

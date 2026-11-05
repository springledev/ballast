// Package proxmox is Ballast's Proxmox VE layer: it backs up guests
// with vzdump, snapshots the host's configuration, runs verification
// drills and checks storage room (spec §17.3). The shared half —
// connecting with an API token, listing guests and resolving the
// token secret — lives in github.com/springledev/keel/proxmox; this
// file adapts it to Ballast's names and user-facing messages.
package proxmox

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	kp "github.com/springledev/keel/proxmox"
	"github.com/springledev/keel/secret"
)

// Product is the name Ballast passes to keel so credential file names
// stay "ballast-…".
const Product = "ballast"

// DefaultPort is the Proxmox API's default HTTPS port.
const DefaultPort = kp.DefaultPort

// RequestTimeout bounds every single Proxmox API call.
const RequestTimeout = kp.RequestTimeout

// Connection is Ballast's one configured Proxmox host (spec §17). It
// holds no secret: the API token secret lives in a root-only
// credential file.
type Connection = kp.Connection

// Credentials is what a user pastes into the Proxmox connection form.
// It only ever reaches the credential file, never the DB or the API.
type Credentials = kp.Credentials

// Guest is one VM or LXC container on the Proxmox host.
type Guest = kp.Guest

// TestResult is what a successful connection test reports.
type TestResult = kp.TestResult

// ErrNoCredentials is returned by RenderCredentials when no token
// secret was given.
var ErrNoCredentials = kp.ErrNoCredentials

// DefaultCredentialName is the credential file name the Proxmox API
// token secret gets when saved through Ballast
// ("ballast-proxmox-token").
var DefaultCredentialName = kp.DefaultCredentialName(Product)

// RenderCredentials renders c into the credential file's contents and
// returns the secret values to redact.
func RenderCredentials(c Credentials) ([]byte, []string, error) {
	return kp.RenderCredentials(c)
}

// Messages keel words product-neutrally that Ballast has always shown
// with Ballast-specific advice.
const (
	keelUserInAddress = "don't put a user in the address; this tool connects with an API token"
	ballastUserInAddr = "don't put a user in the address; Ballast connects with an API token"
	keelTokenRequired = "the API token ID is required, e.g. user@pve!tokenname"
	ballastTokenReq   = "the API token ID is required, e.g. ballast@pve!backup"
	keelRejectedFmt   = "Proxmox rejected the API token (HTTP %d) — check the token ID and secret, and that the token has the privileges this operation needs"
	ballastRejected   = "Proxmox rejected the API token (HTTP %d) — check the token ID and secret, and that the token has the VM.Backup permission on /vms"
)

// NormalizeConnection validates c and rewrites Host and TokenID into
// their canonical form (keel's Connection.Normalize), keeping Ballast's
// wording for the messages keel phrases generically.
func NormalizeConnection(c *Connection) error {
	err := c.Normalize()
	if err == nil {
		return nil
	}
	switch err.Error() {
	case keelUserInAddress:
		return fmt.Errorf("%s", ballastUserInAddr)
	case keelTokenRequired:
		return fmt.Errorf("%s", ballastTokenReq)
	}
	return err
}

// ballastError restores Ballast's VM.Backup hint on keel's
// rejected-token error; every other error passes through unchanged.
func ballastError(err error) error {
	if err == nil {
		return nil
	}
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		if err.Error() == fmt.Sprintf(keelRejectedFmt, code) {
			return fmt.Errorf(ballastRejected, code)
		}
	}
	return err
}

// Client is an authenticated handle on one Proxmox host: keel's client
// plus Ballast's backup, snapshot, drill and storage endpoints. Build
// one through Resolver.Client.
type Client struct {
	*kp.Client
}

// do performs one API call through keel's Client.Do, with Ballast's
// wording on a rejected token.
func (cl *Client) do(ctx context.Context, method, path string, form url.Values, out any) error {
	return ballastError(cl.Client.Do(ctx, method, path, form, out))
}

// Test connects to the host and asks for its API version, proving the
// address, TLS trust and token all work.
func (cl *Client) Test(ctx context.Context) (TestResult, error) {
	res, err := cl.Client.Test(ctx)
	return res, ballastError(err)
}

// ListGuests returns every VM and LXC container across every node on
// the host, sorted by node then VMID.
func (cl *Client) ListGuests(ctx context.Context) ([]Guest, error) {
	gs, err := cl.Client.ListGuests(ctx)
	return gs, ballastError(err)
}

// Resolver turns a configured Connection into a ready-to-use Client,
// resolving the token secret against the credential directories. The
// API and the CLI both resolve the connection through one Resolver.
type Resolver struct {
	// CredDirs are searched in order for the token-secret credential
	// file.
	CredDirs []string
	// Redact receives the token secret, so it can never reach a log
	// or error.
	Redact *secret.Set
	// HTTPClient overrides the HTTP client (tests only); nil builds
	// one honouring Connection.Insecure.
	HTTPClient *http.Client
}

// Client resolves c into a ready-to-use API handle.
func (r Resolver) Client(c Connection) (*Client, error) {
	kc, err := kp.Resolver{CredDirs: r.CredDirs, Redact: r.Redact, HTTPClient: r.HTTPClient}.Client(c)
	if err != nil {
		return nil, err
	}
	return &Client{Client: kc}, nil
}

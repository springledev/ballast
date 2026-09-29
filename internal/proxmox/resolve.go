package proxmox

import (
	"fmt"
	"net/http"

	"github.com/springledev/keel/runner"
	"github.com/springledev/keel/secret"
)

// Resolver turns a configured Connection into a ready-to-use Client,
// resolving the token secret against the credential directories. The
// API and the CLI both resolve the connection through one Resolver so
// it behaves the same everywhere.
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
	if c.Host == "" {
		return nil, fmt.Errorf("no Proxmox connection is configured")
	}
	if c.Credential == "" {
		return nil, fmt.Errorf("Proxmox connection %s: no API token is configured", c.Host)
	}
	dir := runner.FindCredentialDir(c.Credential, r.CredDirs...)
	tokenSecret, err := runner.LoadCredentialValue(dir, c.Credential)
	if err != nil {
		return nil, fmt.Errorf("Proxmox API token: %w", err)
	}
	r.Redact.Add(tokenSecret)

	hc := r.HTTPClient
	if hc == nil {
		hc = newInsecureCapableClient(c.Insecure)
	}
	return &Client{Host: c.Host, TokenID: c.TokenID, TokenSecret: tokenSecret, HTTPClient: hc}, nil
}

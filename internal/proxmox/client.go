package proxmox

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// RequestTimeout bounds every API call: long enough for a slow
// homelab link, short enough that an unreachable host fails promptly.
const RequestTimeout = 30 * time.Second

// Client is a ready-to-use Proxmox API handle for one connection. Build
// one through Resolver.Client, which resolves the token secret from
// the credential store; the zero value is only useful in tests that
// set every field directly.
type Client struct {
	Host        string
	TokenID     string
	TokenSecret string
	HTTPClient  *http.Client
}

// newInsecureCapableClient builds the http.Client a Resolver hands to
// a Client, honouring Connection.Insecure.
func newInsecureCapableClient(insecure bool) *http.Client {
	c := &http.Client{Timeout: RequestTimeout}
	if insecure {
		c.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // explicit, documented opt-in (Connection.Insecure)
	}
	return c
}

// apiEnvelope is the {"data": ...} wrapper every Proxmox API response
// carries.
type apiEnvelope struct {
	Data json.RawMessage `json:"data"`
}

func (cl *Client) do(ctx context.Context, method, path string, form url.Values, out any) error {
	ctx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	u := strings.TrimRight(cl.Host, "/") + path
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return fmt.Errorf("building Proxmox request: %w", err)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("Authorization", "PVEAPIToken="+cl.TokenID+"="+cl.TokenSecret)

	hc := cl.HTTPClient
	if hc == nil {
		hc = newInsecureCapableClient(false)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return wrapNetErr(cl.Host, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	switch resp.StatusCode {
	case http.StatusOK:
		// fall through
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("Proxmox rejected the API token (HTTP %d) — check the token ID and secret, and that the token has the VM.Backup permission on /vms", resp.StatusCode)
	default:
		return fmt.Errorf("Proxmox API request to %s failed (HTTP %d): %s", path, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	if out == nil {
		return nil
	}
	var env apiEnvelope
	if err := json.Unmarshal(respBody, &env); err != nil {
		return fmt.Errorf("decoding Proxmox's response from %s: %w", path, err)
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("decoding Proxmox's response from %s: %w", path, err)
	}
	return nil
}

// Test connects to the host and asks for its API version: it proves
// the address, TLS trust and the token all work, not just that the
// machine answers.
func (cl *Client) Test(ctx context.Context) (TestResult, error) {
	var v struct {
		Version string `json:"version"`
	}
	if err := cl.do(ctx, http.MethodGet, "/api2/json/version", nil, &v); err != nil {
		return TestResult{}, err
	}
	if v.Version == "" {
		return TestResult{}, fmt.Errorf("connected to %s, but Proxmox did not report a version", cl.Host)
	}
	return TestResult{Version: v.Version}, nil
}

// ListGuests returns every VM and LXC container across every node on
// the host, sorted by node then VMID.
func (cl *Client) ListGuests(ctx context.Context) ([]Guest, error) {
	var items []struct {
		Node   string `json:"node"`
		VMID   int    `json:"vmid"`
		Name   string `json:"name"`
		Status string `json:"status"`
		Type   string `json:"type"`
	}
	if err := cl.do(ctx, http.MethodGet, "/api2/json/cluster/resources?type=vm", nil, &items); err != nil {
		return nil, err
	}
	out := make([]Guest, 0, len(items))
	for _, it := range items {
		if it.Type != "qemu" && it.Type != "lxc" {
			continue
		}
		out = append(out, Guest{Node: it.Node, VMID: it.VMID, Type: it.Type, Name: it.Name, Status: it.Status})
	}
	return out, nil
}

// wrapNetErr adds a plain-English hint to a transport-level failure
// (DNS, TCP, TLS) reaching host.
func wrapNetErr(host string, err error) error {
	s := err.Error()
	switch {
	case strings.Contains(s, "certificate signed by unknown authority") || strings.Contains(s, "x509"):
		return fmt.Errorf("could not verify %s's TLS certificate (Proxmox uses a self-signed certificate by default) — double check the address, or enable \"skip certificate verification\" if you trust this network: %w", host, err)
	case strings.Contains(s, "no such host"):
		return fmt.Errorf("could not resolve %s — check the address: %w", host, err)
	case strings.Contains(s, "connection refused"):
		return fmt.Errorf("%s refused the connection — check the address and port (default %s): %w", host, DefaultPort, err)
	case strings.Contains(s, "context deadline exceeded") || strings.Contains(s, "i/o timeout"):
		return fmt.Errorf("no answer from %s within %s — check the address, and that a firewall isn't blocking it: %w", host, RequestTimeout, err)
	case strings.Contains(s, "no route to host"):
		return fmt.Errorf("%s could not be reached on the network: %w", host, err)
	}
	return fmt.Errorf("connecting to %s: %w", host, err)
}

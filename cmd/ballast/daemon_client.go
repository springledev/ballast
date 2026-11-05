package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/springledev/ballast/internal/config"
)

// daemonURL is the --url flag: the address of a running `ballast serve`
// (also read from $BALLAST_URL). Empty means "the daemon on this
// machine, at the configured listen address".
var daemonURL string

// errNoDaemon means no Ballast daemon answered at the address tried.
var errNoDaemon = errors.New("no Ballast daemon is running")

// daemonClient talks to a running `ballast serve` over its REST API
// (the same API the web UI uses), so CLI commands and the daemon never
// disagree about state.
type daemonClient struct {
	base  string // e.g. http://127.0.0.1:8080, no trailing slash
	token string
	http  *http.Client
}

// resolveDaemonURL works out where to look for the daemon. explicit is
// true when the user named the address (--url or $BALLAST_URL): then a
// missing daemon is an error, never a silent switch to running things
// locally against a different machine's setup.
func resolveDaemonURL(flagURL string, cfg config.Config) (base string, explicit bool) {
	u := flagURL
	if u == "" {
		u = os.Getenv("BALLAST_URL")
	}
	if u != "" {
		if !strings.Contains(u, "://") {
			u = "http://" + u
		}
		return strings.TrimRight(u, "/"), true
	}
	host, port, err := net.SplitHostPort(cfg.Listen)
	if err != nil {
		return "", false
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port), false
}

// connectDaemon returns a client for a reachable daemon, or errNoDaemon
// when nothing is listening. An address that answers but is not
// Ballast, or that rejects our token, is a real error.
func connectDaemon(ctx context.Context, cfg config.Config, flagURL string) (*daemonClient, bool, error) {
	base, explicit := resolveDaemonURL(flagURL, cfg)
	if base == "" {
		return nil, explicit, errNoDaemon
	}
	token, err := loadOptionalCredential(cfg.CredentialDirs(), apiTokenCredential)
	if err != nil {
		return nil, explicit, fmt.Errorf("reading the API token: %w", err)
	}
	c := &daemonClient{base: base, token: token, http: &http.Client{Timeout: 0}}

	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	resp, err := c.do(probeCtx, http.MethodGet, "/api/v1/status", nil)
	if err != nil {
		if isConnRefused(err) || errors.Is(err, context.DeadlineExceeded) {
			return nil, explicit, errNoDaemon
		}
		return nil, explicit, fmt.Errorf("reaching the Ballast daemon at %s: %w", base, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, explicit, fmt.Errorf("the Ballast daemon at %s rejected this machine's API token; point BALLAST_CONFIG_DIR at the daemon's config folder, or make sure the ballast-api-token file matches", base)
	}
	return c, explicit, nil
}

func isConnRefused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EHOSTUNREACH)
}

func (c *daemonClient) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", bearerPrefix+c.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.http.Do(req)
}

// bearerPrefix is the Authorization scheme the API expects.
const bearerPrefix = "Bearer "

// call sends a request and decodes a JSON response into out (nil to
// ignore it). Non-2xx answers become errors carrying the daemon's own
// plain-English message.
func (c *daemonClient) call(ctx context.Context, method, path string, body, out any) error {
	resp, err := c.do(ctx, method, path, body)
	if err != nil {
		return fmt.Errorf("talking to the Ballast daemon at %s: %w", c.base, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading the daemon's reply: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("the Ballast daemon answered %s", resp.Status)
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}

// useDaemon decides how a command that supports both modes runs.
// It returns a client when a daemon is available. With a nil client and
// nil error the caller should run the command directly ("single-shot"),
// and a hint that `ballast serve` is the recommended permanent mode has
// already been printed. Only commands that are safe to run on their own
// call this; an explicitly named daemon that is down is an error.
func useDaemon(cmd *cobra.Command, cfg config.Config) (*daemonClient, error) {
	c, explicit, err := connectDaemon(cmd.Context(), cfg, daemonURL)
	if err == nil {
		return c, nil
	}
	if !errors.Is(err, errNoDaemon) {
		return nil, err
	}
	if explicit {
		return nil, fmt.Errorf("no Ballast daemon answered at the address you gave (--url or BALLAST_URL); check it is running with `ballast serve`")
	}
	fmt.Fprintln(cmd.ErrOrStderr(), "ballast: no daemon is running, so this command is running on its own. For scheduled backups and the web page, run `ballast serve` permanently (see the docs).")
	return nil, nil
}

func init() {
	rootCmd.PersistentFlags().StringVar(&daemonURL, "url", "", "address of a running Ballast daemon to send commands to (default: the local daemon, or $BALLAST_URL)")
}

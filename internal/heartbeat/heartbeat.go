// Package heartbeat lets Ballast tell an outside "dead man's switch"
// monitor (Healthchecks, Uptime Kuma, and similar) that it is still
// alive, so something that is not Ballast notices when the daemon dies,
// hangs, or the machine loses power (spec §21.7).
//
// The monitor URL is a secret (spec rule 2): it is read from a
// root-only credential file, never stored in the database, and never
// appears in a log line, an error, doctor output or an API response.
// A failed ping is reported and otherwise ignored: it never changes a
// backup or a service's light.
package heartbeat

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// CredentialName is the credential file holding the monitor URL.
const CredentialName = "ballast-heartbeat-url"

// MetaKeyLastOK is the meta key holding when a ping last succeeded
// (RFC 3339). It stores a time only, never the URL.
const MetaKeyLastOK = "heartbeat_last_ok"

// DefaultInterval is how often the monitor is pinged.
const DefaultInterval = 5 * time.Minute

// pingTimeout bounds one request so a slow monitor cannot pile up pings.
const pingTimeout = 15 * time.Second

// MetaStore is the small slice of the state store the pinger needs.
type MetaStore interface {
	MetaSet(key, value string) error
}

// Pinger requests the monitor URL on an interval. The zero value is not
// useful: set Credential at least.
type Pinger struct {
	// Credential returns the monitor URL, or "" when none is
	// configured. It is called for every ping so a replaced file takes
	// effect without a restart.
	Credential func() (string, error)
	// Client is the HTTP client; nil means a default client.
	Client *http.Client
	// Interval is the time between pings; zero means DefaultInterval.
	Interval time.Duration
	// Meta records the last success time; may be nil.
	Meta MetaStore
	// Log receives plain-English problem reports; may be nil.
	Log func(format string, args ...any)

	mu   sync.Mutex
	last time.Time
}

// Beat pings if at least Interval has passed since the last attempt.
// The scheduler loop calls it on every tick, so pings stop the moment
// the loop does. It is a no-op when no URL is configured. Failures are
// logged and never returned.
func (p *Pinger) Beat(ctx context.Context, now time.Time) {
	interval := p.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	p.mu.Lock()
	if !p.last.IsZero() && now.Sub(p.last) < interval {
		p.mu.Unlock()
		return
	}
	p.last = now
	p.mu.Unlock()

	if err := p.Ping(ctx, now); err != nil && p.Log != nil {
		p.Log("heartbeat: %v", err)
	}
}

// Ping requests the monitor URL once and, on success, records now as
// the last success. It returns nil without doing anything when no URL
// is configured. Returned errors never contain the URL.
func (p *Pinger) Ping(ctx context.Context, now time.Time) error {
	raw, err := p.Credential()
	if err != nil {
		return fmt.Errorf("the heartbeat address could not be read: %w", err)
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return errors.New("the heartbeat address is not a valid web address")
	}
	client := p.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("the monitor could not be reached: %s", scrub(err, raw))
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("the monitor answered with HTTP status %d", resp.StatusCode)
	}
	if p.Meta != nil {
		if err := p.Meta.MetaSet(MetaKeyLastOK, now.UTC().Format(time.RFC3339)); err != nil {
			return fmt.Errorf("recording the last successful heartbeat: %w", err)
		}
	}
	return nil
}

// scrub returns err's text without the request URL: net/http embeds
// the URL in its errors, so only the inner error is used, and any
// remaining copy of the URL or its parts is removed.
func scrub(err error, raw string) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	msg := err.Error()
	parts := []string{raw}
	if u, perr := url.Parse(raw); perr == nil {
		parts = append(parts, u.String(), u.EscapedPath(), u.Path, u.RawQuery)
		if u.User != nil {
			if pw, ok := u.User.Password(); ok {
				parts = append(parts, pw)
			}
			parts = append(parts, u.User.Username())
		}
		for _, seg := range strings.Split(u.Path, "/") {
			if len(seg) >= 6 {
				parts = append(parts, seg)
			}
		}
	}
	for _, s := range parts {
		if len(s) >= 6 {
			msg = strings.ReplaceAll(msg, s, "[hidden]")
		}
	}
	return msg
}

// LastOK reads when a ping last succeeded from db. ok is false when
// none has yet.
func LastOK(db *sql.DB) (t time.Time, ok bool, err error) {
	var v string
	switch err := db.QueryRow(`SELECT value FROM meta WHERE key = ?`, MetaKeyLastOK).Scan(&v); {
	case errors.Is(err, sql.ErrNoRows):
		return time.Time{}, false, nil
	case err != nil:
		return time.Time{}, false, fmt.Errorf("reading the last heartbeat time: %w", err)
	}
	t, perr := time.Parse(time.RFC3339, v)
	if perr != nil {
		return time.Time{}, false, nil
	}
	return t, true, nil
}

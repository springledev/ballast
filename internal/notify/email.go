package notify

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// ChannelTypeWebhook and ChannelTypeEmail join ntfy as channel types.
const (
	ChannelTypeWebhook = "webhook"
	ChannelTypeEmail   = "email"
)

// Email connection security modes.
const (
	// EmailSecurityStartTLS upgrades a plain connection (usually port 587).
	EmailSecurityStartTLS = "starttls"
	// EmailSecurityTLS connects encrypted from the start (usually port 465).
	EmailSecurityTLS = "tls"
	// EmailSecurityNone sends unencrypted; only sensible on a trusted LAN relay.
	EmailSecurityNone = "none"
)

// EmailSettings are an email channel's non-secret settings. The
// password lives in the channel's token credential file.
type EmailSettings struct {
	Host     string   `json:"host"`
	Port     int      `json:"port"`
	Security string   `json:"security"`
	From     string   `json:"from"`
	To       []string `json:"to"`
}

// Validate checks the settings are complete enough to send with.
func (s EmailSettings) Validate() error {
	switch {
	case strings.TrimSpace(s.Host) == "":
		return fmt.Errorf("%w: an email channel needs a mail server (SMTP host)", ErrInvalidChannel)
	case s.Port < 1 || s.Port > 65535:
		return fmt.Errorf("%w: the mail server port must be between 1 and 65535 (587 and 465 are common)", ErrInvalidChannel)
	case s.Security != EmailSecurityStartTLS && s.Security != EmailSecurityTLS && s.Security != EmailSecurityNone:
		return fmt.Errorf("%w: email security must be starttls, tls or none", ErrInvalidChannel)
	case !strings.Contains(s.From, "@"):
		return fmt.Errorf("%w: the From address must be an email address", ErrInvalidChannel)
	case len(s.To) == 0:
		return fmt.Errorf("%w: give at least one address to send to", ErrInvalidChannel)
	}
	for _, t := range s.To {
		if !strings.Contains(t, "@") {
			return fmt.Errorf("%w: %q is not an email address", ErrInvalidChannel, t)
		}
	}
	return nil
}

// EmailChannel delivers events as plain-text email over SMTP.
type EmailChannel struct {
	EmailSettings
	Username string
	Password string // from the credential store; never logged
	// TLSConfig is injected by tests; nil uses the host's name.
	TLSConfig *tls.Config
}

// Type returns "email".
func (e *EmailChannel) Type() string { return ChannelTypeEmail }

// Address returns the mail server, for display.
func (e *EmailChannel) Address() string { return e.Host }

// Send emails the event to every recipient. The error never contains
// the password.
func (e *EmailChannel) Send(ctx context.Context, event Event) error {
	if err := e.Validate(); err != nil {
		return fmt.Errorf("%s: %w", event.Service, err)
	}
	addr := net.JoinHostPort(e.Host, strconv.Itoa(e.Port))
	d := net.Dialer{Timeout: 15 * time.Second}
	cfg := e.TLSConfig
	if cfg == nil {
		cfg = &tls.Config{ServerName: e.Host, MinVersion: tls.VersionTLS12}
	}
	var conn net.Conn
	var err error
	if e.Security == EmailSecurityTLS {
		conn, err = (&tls.Dialer{NetDialer: &d, Config: cfg}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("%s: connecting to mail server %s: %w", event.Service, addr, err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	}
	c, err := smtp.NewClient(conn, e.Host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("%s: talking to mail server %s: %w", event.Service, addr, err)
	}
	defer c.Close()
	if e.Security == EmailSecurityStartTLS {
		if err := c.StartTLS(cfg); err != nil {
			return fmt.Errorf("%s: mail server %s refused STARTTLS: %w", event.Service, addr, err)
		}
	}
	if e.Password != "" {
		user := e.Username
		if err := c.Auth(plainAuthAnyConn{user: user, pass: e.Password}); err != nil {
			return fmt.Errorf("%s: mail server %s rejected the login for %q: %w", event.Service, addr, user, err)
		}
	}
	if err := c.Mail(e.From); err != nil {
		return fmt.Errorf("%s: mail server rejected the From address: %w", event.Service, err)
	}
	for _, t := range e.To {
		if err := c.Rcpt(t); err != nil {
			return fmt.Errorf("%s: mail server rejected recipient %s: %w", event.Service, t, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("%s: sending email: %w", event.Service, err)
	}
	if _, err := w.Write([]byte(e.message(event))); err != nil {
		return fmt.Errorf("%s: sending email: %w", event.Service, err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("%s: mail server did not accept the message: %w", event.Service, err)
	}
	return c.Quit()
}

func (e *EmailChannel) message(ev Event) string {
	subject := sanitizeHeader(fmt.Sprintf("[Ballast] %s", ev.Message))
	body := fmt.Sprintf("%s\r\n\r\nService: %s\r\nBackup copy: %s\r\nWhen: %s\r\n",
		ev.Message, ev.Service, ev.TierName(), ev.Timestamp.UTC().Format(time.RFC1123))
	if ev.Detail != "" {
		body += "\r\n" + strings.ReplaceAll(ev.Detail, "\n", "\r\n") + "\r\n"
	}
	return "From: " + sanitizeHeader(e.From) + "\r\nTo: " + sanitizeHeader(strings.Join(e.To, ", ")) +
		"\r\nSubject: " + subject + "\r\nDate: " + time.Now().UTC().Format(time.RFC1123Z) +
		"\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + body
}

func sanitizeHeader(s string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
}

// plainAuthAnyConn is PLAIN auth without net/smtp's "no password over
// an unencrypted connection" guard: the admin chose "none" security
// knowingly, and the unencrypted case is surfaced in the UI.
type plainAuthAnyConn struct{ user, pass string }

func (a plainAuthAnyConn) Start(*smtp.ServerInfo) (string, []byte, error) {
	return "PLAIN", []byte("\x00" + a.user + "\x00" + a.pass), nil
}

func (a plainAuthAnyConn) Next(_ []byte, more bool) ([]byte, error) {
	if more {
		return nil, fmt.Errorf("unexpected server challenge")
	}
	return nil, nil
}

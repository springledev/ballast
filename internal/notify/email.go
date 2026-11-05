package notify

import (
	"context"
	"crypto/tls"

	kn "github.com/springledev/keel/notify"
)

// ChannelTypeWebhook and ChannelTypeEmail join ntfy as channel types.
const (
	ChannelTypeWebhook = kn.ChannelTypeWebhook
	ChannelTypeEmail   = kn.ChannelTypeEmail
)

// Email connection security modes.
const (
	// EmailSecurityStartTLS upgrades a plain connection (usually port 587).
	EmailSecurityStartTLS = kn.EmailSecurityStartTLS
	// EmailSecurityTLS connects encrypted from the start (usually port 465).
	EmailSecurityTLS = kn.EmailSecurityTLS
	// EmailSecurityNone sends unencrypted; only sensible on a trusted LAN relay.
	EmailSecurityNone = kn.EmailSecurityNone
)

// EmailSettings are an email channel's non-secret settings (mail
// server, security mode, From and To). They are stored in the
// database; the SMTP password never is.
type EmailSettings = kn.EmailSettings

// emailProduct is the subject prefix of every Ballast email
// ("[Ballast] ...").
const emailProduct = "Ballast"

// EmailChannel delivers events as plain-text email over SMTP, through
// keel's EmailChannel.
type EmailChannel struct {
	EmailSettings
	Username string
	Password string // from the credential store; never logged
	// TLSConfig is injected by tests; nil uses the host's name.
	TLSConfig *tls.Config
}

// Type returns "email".
func (e *EmailChannel) Type() string { return ChannelTypeEmail }

// Address is the mail server, for display (it holds no secret).
func (e *EmailChannel) Address() string { return e.Host }

// Send delivers event as one email to every recipient. The subject is
// "[Ballast] <message>", and the body names the backup copy (tier) the
// event is about, so a problem with one copy is never read as being
// about another (spec rule 3).
func (e *EmailChannel) Send(ctx context.Context, event Event) error {
	k := event.Notify()
	tier := "Backup copy: " + event.TierName()
	if k.Detail != "" {
		k.Detail = tier + "\n" + k.Detail
	} else {
		k.Detail = tier
	}
	ch := &kn.EmailChannel{
		EmailSettings: e.EmailSettings,
		Username:      e.Username,
		Password:      e.Password,
		Product:       emailProduct,
		TLSConfig:     e.TLSConfig,
	}
	return ch.Send(ctx, k)
}

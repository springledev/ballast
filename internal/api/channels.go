package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/notify"
	"github.com/springledev/keel/secret"
)

// ChannelView is a notification channel as shown to the UI and CLI.
// It never carries the address or token themselves (spec rule 2): the
// address is shortened to its host, and the token is only reported as
// set or not.
type ChannelView struct {
	Name       string   `json:"name"`
	Type       string   `json:"type"`
	URLDisplay string   `json:"url_display"`
	Username   string   `json:"username,omitempty"`
	HasToken   bool     `json:"has_token"`
	ApplyToAll bool     `json:"apply_to_all"`
	Services   []string `json:"services"`
	// Email is an email channel's mail server settings (never the password).
	Email *notify.EmailSettings `json:"email,omitempty"`
	// Events and Tiers are the routing filters; empty means everything.
	Events []string `json:"events"`
	Tiers  []string `json:"tiers"`
	// Digest is true when the channel receives the weekly digest.
	Digest bool `json:"digest"`
}

// ChannelInput is what a caller sends to create or change a channel.
// An empty URL or Token on an existing channel keeps the stored one,
// so editing a channel never requires retyping its secrets; ClearToken
// removes the token.
type ChannelInput struct {
	Name       string   `json:"name"`
	Type       string   `json:"type"`
	URL        string   `json:"url"`
	Username   string   `json:"username"`
	Token      string   `json:"token"`
	ClearToken bool     `json:"clear_token"`
	ApplyToAll bool     `json:"apply_to_all"`
	Services   []string `json:"services"`
	// Email holds the mail server settings for an email channel; the
	// password goes in Token.
	Email *notify.EmailSettings `json:"email"`
	// Events and Tiers limit what the channel delivers; empty means all.
	Events []string `json:"events"`
	Tiers  []string `json:"tiers"`
	// Digest turns the weekly digest on or off; nil keeps what is stored.
	Digest *bool `json:"digest"`
}

// channelErr maps notify's channel errors onto the API's error kinds.
func channelErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, notify.ErrInvalidChannel):
		return fmt.Errorf("%w: %w", ErrValidation, err)
	case errors.Is(err, notify.ErrChannelNotFound):
		return fmt.Errorf("%w: %w", inventory.ErrNotFound, err)
	}
	return err
}

// secretSet returns the shared redaction set, or a throwaway one when
// the Ops was built without it.
func (o *Ops) secretSet() *secret.Set {
	if o.Redact != nil {
		return o.Redact
	}
	return secret.NewSet()
}

func (o *Ops) credDirs() []string {
	if len(o.CredentialSearchDirs) > 0 {
		return o.CredentialSearchDirs
	}
	return []string{o.CredentialsDir}
}

func (o *Ops) channelView(c notify.ChannelConfig) ChannelView {
	v := ChannelView{Name: c.Name, Type: c.Type, Username: c.Username, HasToken: c.TokenCredential != "",
		ApplyToAll: c.ApplyToAll, Services: c.Services, Email: c.Email, Events: c.Events, Tiers: c.Tiers, Digest: c.Digest}
	if v.Services == nil {
		v.Services = []string{}
	}
	if v.Events == nil {
		v.Events = []string{}
	}
	if v.Tiers == nil {
		v.Tiers = []string{}
	}
	if ch, err := c.Build(o.credDirs(), o.secretSet()); err == nil {
		v.URLDisplay = notify.ChannelAddress(ch)
	}
	return v
}

// ListChannels lists every notification channel, first migrating any
// pre-v0.3 ntfy credential file into a channel so it shows up here.
func (o *Ops) ListChannels(ctx context.Context) ([]ChannelView, error) {
	svcs, err := inventory.ListServices(o.DB)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(svcs))
	for _, s := range svcs {
		names = append(names, s.Name)
	}
	if err := notify.MigrateLegacyChannels(o.DB, o.credDirs(), names); err != nil {
		return nil, err
	}
	cs, err := notify.ListChannels(o.DB)
	if err != nil {
		return nil, err
	}
	out := make([]ChannelView, 0, len(cs))
	for _, c := range cs {
		out = append(out, o.channelView(c))
	}
	return out, nil
}

// PutChannel creates or updates a channel, saving its address and
// token as root-only credential files (never the database).
func (o *Ops) PutChannel(ctx context.Context, in ChannelInput) (ChannelView, error) {
	if err := notify.ValidateChannelName(in.Name); err != nil {
		return ChannelView{}, channelErr(err)
	}
	for _, s := range in.Services {
		if _, err := inventory.GetService(o.DB, s); err != nil {
			return ChannelView{}, wrapf("service "+s, err)
		}
	}
	existing, err := notify.GetChannel(o.DB, in.Name)
	if err != nil && !errors.Is(err, notify.ErrChannelNotFound) {
		return ChannelView{}, err
	}
	if in.Type == "" {
		in.Type = notify.ChannelTypeNtfy
		if existing.ID != 0 {
			in.Type = existing.Type
		}
	}
	if existing.ID != 0 && existing.Type != in.Type {
		return ChannelView{}, channelErr(fmt.Errorf("channel %s: %w: a %s channel cannot be changed into a %s one; remove it and add a new one", in.Name, notify.ErrInvalidChannel, existing.Type, in.Type))
	}
	cfg := notify.ChannelConfig{Name: in.Name, Type: in.Type, Username: in.Username,
		ApplyToAll: in.ApplyToAll, Services: in.Services, Email: in.Email, Events: in.Events, Tiers: in.Tiers,
		URLCredential: existing.URLCredential, TokenCredential: existing.TokenCredential}
	// A missing (null) filter keeps what is stored; an empty list clears it.
	if in.Events == nil {
		cfg.Events = existing.Events
	}
	if in.Tiers == nil {
		cfg.Tiers = existing.Tiers
	}
	cfg.Digest = existing.Digest
	if in.Digest != nil {
		cfg.Digest = *in.Digest
	}
	if in.Type == notify.ChannelTypeEmail && in.Email == nil {
		cfg.Email = existing.Email // editing keeps the stored server settings
	}
	if in.Type == notify.ChannelTypeEmail && in.URL != "" {
		return ChannelView{}, channelErr(fmt.Errorf("channel %s: %w: an email channel has no web address", in.Name, notify.ErrInvalidChannel))
	}
	if in.URL != "" {
		if err := notify.ValidateChannelURL(in.URL); err != nil {
			return ChannelView{}, channelErr(err)
		}
		cfg.URLCredential = notify.ChannelURLCredential(in.Name)
		if err := o.writeChannelSecret(cfg.URLCredential, in.URL); err != nil {
			return ChannelView{}, err
		}
	}
	if cfg.URLCredential == "" && in.Type != notify.ChannelTypeEmail {
		return ChannelView{}, channelErr(fmt.Errorf("channel %s: %w: an address is required", in.Name, notify.ErrInvalidChannel))
	}
	switch {
	case in.Token != "":
		cfg.TokenCredential = notify.ChannelTokenCredential(in.Name)
		if err := o.writeChannelSecret(cfg.TokenCredential, in.Token); err != nil {
			return ChannelView{}, err
		}
	case in.ClearToken:
		o.removeChannelSecret(cfg.TokenCredential)
		cfg.TokenCredential = ""
	}
	saved, err := notify.PutChannel(o.DB, cfg)
	if err != nil {
		return ChannelView{}, channelErr(err)
	}
	return o.channelView(saved), nil
}

// DeleteChannel removes a channel and the credential files Ballast
// wrote for it. Files it did not write (an operator's own, or a
// migrated legacy file) are left alone.
func (o *Ops) DeleteChannel(ctx context.Context, name string) error {
	c, err := notify.DeleteChannel(o.DB, name)
	if err != nil {
		return channelErr(err)
	}
	for _, cred := range []string{c.URLCredential, c.TokenCredential} {
		if cred == notify.ChannelURLCredential(name) || cred == notify.ChannelTokenCredential(name) {
			o.removeChannelSecret(cred)
		}
	}
	return nil
}

// TestChannel sends the "Ballast test notification" through the named
// channel, using exactly the code that delivers real alerts.
func (o *Ops) TestChannel(ctx context.Context, name string) error {
	_, err := o.TestChannelResult(ctx, name)
	return err
}

// TestChannelResult is TestChannel plus a short plain-English note on
// what happened (for a webhook, the HTTP status code it answered with).
func (o *Ops) TestChannelResult(ctx context.Context, name string) (string, error) {
	c, err := notify.GetChannel(o.DB, name)
	if err != nil {
		return "", channelErr(err)
	}
	ch, err := c.Build(o.credDirs(), o.secretSet())
	if err != nil {
		return "", err
	}
	if err := notify.SendTest(ctx, ch); err != nil {
		return "", fmt.Errorf("channel %s: the test message could not be delivered: %w", name, err)
	}
	if st := notify.LastStatus(ch); st != 0 {
		return fmt.Sprintf("The webhook answered with HTTP status %d.", st), nil
	}
	return "Test message delivered.", nil
}

func (o *Ops) writeChannelSecret(cred, value string) error {
	if o.CredentialsDir == "" {
		return fmt.Errorf("%w: no writable credential directory is configured", ErrValidation)
	}
	// An operator-provided file of the same name would win over what
	// is written here; refuse loudly instead of silently ignoring it.
	for _, dir := range o.CredentialSearchDirs {
		if dir == o.CredentialsDir {
			continue
		}
		if _, err := os.Lstat(filepath.Join(dir, cred)); err == nil {
			return fmt.Errorf("%w: a credential file named %q already exists in %s and takes precedence; edit that file directly instead", ErrValidation, cred, dir)
		}
	}
	if err := os.MkdirAll(o.CredentialsDir, 0o700); err != nil {
		return fmt.Errorf("creating credential directory %s: %w", o.CredentialsDir, err)
	}
	path := filepath.Join(o.CredentialsDir, cred)
	if err := os.WriteFile(path, []byte(strings.TrimSpace(value)+"\n"), 0o600); err != nil {
		return fmt.Errorf("saving %s: %w", cred, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("restricting %s: %w", cred, err)
	}
	o.secretSet().Add(value)
	return nil
}

func (o *Ops) removeChannelSecret(cred string) {
	if cred != "" && o.CredentialsDir != "" {
		_ = os.Remove(filepath.Join(o.CredentialsDir, cred))
	}
}

// --- HTTP ----------------------------------------------------------------

func (h *handler) listChannels(w http.ResponseWriter, r *http.Request) {
	cs, err := h.ops.ListChannels(r.Context())
	if err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, cs)
}

func (h *handler) putChannel(w http.ResponseWriter, r *http.Request) {
	var in ChannelInput
	if err := decodeJSON(r, &in); err != nil {
		h.writeErr(w, err, http.StatusBadRequest)
		return
	}
	if name := r.PathValue("name"); name != "" {
		in.Name = name
	}
	v, err := h.ops.PutChannel(r.Context(), in)
	if err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (h *handler) deleteChannel(w http.ResponseWriter, r *http.Request) {
	if err := h.ops.DeleteChannel(r.Context(), pathName(r)); err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) testChannel(w http.ResponseWriter, r *http.Request) {
	detail, err := h.ops.TestChannelResult(r.Context(), pathName(r))
	if err != nil {
		h.writeErr(w, err, http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "detail": detail})
}

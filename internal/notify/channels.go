package notify

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/springledev/keel/runner"
	"github.com/springledev/keel/secret"
)

// ChannelTypeNtfy is the ntfy channel type; ChannelTypeWebhook and
// ChannelTypeEmail (email.go) are the others.
const ChannelTypeNtfy = "ntfy"

// DefaultNtfyURL is the starting value of an ntfy channel's address.
// Self-hosted servers are equally first-class: any full URL works.
const DefaultNtfyURL = "https://ntfy.sh/"

// ErrInvalidChannel marks a channel definition refused because of what
// the caller asked for (not a database failure).
var ErrInvalidChannel = errors.New("invalid notification channel")

// ErrChannelNotFound is returned when no channel has the given name.
var ErrChannelNotFound = errors.New("notification channel not found")

var channelNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// ChannelURLCredential names the credential file holding a channel's
// address. It is derived from the channel name, so the secret itself
// never needs to appear in the database.
func ChannelURLCredential(name string) string { return "ballast-channel." + name + ".url" }

// ChannelTokenCredential names the credential file holding a channel's
// optional access token (or password, when it has a username).
func ChannelTokenCredential(name string) string { return "ballast-channel." + name + ".token" }

// ChannelConfig is a reusable notification channel as stored. It holds
// only the names of the credential files with its secrets (spec rule
// 2), never the secrets themselves.
type ChannelConfig struct {
	ID              int64
	Name            string
	Type            string
	URLCredential   string
	Username        string
	TokenCredential string
	ApplyToAll      bool
	Services        []string
	// Email holds an email channel's non-secret settings (nil for other types).
	Email *EmailSettings
	// Events and Tiers limit what the channel delivers; empty means all.
	Events []string
	Tiers  []string
}

// ValidateChannelName checks name is safe to use in a file name and
// easy to type.
func ValidateChannelName(name string) error {
	if !channelNameRE.MatchString(name) {
		return fmt.Errorf("%w: the name %q must be 1-40 lowercase letters, digits or dashes, starting with a letter or digit", ErrInvalidChannel, name)
	}
	return nil
}

// ValidateChannelURL checks raw is a full http(s) web address.
func ValidateChannelURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%w: the address must be a full web address starting with https:// (for example https://ntfy.sh/my-secret-topic)", ErrInvalidChannel)
	}
	return nil
}

// DisplayURL shortens a channel address to its scheme and host, so the
// UI and CLI can show where it points without revealing the topic
// (which acts as a password on ntfy).
func DisplayURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host + "/…"
}

// PutChannel creates or replaces the channel named c.Name (matched by
// name, keeping its ID) together with the services linked to it.
func PutChannel(db *sql.DB, c ChannelConfig) (ChannelConfig, error) {
	if err := ValidateChannelName(c.Name); err != nil {
		return ChannelConfig{}, err
	}
	settings := "{}"
	switch c.Type {
	case ChannelTypeNtfy, ChannelTypeWebhook:
		if c.URLCredential == "" {
			return ChannelConfig{}, fmt.Errorf("channel %s: %w: an address is required", c.Name, ErrInvalidChannel)
		}
	case ChannelTypeEmail:
		if c.Email == nil {
			return ChannelConfig{}, fmt.Errorf("channel %s: %w: mail server settings are required", c.Name, ErrInvalidChannel)
		}
		if err := c.Email.Validate(); err != nil {
			return ChannelConfig{}, fmt.Errorf("channel %s: %w", c.Name, err)
		}
		b, err := json.Marshal(c.Email)
		if err != nil {
			return ChannelConfig{}, fmt.Errorf("channel %s: saving: %w", c.Name, err)
		}
		settings = string(b)
	default:
		return ChannelConfig{}, fmt.Errorf("channel %s: %w: type %q is not supported (use ntfy, webhook or email)", c.Name, ErrInvalidChannel, c.Type)
	}
	var err error
	if c.Events, c.Tiers, err = ValidateRouting(c.Events, c.Tiers); err != nil {
		return ChannelConfig{}, fmt.Errorf("channel %s: %w", c.Name, err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := db.Begin()
	if err != nil {
		return ChannelConfig{}, fmt.Errorf("channel %s: saving: %w", c.Name, err)
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.Exec(`
		INSERT INTO notification_channels (name, type, url_credential, username, token_credential, apply_to_all, settings_json, events, tiers, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET type = excluded.type, url_credential = excluded.url_credential,
			username = excluded.username, token_credential = excluded.token_credential,
			apply_to_all = excluded.apply_to_all, settings_json = excluded.settings_json,
				events = excluded.events, tiers = excluded.tiers, updated_at = excluded.updated_at`,
		c.Name, c.Type, c.URLCredential, c.Username, c.TokenCredential, boolToInt(c.ApplyToAll),
		settings, strings.Join(c.Events, ","), strings.Join(c.Tiers, ","), now, now,
	); err != nil {
		return ChannelConfig{}, fmt.Errorf("channel %s: saving: %w", c.Name, err)
	}
	if err := tx.QueryRow(`SELECT id FROM notification_channels WHERE name = ?`, c.Name).Scan(&c.ID); err != nil {
		return ChannelConfig{}, fmt.Errorf("channel %s: saving: %w", c.Name, err)
	}
	if _, err := tx.Exec(`DELETE FROM service_channels WHERE channel_id = ?`, c.ID); err != nil {
		return ChannelConfig{}, fmt.Errorf("channel %s: saving services: %w", c.Name, err)
	}
	c.Services = uniqueStrings(c.Services)
	for _, s := range c.Services {
		if _, err := tx.Exec(`INSERT INTO service_channels (service, channel_id) VALUES (?, ?)`, s, c.ID); err != nil {
			return ChannelConfig{}, fmt.Errorf("channel %s: linking service %s: %w", c.Name, s, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return ChannelConfig{}, fmt.Errorf("channel %s: saving: %w", c.Name, err)
	}
	return c, nil
}

// GetChannel returns the channel called name, or ErrChannelNotFound.
func GetChannel(db *sql.DB, name string) (ChannelConfig, error) {
	cs, err := queryChannels(db, `WHERE name = ?`, name)
	if err != nil {
		return ChannelConfig{}, err
	}
	if len(cs) == 0 {
		return ChannelConfig{}, fmt.Errorf("channel %s: %w", name, ErrChannelNotFound)
	}
	return cs[0], nil
}

// ListChannels returns every channel, sorted by name.
func ListChannels(db *sql.DB) ([]ChannelConfig, error) {
	return queryChannels(db, ``)
}

// DeleteChannel removes the channel called name (its service links go
// with it) and returns what was removed so the caller can clean up its
// credential files. Unknown names are ErrChannelNotFound.
func DeleteChannel(db *sql.DB, name string) (ChannelConfig, error) {
	c, err := GetChannel(db, name)
	if err != nil {
		return ChannelConfig{}, err
	}
	if _, err := db.Exec(`DELETE FROM notification_channels WHERE id = ?`, c.ID); err != nil {
		return ChannelConfig{}, fmt.Errorf("channel %s: deleting: %w", name, err)
	}
	return c, nil
}

// ChannelsForService returns the channels that deliver for service:
// every apply-to-all channel plus those linked to it.
func ChannelsForService(db *sql.DB, service string) ([]ChannelConfig, error) {
	return queryChannels(db, `WHERE apply_to_all = 1 OR id IN (SELECT channel_id FROM service_channels WHERE service = ?)`, service)
}

func queryChannels(db *sql.DB, where string, args ...any) ([]ChannelConfig, error) {
	rows, err := db.Query(`SELECT id, name, type, url_credential, username, token_credential, apply_to_all, settings_json, events, tiers
		FROM notification_channels `+where+` ORDER BY name`, args...)
	if err != nil {
		return nil, fmt.Errorf("loading notification channels: %w", err)
	}
	defer rows.Close()
	var out []ChannelConfig
	for rows.Next() {
		var c ChannelConfig
		var all int
		var settings, events, tiers string
		if err := rows.Scan(&c.ID, &c.Name, &c.Type, &c.URLCredential, &c.Username, &c.TokenCredential, &all, &settings, &events, &tiers); err != nil {
			return nil, fmt.Errorf("loading notification channels: %w", err)
		}
		c.ApplyToAll = all != 0
		c.Events, c.Tiers = splitList(events), splitList(tiers)
		if c.Type == ChannelTypeEmail {
			var es EmailSettings
			if err := json.Unmarshal([]byte(settings), &es); err != nil {
				return nil, fmt.Errorf("channel %s: reading its mail settings: %w", c.Name, err)
			}
			c.Email = &es
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("loading notification channels: %w", err)
	}
	for i := range out {
		srows, err := db.Query(`SELECT service FROM service_channels WHERE channel_id = ? ORDER BY service`, out[i].ID)
		if err != nil {
			return nil, fmt.Errorf("channel %s: loading services: %w", out[i].Name, err)
		}
		for srows.Next() {
			var s string
			if err := srows.Scan(&s); err != nil {
				srows.Close()
				return nil, fmt.Errorf("channel %s: loading services: %w", out[i].Name, err)
			}
			out[i].Services = append(out[i].Services, s)
		}
		srows.Close()
	}
	return out, nil
}

// Build turns a stored channel into a ready-to-send Channel, reading
// its credential files now (so a rotated URL or token is used on the
// next send with no restart) and adding every value read to secrets
// so it is redacted from logs from then on. credDirs are searched in
// order, like every other credential file.
func (c ChannelConfig) Build(credDirs []string, secrets *secret.Set) (Channel, error) {
	read := func(name string) (string, error) {
		dir := runner.FindCredentialDir(name, credDirs...)
		if dir == "" {
			return "", fmt.Errorf("no credential directory configured")
		}
		v, err := LoadCredentialValue(dir, name)
		if err != nil {
			return "", err
		}
		secrets.Add(v)
		return v, nil
	}
	var ch Channel
	switch c.Type {
	case ChannelTypeEmail:
		if c.Email == nil {
			return nil, fmt.Errorf("channel %s: its mail settings are missing", c.Name)
		}
		e := &EmailChannel{EmailSettings: *c.Email, Username: c.Username}
		if c.TokenCredential != "" {
			p, err := read(c.TokenCredential)
			if err != nil {
				return nil, fmt.Errorf("channel %s: reading its mail password: %w", c.Name, err)
			}
			e.Password = p
		}
		ch = e
	case ChannelTypeWebhook:
		u, err := read(c.URLCredential)
		if err != nil {
			return nil, fmt.Errorf("channel %s: reading its address: %w", c.Name, err)
		}
		w := NewWebhookChannel(u)
		if c.TokenCredential != "" {
			if w.Secret, err = read(c.TokenCredential); err != nil {
				return nil, fmt.Errorf("channel %s: reading its signing secret: %w", c.Name, err)
			}
		}
		ch = w
	default:
		u, err := read(c.URLCredential)
		if err != nil {
			return nil, fmt.Errorf("channel %s: reading its address: %w", c.Name, err)
		}
		n := NewNtfyChannel(u)
		n.Username = c.Username
		if c.TokenCredential != "" {
			if n.Secret, err = read(c.TokenCredential); err != nil {
				return nil, fmt.Errorf("channel %s: reading its access token: %w", c.Name, err)
			}
		}
		ch = n
	}
	return newRouted(ch, c.Events, c.Tiers), nil
}

// ResolveChannels builds every channel that delivers for service from
// the database. One broken channel never hides the others: the ones
// that built are returned together with the joined errors of the rest.
func ResolveChannels(db *sql.DB, credDirs []string, secrets *secret.Set, service string) ([]Channel, error) {
	cs, err := ChannelsForService(db, service)
	if err != nil {
		return nil, fmt.Errorf("service %s: %w", service, err)
	}
	var chs []Channel
	var errs []error
	for _, c := range cs {
		ch, err := c.Build(credDirs, secrets)
		if err != nil {
			errs = append(errs, fmt.Errorf("service %s: %w", service, err))
			continue
		}
		chs = append(chs, ch)
	}
	return chs, errors.Join(errs...)
}

// SendTest delivers the "Ballast test notification" through ch using
// the same Send as a real event, so a typo in the address or the
// credentials shows up now instead of during tonight's backup.
func SendTest(ctx context.Context, ch Channel) error {
	return ch.Send(ctx, Event{
		Service:   "ballast",
		Type:      EventTest,
		Message:   "Ballast test notification",
		Detail:    "If you can read this, Ballast can reach you. Backups will report here.",
		Severity:  "info",
		Timestamp: time.Now().UTC(),
	})
}

// MigrateLegacyChannels turns the pre-v0.3 credential-file setup
// (ballast-ntfy-topic[.<service>]) into channels that point at those
// same files, so nothing has to be retyped and the files keep
// working. It is idempotent: a file already referenced by a channel
// is skipped. The global file becomes an apply-to-all channel named
// "ntfy"; a per-service file becomes "ntfy-<service>" linked to it.
// A missing file is simply not migrated; any other read problem is
// returned.
func MigrateLegacyChannels(db *sql.DB, credDirs []string, services []string) error {
	existing, err := ListChannels(db)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, c := range existing {
		have[c.URLCredential] = true
		have["name:"+c.Name] = true
	}
	exists := func(name string) (bool, error) {
		dir := runner.FindCredentialDir(name, credDirs...)
		if dir == "" {
			return false, nil
		}
		if _, err := LoadCredentialValue(dir, name); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return false, nil
			}
			return false, fmt.Errorf("reading %s: %w", name, err)
		}
		return true, nil
	}
	var errs []error
	add := func(cred, name, service string) {
		// Never overwrite a channel the user already made under the
		// same name.
		if have[cred] || have["name:"+name] {
			return
		}
		ok, err := exists(cred)
		if err != nil {
			errs = append(errs, err)
			return
		}
		if !ok {
			return
		}
		c := ChannelConfig{Name: name, Type: ChannelTypeNtfy, URLCredential: cred, ApplyToAll: service == ""}
		if service != "" {
			c.Services = []string{service}
		}
		if _, err := PutChannel(db, c); err != nil {
			errs = append(errs, fmt.Errorf("migrating %s: %w", cred, err))
			return
		}
		have[cred] = true
	}
	add("ballast-ntfy-topic", "ntfy", "")
	for _, svc := range uniqueStrings(services) {
		name := "ntfy-" + strings.ToLower(svc)
		if ValidateChannelName(name) != nil {
			continue
		}
		add("ballast-ntfy-topic."+svc, name, svc)
	}
	return errors.Join(errs...)
}

func uniqueStrings(ss []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range ss {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

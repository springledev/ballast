package pipeline

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"sort"

	"github.com/springledev/ballast/internal/notify"
	"github.com/springledev/keel/runner"
	"github.com/springledev/keel/secret"
)

// BuildNotifier constructs a notify.Notifier that finds each
// service's channels in root-only credential files (spec rule 2:
// never the database, never ballast.yaml), read fresh every time a
// notification is sent. A service added after Ballast started — from
// the web UI while `ballast serve` is running, say — or a credential
// file written later is therefore picked up without a restart; a
// failure for it is never silently dropped.
//
// For each service, a channel is used from the first file that
// exists, in this order:
//
//	ntfy topic:  ballast-ntfy-topic.<service>   then ballast-ntfy-topic
//	webhook URL: ballast-webhook-url.<service>  then ballast-webhook-url
//
// A missing file (of either kind) simply means no channel of that
// kind for that service, not an error. Any other read error (bad
// permissions, a directory where a file is expected, ...) is loud,
// matching how every other credential file is treated.
//
// Each file is looked up in every dir of credDirs in turn (see
// runner.FindCredentialDir): the operator's $CREDENTIALS_DIRECTORY
// first, then the folder where Ballast saves secrets it created
// itself, such as an ntfy link typed into the wizard.
//
// The channels of every service in services are loaded once up front,
// so their secrets are redacted from the very first log line. A
// service whose credential file is broken is returned as one problem
// per service for the caller to report, never a startup failure: one
// bad file must not stop every other service's backups (spec §3
// principle 3). Sending for that service keeps failing loudly until
// the file is fixed. Every loaded value — now or at send time — is
// added to secrets, the Set shared with every Runner/Coordinator/
// Pipeline, so it is redacted everywhere from the moment Ballast
// first reads it.
//
// With a non-nil db, channels come from the notification_channels
// table (reusable channels, spec §18.1); any legacy credential file
// above is first migrated into a channel that points at that same
// file, so old setups keep working and show up in the UI. A nil db
// uses the credential files directly.
func BuildNotifier(db *sql.DB, credDirs []string, services []string, secrets *secret.Set) (*notify.Notifier, []error) {
	load := func(svc string) ([]notify.Channel, error) {
		if db == nil {
			return loadChannels(credDirs, svc, secrets)
		}
		if err := notify.MigrateLegacyChannels(db, credDirs, []string{svc}); err != nil {
			return nil, err
		}
		return notify.ResolveChannels(db, credDirs, secrets, svc)
	}
	var problems []error
	for _, svc := range uniqueSorted(services) {
		if _, err := load(svc); err != nil {
			problems = append(problems, err)
		}
	}
	n := notify.NewNotifier()
	n.Resolve = load
	return n, problems
}

// loadChannels reads svc's ntfy/webhook credential files (per-service
// first, then the global fallback) and records every value in secrets.
func loadChannels(credDirs []string, svc string, secrets *secret.Set) ([]notify.Channel, error) {
	var chs []notify.Channel
	topic, err := firstCredential(credDirs, "ballast-ntfy-topic."+svc, "ballast-ntfy-topic")
	if err != nil {
		return nil, fmt.Errorf("loading ntfy channel for %s: %w", svc, err)
	}
	if topic != "" {
		secrets.Add(topic)
		chs = append(chs, notify.NewNtfyChannel(topic))
	}

	url, err := firstCredential(credDirs, "ballast-webhook-url."+svc, "ballast-webhook-url")
	if err != nil {
		return nil, fmt.Errorf("loading webhook channel for %s: %w", svc, err)
	}
	if url != "" {
		secrets.Add(url)
		chs = append(chs, notify.NewWebhookChannel(url))
	}
	return chs, nil
}

// firstCredential returns the value of the first of names that exists
// as a credential file in any of dirs, or "" when none of them exist.
// Names are tried in order, so a per-service file in any dir wins
// over a global one.
func firstCredential(dirs []string, names ...string) (string, error) {
	for _, name := range names {
		dir := runner.FindCredentialDir(name, dirs...)
		if dir == "" {
			return "", fmt.Errorf("no credential directory configured")
		}
		v, err := notify.LoadCredentialValue(dir, name)
		if err == nil {
			return v, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
	}
	return "", nil
}

// uniqueSorted returns the sorted, de-duplicated, non-empty contents
// of ss, for deterministic credential-loading order.
func uniqueSorted(ss []string) []string {
	seen := make(map[string]bool, len(ss))
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

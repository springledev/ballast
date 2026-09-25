package pipeline

import (
	"errors"
	"fmt"
	"io/fs"
	"sort"

	"github.com/springledev/ballast/internal/notify"
)

// BuildNotifier constructs a notify.Notifier wired to every service's
// configured channels, read from root-only credential files (spec
// rule 2: never the database, never ballast.yaml).
//
// For each service, a channel is registered from the first file that
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
// The loaded secret values are returned too, so the caller can add
// them to every Runner/Coordinator/Pipeline Redact list built during
// wiring: the same secret list must reach all three (spec rule 2).
func BuildNotifier(credDir string, services []string) (*notify.Notifier, []string, error) {
	n := notify.NewNotifier()
	var secrets []string

	for _, svc := range uniqueSorted(services) {
		topic, err := firstCredential(credDir, "ballast-ntfy-topic."+svc, "ballast-ntfy-topic")
		if err != nil {
			return nil, nil, fmt.Errorf("loading ntfy channel for %s: %w", svc, err)
		}
		if topic != "" {
			n.RegisterChannel(svc, notify.NewNtfyChannel(topic))
			secrets = append(secrets, topic)
		}

		url, err := firstCredential(credDir, "ballast-webhook-url."+svc, "ballast-webhook-url")
		if err != nil {
			return nil, nil, fmt.Errorf("loading webhook channel for %s: %w", svc, err)
		}
		if url != "" {
			n.RegisterChannel(svc, notify.NewWebhookChannel(url))
			secrets = append(secrets, url)
		}
	}
	return n, secrets, nil
}

// firstCredential returns the value of the first of names that exists
// as a credential file in dir, or "" when none of them exist.
func firstCredential(dir string, names ...string) (string, error) {
	for _, name := range names {
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

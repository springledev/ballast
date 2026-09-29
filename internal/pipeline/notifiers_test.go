package pipeline

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/springledev/ballast/internal/notify"
	"github.com/springledev/keel/secret"
)

func writeCred(t *testing.T, dir, name, value string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0o600); err != nil {
		t.Fatalf("writing credential %s: %v", name, err)
	}
}

// recordingServer counts POSTs per request path, standing in for both
// an ntfy server and a webhook receiver.
type recordingServer struct {
	*httptest.Server
	mu   sync.Mutex
	hits map[string]int
}

func newRecordingServer(t *testing.T) *recordingServer {
	t.Helper()
	rs := &recordingServer{hits: map[string]int{}}
	rs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rs.mu.Lock()
		rs.hits[r.URL.Path]++
		rs.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(rs.Close)
	return rs
}

func (rs *recordingServer) count(path string) int {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.hits[path]
}

func sendFailure(n *notify.Notifier, service string) error {
	return n.Send(context.Background(), notify.Event{
		Service: service, Type: notify.EventJobFailure, Message: "test", Severity: "error", Timestamp: time.Now().UTC(),
	})
}

func TestBuildNotifierPreloadsSecrets(t *testing.T) {
	tests := []struct {
		name     string
		files    map[string]string
		services []string
		want     []string
	}{
		{
			name:     "per-service beats global",
			files:    map[string]string{"ballast-ntfy-topic": "https://ntfy.sh/global", "ballast-ntfy-topic.immich": "https://ntfy.sh/immich-only"},
			services: []string{"immich"},
			want:     []string{"https://ntfy.sh/immich-only"},
		},
		{
			name:     "missing files mean no channel",
			services: []string{"immich"},
		},
		{
			name:     "global fallback shared by every service",
			files:    map[string]string{"ballast-webhook-url": "https://example.com/hook"},
			services: []string{"vaultwarden", "immich"},
			want:     []string{"https://example.com/hook"},
		},
		{
			name:     "both channel kinds",
			files:    map[string]string{"ballast-ntfy-topic.immich": "https://ntfy.sh/immich", "ballast-webhook-url.immich": "https://example.com/hook"},
			services: []string{"immich"},
			want:     []string{"https://example.com/hook", "https://ntfy.sh/immich"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, v := range tt.files {
				writeCred(t, dir, name, v)
			}
			secrets := secret.NewSet()
			if _, problems := BuildNotifier([]string{dir}, tt.services, secrets); problems != nil {
				t.Fatalf("BuildNotifier: %v", problems)
			}
			got := secrets.Values()
			sort.Strings(got)
			if len(got) != len(tt.want) {
				t.Fatalf("secrets = %v, want %v", got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("secrets[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// TestBuildNotifierServiceAddedAfterStartup: the web UI adds a service
// while `ballast serve` is running. Its failure must still reach the
// global channel (and a per-service file written later must be
// picked up), with every value it read redacted from then on.
func TestBuildNotifierServiceAddedAfterStartup(t *testing.T) {
	rs := newRecordingServer(t)
	dir := t.TempDir()
	writeCred(t, dir, "ballast-webhook-url", rs.URL+"/global")
	secrets := secret.NewSet()

	n, problems := BuildNotifier([]string{dir}, nil /* no services yet */, secrets)
	if problems != nil {
		t.Fatalf("BuildNotifier: %v", problems)
	}

	if err := sendFailure(n, "added-later"); err != nil {
		t.Fatalf("Send for a service added after startup: %v", err)
	}
	if got := rs.count("/global"); got != 1 {
		t.Errorf("global webhook hits = %d, want 1", got)
	}

	perService := rs.URL + "/added-later"
	writeCred(t, dir, "ballast-webhook-url.added-later", perService)
	if err := sendFailure(n, "added-later"); err != nil {
		t.Fatalf("Send after writing a per-service credential: %v", err)
	}
	if got := rs.count("/added-later"); got != 1 {
		t.Errorf("per-service webhook hits = %d, want 1 (written after startup)", got)
	}
	if got := secrets.Redact(perService); got != secret.Placeholder {
		t.Errorf("Redact(per-service URL) = %q, want it redacted once read", got)
	}
}

func TestBuildNotifierNoChannelIsLoud(t *testing.T) {
	n, problems := BuildNotifier([]string{t.TempDir()}, nil, secret.NewSet())
	if problems != nil {
		t.Fatalf("BuildNotifier: %v", problems)
	}
	if err := sendFailure(n, "nobody-listening"); !errors.Is(err, notify.ErrNoChannels) {
		t.Errorf("Send = %v, want ErrNoChannels", err)
	}
}

// TestBuildNotifierBrokenFileIsPerService: one service's broken
// credential file is reported for that service alone (spec §3
// principle 3) — it neither stops startup nor silences another
// service — and sending for the broken service keeps failing loudly.
func TestBuildNotifierBrokenFileIsPerService(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions")
	}
	rs := newRecordingServer(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ballast-ntfy-topic.immich"), []byte("https://ntfy.sh/x"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeCred(t, dir, "ballast-webhook-url.vault", rs.URL+"/vault")

	n, problems := BuildNotifier([]string{dir}, []string{"immich", "vault"}, secret.NewSet())
	if len(problems) != 1 || !strings.Contains(problems[0].Error(), "immich") {
		t.Fatalf("problems = %v, want exactly one, naming immich", problems)
	}
	if n == nil {
		t.Fatal("BuildNotifier returned no notifier; one broken file must not disable notifications")
	}
	if err := sendFailure(n, "vault"); err != nil {
		t.Errorf("Send for the healthy service: %v", err)
	}
	if got := rs.count("/vault"); got != 1 {
		t.Errorf("vault webhook hits = %d, want 1", got)
	}
	if err := sendFailure(n, "immich"); err == nil {
		t.Error("Send for the service with the broken file: want an error, got nil")
	}
}

// TestBuildNotifierSearchesEveryDir: under systemd the operator's
// $CREDENTIALS_DIRECTORY comes first, but a channel Ballast saved
// itself (the wizard's ntfy link) in its own folder must still be
// found, and an operator-provided file wins when both exist.
func TestBuildNotifierSearchesEveryDir(t *testing.T) {
	rs := newRecordingServer(t)
	systemd, own := t.TempDir(), t.TempDir()
	writeCred(t, own, "ballast-webhook-url.photos", rs.URL+"/own")
	writeCred(t, systemd, "ballast-webhook-url.vault", rs.URL+"/systemd")
	writeCred(t, own, "ballast-webhook-url.vault", rs.URL+"/shadowed")

	n, problems := BuildNotifier([]string{systemd, own}, []string{"photos", "vault"}, secret.NewSet())
	if problems != nil {
		t.Fatalf("BuildNotifier: %v", problems)
	}
	for _, svc := range []string{"photos", "vault"} {
		if err := sendFailure(n, svc); err != nil {
			t.Errorf("Send(%s): %v", svc, err)
		}
	}
	for path, want := range map[string]int{"/own": 1, "/systemd": 1, "/shadowed": 0} {
		if got := rs.count(path); got != want {
			t.Errorf("hits on %s = %d, want %d", path, got, want)
		}
	}
}

func TestUniqueSorted(t *testing.T) {
	got := uniqueSorted([]string{"b", "a", "", "a", "c"})
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("uniqueSorted = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("uniqueSorted[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

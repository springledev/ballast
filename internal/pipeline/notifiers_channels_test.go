package pipeline

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/springledev/ballast/internal/notify"
	"github.com/springledev/ballast/internal/store"
	"github.com/springledev/keel/secret"
)

// TestBuildNotifierDeliversThroughChannels proves a job-failure alert
// reaches an authenticated self-hosted ntfy through a stored channel
// (shared by two services), and that a legacy credential file keeps
// working after migration, with no duplicate delivery.
func TestBuildNotifierDeliversThroughChannels(t *testing.T) {
	var mu sync.Mutex
	hits := map[string][]string{} // path -> Authorization headers
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits[r.URL.Path] = append(hits[r.URL.Path], r.Header.Get("Authorization"))
		mu.Unlock()
	}))
	defer srv.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	dir := t.TempDir()
	write := func(name, v string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(v+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("chan-url", srv.URL+"/shared")
	write("chan-tok", "tk_1")
	if _, err := notify.PutChannel(st.DB(), notify.ChannelConfig{Name: "shared", Type: notify.ChannelTypeNtfy,
		URLCredential: "chan-url", TokenCredential: "chan-tok", Services: []string{"immich", "vault"}}); err != nil {
		t.Fatal(err)
	}
	write("ballast-ntfy-topic.photos", srv.URL+"/legacy")

	secrets := secret.NewSet()
	n, problems := BuildNotifier(st.DB(), []string{dir}, []string{"immich", "photos"}, secrets)
	if problems != nil {
		t.Fatalf("problems: %v", problems)
	}
	for _, svc := range []string{"immich", "vault", "photos", "photos"} {
		if err := n.Send(context.Background(), notify.FormatJobFailure(svc, "1", "boom")); err != nil {
			t.Fatalf("Send %s: %v", svc, err)
		}
	}
	if len(hits["/shared"]) != 2 || hits["/shared"][0] != "Bearer tk_1" {
		t.Errorf("shared channel hits = %v", hits["/shared"])
	}
	if len(hits["/legacy"]) != 2 { // two sends, one delivery each
		t.Errorf("legacy hits = %v", hits["/legacy"])
	}
	if err := n.Send(context.Background(), notify.FormatJobFailure("unrouted", "1", "x")); err == nil || !strings.Contains(err.Error(), "no notifications") {
		t.Errorf("unrouted service err = %v", err)
	}
	if red := secrets.Redact(srv.URL + "/shared tk_1"); strings.Contains(red, "/shared") || strings.Contains(red, "tk_1") {
		t.Errorf("secrets not redacted: %q", red)
	}
}

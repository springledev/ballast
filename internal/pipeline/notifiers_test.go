package pipeline

import (
	"os"
	"path/filepath"
	"testing"
)

func writeCred(t *testing.T, dir, name, value string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0o600); err != nil {
		t.Fatalf("writing credential %s: %v", name, err)
	}
}

func TestBuildNotifierPerServiceBeatsGlobal(t *testing.T) {
	dir := t.TempDir()
	writeCred(t, dir, "ballast-ntfy-topic", "https://ntfy.sh/global")
	writeCred(t, dir, "ballast-ntfy-topic.immich", "https://ntfy.sh/immich-only")

	n, secrets, err := BuildNotifier(dir, []string{"immich"})
	if err != nil {
		t.Fatalf("BuildNotifier: %v", err)
	}
	if len(secrets) != 1 || secrets[0] != "https://ntfy.sh/immich-only" {
		t.Errorf("secrets = %v, want the per-service topic only", secrets)
	}
	if n == nil {
		t.Fatal("Notifier is nil")
	}
}

func TestBuildNotifierMissingFilesMeanNoChannel(t *testing.T) {
	dir := t.TempDir()
	n, secrets, err := BuildNotifier(dir, []string{"immich"})
	if err != nil {
		t.Fatalf("BuildNotifier: %v", err)
	}
	if len(secrets) != 0 {
		t.Errorf("secrets = %v, want none", secrets)
	}
	if n == nil {
		t.Fatal("Notifier is nil")
	}
}

func TestBuildNotifierGlobalFallback(t *testing.T) {
	dir := t.TempDir()
	writeCred(t, dir, "ballast-webhook-url", "https://example.com/hook")

	n, secrets, err := BuildNotifier(dir, []string{"vaultwarden", "immich"})
	if err != nil {
		t.Fatalf("BuildNotifier: %v", err)
	}
	if len(secrets) != 2 || secrets[0] != secrets[1] {
		t.Errorf("secrets = %v, want the global webhook url registered for both services", secrets)
	}
	_ = n
}

func TestBuildNotifierBothChannels(t *testing.T) {
	dir := t.TempDir()
	writeCred(t, dir, "ballast-ntfy-topic.immich", "https://ntfy.sh/immich")
	writeCred(t, dir, "ballast-webhook-url.immich", "https://example.com/hook")

	_, secrets, err := BuildNotifier(dir, []string{"immich"})
	if err != nil {
		t.Fatalf("BuildNotifier: %v", err)
	}
	if len(secrets) != 2 {
		t.Fatalf("secrets = %v, want 2 (ntfy + webhook)", secrets)
	}
}

func TestBuildNotifierUnreadableFileErrors(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "ballast-ntfy-topic.immich")
	if err := os.WriteFile(path, []byte("https://ntfy.sh/x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := BuildNotifier(dir, []string{"immich"})
	if err == nil {
		t.Fatal("BuildNotifier: want an error for a too-permissive credential file")
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

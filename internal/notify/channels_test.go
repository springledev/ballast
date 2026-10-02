package notify

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/springledev/keel/secret"
)

func writeCred(t *testing.T, dir, name, value string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(value+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestValidateChannel(t *testing.T) {
	tests := []struct {
		name    string
		fn      func() error
		wantErr bool
	}{
		{"good name", func() error { return ValidateChannelName("my-ntfy1") }, false},
		{"upper case", func() error { return ValidateChannelName("My") }, true},
		{"path chars", func() error { return ValidateChannelName("../x") }, true},
		{"empty name", func() error { return ValidateChannelName("") }, true},
		{"https url", func() error { return ValidateChannelURL("https://ntfy.example.com/topic") }, false},
		{"bare topic", func() error { return ValidateChannelURL("my-topic") }, true},
		{"ftp url", func() error { return ValidateChannelURL("ftp://x/y") }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.fn()
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && !errors.Is(err, ErrInvalidChannel) {
				t.Errorf("err %v does not wrap ErrInvalidChannel", err)
			}
		})
	}
}

func TestDisplayURLHidesTopic(t *testing.T) {
	got := DisplayURL("https://ntfy.example.com/super-secret")
	if strings.Contains(got, "super-secret") || !strings.Contains(got, "ntfy.example.com") {
		t.Errorf("DisplayURL = %q", got)
	}
}

func TestChannelStoreRoundTrip(t *testing.T) {
	db := testDB(t)
	defer db.Close()
	d := db.DB()

	c, err := PutChannel(d, ChannelConfig{Name: "phone", Type: ChannelTypeNtfy, URLCredential: "u", Services: []string{"b", "a", "a"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PutChannel(d, ChannelConfig{Name: "all", Type: ChannelTypeNtfy, URLCredential: "u2", ApplyToAll: true}); err != nil {
		t.Fatal(err)
	}
	// Updating keeps the ID and replaces the links.
	c2, err := PutChannel(d, ChannelConfig{Name: "phone", Type: ChannelTypeNtfy, URLCredential: "u", Services: []string{"a"}})
	if err != nil || c2.ID != c.ID {
		t.Fatalf("update: id %d vs %d, err %v", c2.ID, c.ID, err)
	}

	forA, _ := ChannelsForService(d, "a")
	forB, _ := ChannelsForService(d, "b")
	if len(forA) != 2 || len(forB) != 1 || forB[0].Name != "all" {
		t.Errorf("routing wrong: a=%v b=%v", forA, forB)
	}

	if _, err := PutChannel(d, ChannelConfig{Name: "x", Type: "pigeon", URLCredential: "u"}); !errors.Is(err, ErrInvalidChannel) {
		t.Errorf("bad type err = %v", err)
	}
	if _, err := DeleteChannel(d, "phone"); err != nil {
		t.Fatal(err)
	}
	if _, err := GetChannel(d, "phone"); !errors.Is(err, ErrChannelNotFound) {
		t.Errorf("after delete err = %v", err)
	}
	var n int
	_ = d.QueryRow(`SELECT COUNT(*) FROM service_channels`).Scan(&n)
	if n != 0 {
		t.Errorf("service links left behind: %d", n)
	}
}

func TestChannelBuildAndSendAuth(t *testing.T) {
	tests := []struct {
		name, username, token string
		wantAuth              string
		status                int
		wantErr               bool
	}{
		{"open server", "", "", "", 200, false},
		{"bearer token", "", "tk_abc", "Bearer tk_abc", 200, false},
		{"basic auth", "bob", "pw", "Basic ", 200, false},
		{"auth failure", "", "wrong", "Bearer wrong", 401, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotAuth, gotTitle string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotAuth, gotTitle = r.Header.Get("Authorization"), r.Header.Get("Title")
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()
			dir := t.TempDir()
			writeCred(t, dir, "url", srv.URL+"/topic")
			c := ChannelConfig{Name: "c", Type: ChannelTypeNtfy, URLCredential: "url", Username: tt.username}
			if tt.token != "" {
				writeCred(t, dir, "tok", tt.token)
				c.TokenCredential = "tok"
			}
			secrets := secret.NewSet()
			ch, err := c.Build([]string{dir}, secrets)
			if err != nil {
				t.Fatal(err)
			}
			err = SendTest(context.Background(), ch)
			if (err != nil) != tt.wantErr {
				t.Fatalf("SendTest err = %v, wantErr %v", err, tt.wantErr)
			}
			if !strings.HasPrefix(gotAuth, tt.wantAuth) || gotTitle != "Ballast test notification" {
				t.Errorf("auth %q title %q", gotAuth, gotTitle)
			}
			if err != nil && strings.Contains(err.Error(), srv.URL) {
				t.Errorf("error leaks the address: %v", err)
			}
			if got := secrets.Redact("x " + srv.URL + "/topic"); strings.Contains(got, "/topic") {
				t.Errorf("address not registered for redaction: %q", got)
			}
		})
	}
}

func TestChannelBuildMissingCredential(t *testing.T) {
	_, err := ChannelConfig{Name: "c", URLCredential: "nope"}.Build([]string{t.TempDir()}, secret.NewSet())
	if err == nil || !strings.Contains(err.Error(), "channel c") {
		t.Errorf("err = %v", err)
	}
}

func TestResolveChannelsIsolatesBrokenChannel(t *testing.T) {
	db := testDB(t)
	defer db.Close()
	dir := t.TempDir()
	writeCred(t, dir, "good", "https://ntfy.sh/a")
	_, _ = PutChannel(db.DB(), ChannelConfig{Name: "good", Type: ChannelTypeNtfy, URLCredential: "good", ApplyToAll: true})
	_, _ = PutChannel(db.DB(), ChannelConfig{Name: "bad", Type: ChannelTypeNtfy, URLCredential: "missing", ApplyToAll: true})
	chs, err := ResolveChannels(db.DB(), []string{dir}, secret.NewSet(), "svc")
	if len(chs) != 1 || err == nil {
		t.Fatalf("chs=%d err=%v; want 1 channel and the broken one reported", len(chs), err)
	}
}

func TestMigrateLegacyChannels(t *testing.T) {
	db := testDB(t)
	defer db.Close()
	dir := t.TempDir()
	writeCred(t, dir, "ballast-ntfy-topic", "https://ntfy.sh/global")
	writeCred(t, dir, "ballast-ntfy-topic.immich", "https://ntfy.sh/immich")
	// A channel the user already named "ntfy" must not be overwritten.
	for i := 0; i < 2; i++ { // second pass proves idempotence
		if err := MigrateLegacyChannels(db.DB(), []string{dir}, []string{"immich", "vault"}); err != nil {
			t.Fatal(err)
		}
	}
	cs, _ := ListChannels(db.DB())
	if len(cs) != 2 {
		t.Fatalf("channels = %+v", cs)
	}
	forVault, _ := ChannelsForService(db.DB(), "vault")
	forImmich, _ := ChannelsForService(db.DB(), "immich")
	if len(forVault) != 1 || len(forImmich) != 2 {
		t.Errorf("vault=%d immich=%d", len(forVault), len(forImmich))
	}

	db2 := testDB(t)
	defer db2.Close()
	_, _ = PutChannel(db2.DB(), ChannelConfig{Name: "ntfy", Type: ChannelTypeNtfy, URLCredential: "mine"})
	if err := MigrateLegacyChannels(db2.DB(), []string{dir}, nil); err != nil {
		t.Fatal(err)
	}
	c, _ := GetChannel(db2.DB(), "ntfy")
	if c.URLCredential != "mine" {
		t.Errorf("user's channel overwritten: %+v", c)
	}
}

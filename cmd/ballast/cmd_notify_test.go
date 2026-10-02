package main

import (
	"os"
	"strings"
	"testing"
)

func TestNotifySetAndShow(t *testing.T) {
	withStubRestic(t)
	cliEnv(t, &spyRunner{})
	if _, err := execCLI(t, "destination", "add", "local", "--location", "/srv/backups"); err != nil {
		t.Fatalf("destination add: %v", err)
	}
	if _, err := execCLI(t, "service", "add", "immich", "--catalog", "folder", "--destination", "local", "--param", "path=/tmp"); err != nil {
		t.Fatalf("service add: %v", err)
	}

	steps := []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "defaults before any set",
			args: []string{"notify", "show", "immich"},
			want: []string{"Alert when a backup is missing:   on", "based on its schedule", "Message after every good backup:  off"},
		},
		{
			name: "set echoes the new settings",
			args: []string{"notify", "set", "immich", "--max-age-hours", "72", "--heartbeat=true", "--enabled=false"},
			want: []string{"Notifications for immich", "missing:   off", "72 hours", "good backup:  on"},
		},
		{
			name: "show reads them back",
			args: []string{"notify", "show", "immich"},
			want: []string{"missing:   off", "72 hours", "good backup:  on"},
		},
		{
			// set replaces the full settings each time (like
			// `destination add`): omitted flags reset to their defaults.
			name: "set with no flags resets to defaults",
			args: []string{"notify", "set", "immich"},
			want: []string{"missing:   on", "based on its schedule", "good backup:  off"},
		},
	}
	for _, st := range steps {
		out, err := execCLI(t, st.args...)
		if err != nil {
			t.Fatalf("%s: %v (output %q)", st.name, err, out)
		}
		for _, w := range st.want {
			if !strings.Contains(out, w) {
				t.Errorf("%s: output = %q, want it to contain %q", st.name, out, w)
			}
		}
	}
}

// TestNotifyUnknownService: a typo in the service name is an error,
// not a silently created settings row for a service that isn't there.
func TestNotifyUnknownService(t *testing.T) {
	cliEnv(t, &spyRunner{})
	for _, args := range [][]string{{"notify", "show", "nope"}, {"notify", "set", "nope"}} {
		out, err := execCLI(t, args...)
		if err == nil {
			t.Errorf("%v: want an error for an unknown service (output %q)", args, out)
		} else if !strings.Contains(err.Error(), "not found") {
			t.Errorf("%v: error = %v, want it to say the service was not found", args, err)
		}
	}
}

func TestNotifyChannelListAddRemove(t *testing.T) {
	withStubRestic(t)
	cliEnv(t, &spyRunner{})
	if _, err := execCLI(t, "destination", "add", "local", "--location", "/srv/backups"); err != nil {
		t.Fatalf("destination add: %v", err)
	}
	if _, err := execCLI(t, "service", "add", "immich", "--catalog", "folder", "--destination", "local", "--param", "path=/tmp"); err != nil {
		t.Fatalf("service add: %v", err)
	}

	tests := []struct {
		name string
		args []string
		want []string
		err  string
	}{
		{
			name: "list is empty initially",
			args: []string{"notify", "channel", "list"},
			want: []string{"No notification channels yet"},
		},
		{
			name: "add channel with all-services",
			args: []string{"notify", "channel", "add", "test-ntfy", "--url", "https://ntfy.sh/my-topic", "--all-services"},
			want: []string{"Channel: test-ntfy", "Type:      ntfy", "Address:   https://ntfy.sh/…", "Token:     off", "Applies to: all services"},
		},
		{
			name: "list shows added channel",
			args: []string{"notify", "channel", "list"},
			want: []string{"Channel: test-ntfy", "Type:      ntfy", "Address:   https://ntfy.sh/…"},
		},
		{
			name: "add second channel for specific service",
			args: []string{"notify", "channel", "add", "service-ntfy", "--url", "https://ntfy.sh/service-topic", "--service", "immich"},
			want: []string{"Channel: service-ntfy", "Applies to: immich"},
		},
		{
			name: "remove channel",
			args: []string{"notify", "channel", "remove", "test-ntfy"},
			want: []string{"Removed channel", "test-ntfy"},
		},
		{
			name: "list after remove",
			args: []string{"notify", "channel", "list"},
			want: []string{"service-ntfy"},
		},
	}

	for _, tt := range tests {
		out, err := execCLI(t, tt.args...)
		if tt.err != "" {
			if err == nil {
				t.Errorf("%s: want error containing %q, got success (output %q)", tt.name, tt.err, out)
			} else if !strings.Contains(err.Error(), tt.err) {
				t.Errorf("%s: error %v does not contain %q", tt.name, err, tt.err)
			}
		} else {
			if err != nil {
				t.Errorf("%s: %v (output %q)", tt.name, err, out)
				continue
			}
			for _, w := range tt.want {
				if !strings.Contains(out, w) {
					t.Errorf("%s: output %q does not contain %q", tt.name, out, w)
				}
			}
		}
	}
}

func TestNotifyChannelTokens(t *testing.T) {
	withStubRestic(t)
	cliEnv(t, &spyRunner{})
	if _, err := execCLI(t, "destination", "add", "local", "--location", "/srv/backups"); err != nil {
		t.Fatalf("destination add: %v", err)
	}

	tmpdir := t.TempDir()

	tests := []struct {
		name      string
		args      []string
		env       map[string]string
		wantHas   string
		wantNotIn string
	}{
		{
			name:    "add with token from file",
			args:    []string{"notify", "channel", "add", "with-file-token", "--url", "https://ntfy.sh/topic", "--token-file", tmpdir + "/token.txt", "--all-services"},
			env:     map[string]string{"_TOKEN_PATH": tmpdir + "/token.txt", "_TOKEN_CONTENT": "secret-token-from-file"},
			wantHas: "Token:     on",
		},
		{
			name:    "add with token from env var",
			args:    []string{"notify", "channel", "add", "with-env-token", "--url", "https://ntfy.sh/topic", "--all-services"},
			env:     map[string]string{"BALLAST_NTFY_TOKEN": "env-secret-token"},
			wantHas: "Token:     on",
		},
	}

	for _, tt := range tests {
		// Set up token file if needed
		if path, ok := tt.env["_TOKEN_PATH"]; ok {
			if err := os.WriteFile(path, []byte(tt.env["_TOKEN_CONTENT"]), 0o600); err != nil {
				t.Fatalf("%s: writing token file: %v", tt.name, err)
			}
		}

		// Set env vars
		if token, ok := tt.env["BALLAST_NTFY_TOKEN"]; ok {
			t.Setenv("BALLAST_NTFY_TOKEN", token)
		}

		out, err := execCLI(t, tt.args...)
		if err != nil {
			t.Errorf("%s: %v (output %q)", tt.name, err, out)
			continue
		}

		if !strings.Contains(out, tt.wantHas) {
			t.Errorf("%s: output %q does not contain %q", tt.name, out, tt.wantHas)
		}

		// Clean up env for next test
		if _, ok := tt.env["BALLAST_NTFY_TOKEN"]; ok {
			t.Setenv("BALLAST_NTFY_TOKEN", "")
		}
	}
}

func TestNotifyChannelJSON(t *testing.T) {
	withStubRestic(t)
	cliEnv(t, &spyRunner{})
	if _, err := execCLI(t, "destination", "add", "local", "--location", "/srv/backups"); err != nil {
		t.Fatalf("destination add: %v", err)
	}

	// Add a channel
	if _, err := execCLI(t, "notify", "channel", "add", "test", "--url", "https://ntfy.sh/test", "--all-services"); err != nil {
		t.Fatalf("add channel: %v", err)
	}

	// List with JSON output
	out, err := execCLI(t, "notify", "channel", "list", "--json")
	if err != nil {
		t.Fatalf("list --json: %v", err)
	}

	// Strip log lines and verify it's valid JSON
	jsonOut := stripLogLines(out)
	if !strings.Contains(jsonOut, `"name": "test"`) {
		t.Errorf("JSON output missing channel name: %q", jsonOut)
	}
	if !strings.Contains(jsonOut, `"url_display"`) {
		t.Errorf("JSON output missing url_display: %q", jsonOut)
	}
}

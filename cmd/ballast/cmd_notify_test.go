package main

import (
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

package catalog

import (
	"reflect"
	"strings"
	"testing"
)

func TestValidateExcludes(t *testing.T) {
	tests := []struct {
		name     string
		patterns []string
		wantErr  string
	}{
		{"none", nil, ""},
		{"fine", []string{"*.log", "node_modules", "/tmp", "cache/thumbs"}, ""},
		{"empty", []string{" "}, "empty"},
		{"line break", []string{"a\nb"}, "single line"},
		{"negation", []string{"!keep"}, "not supported"},
		{"bad bracket", []string{"a[b"}, "not valid"},
		{"template", []string{"{{ .Params.x }}"}, "{{"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateExcludes(tt.patterns)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestExcludesLoadCopyAndAdd(t *testing.T) {
	block := "  files:\n    - path: /srv/a\n      exclude: [\"*.log\"]\n  hostPaths:\n    - path: /srv/b\n      exclude: [cache]\n"
	// files and hostPaths together are fine for the catalog; the
	// strategy refuses mixing at run time.
	entry, err := Load([]byte(hostPathsEntryYAML(block)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(entry.Backup.Files[0].Exclude, []string{"*.log"}) || !reflect.DeepEqual(entry.Backup.HostPaths[0].Exclude, []string{"cache"}) {
		t.Fatalf("excludes not loaded: %+v %+v", entry.Backup.Files, entry.Backup.HostPaths)
	}
	c := entry.DeepCopy()
	c.AddExcludes([]string{"tmp"})
	if !reflect.DeepEqual(c.Backup.Files[0].Exclude, []string{"*.log", "tmp"}) || !reflect.DeepEqual(c.Backup.HostPaths[0].Exclude, []string{"cache", "tmp"}) {
		t.Errorf("AddExcludes on the copy: %+v %+v", c.Backup.Files, c.Backup.HostPaths)
	}
	if len(entry.Backup.Files[0].Exclude) != 1 || len(entry.Backup.HostPaths[0].Exclude) != 1 {
		t.Error("AddExcludes on a copy must not change the original")
	}

	bad := "  files:\n    - path: /srv/a\n      exclude: [\"!x\"]\n"
	if _, err := Load([]byte(hostPathsEntryYAML(bad))); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Errorf("a bad pattern must fail at load time, got %v", err)
	}
}

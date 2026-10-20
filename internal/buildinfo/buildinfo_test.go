package buildinfo

import (
	"runtime/debug"
	"testing"
)

// TestResolve checks spec §21.5: a source build reports its version
// from git, never a bare "dev", unless nothing at all is known.
func TestResolve(t *testing.T) {
	vcs := func(rev string, modified bool) *debug.BuildInfo {
		mod := "false"
		if modified {
			mod = "true"
		}
		return &debug.BuildInfo{Settings: []debug.BuildSetting{
			{Key: "vcs", Value: "git"},
			{Key: "vcs.revision", Value: rev},
			{Key: "vcs.modified", Value: mod},
		}}
	}
	tests := []struct {
		name    string
		stamped string
		bi      *debug.BuildInfo
		want    string
	}{
		{"stamped tag wins", "v0.5.0", vcs("35dad2f0123456789abcdef0123456789abcdef0", false), "v0.5.0"},
		{"stamped describe wins", "v0.5-35dad2f", nil, "v0.5-35dad2f"},
		{"no stamp uses commit", "dev", vcs("35dad2f0123456789abcdef0123456789abcdef0", false), "35dad2f"},
		{"empty stamp uses commit", "", vcs("35dad2f0123456789abcdef0123456789abcdef0", false), "35dad2f"},
		{"dirty tree is marked", "dev", vcs("35dad2f0123456789abcdef0123456789abcdef0", true), "35dad2f-dirty"},
		{"no vcs info is dev", "dev", &debug.BuildInfo{}, "dev"},
		{"no build info is dev", "", nil, "dev"},
		{"empty revision is dev", "dev", vcs("", false), "dev"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Resolve(tt.stamped, tt.bi); got != tt.want {
				t.Errorf("Resolve(%q) = %q, want %q", tt.stamped, got, tt.want)
			}
		})
	}
}

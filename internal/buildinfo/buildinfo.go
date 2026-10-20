// Package buildinfo works out the version Ballast reports (spec §21.5).
//
// A release build is stamped with -ldflags from `git describe`. A plain
// `go build` in a git checkout has no stamp, so the commit the Go
// toolchain recorded is used instead. "dev" appears only when neither
// is known.
package buildinfo

import "runtime/debug"

// Dev is the marker used when no version can be determined.
const Dev = "dev"

// shortRev is how many characters of the commit hash are shown.
const shortRev = 7

// Resolve returns the version to report. stamped is the value linked in
// with -ldflags ("" or "dev" means unstamped); bi is the toolchain's
// build information, or nil when unavailable.
func Resolve(stamped string, bi *debug.BuildInfo) string {
	if stamped != "" && stamped != Dev {
		return stamped
	}
	if bi == nil {
		return Dev
	}
	var rev, modified string
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			modified = s.Value
		}
	}
	if rev == "" {
		return Dev
	}
	if len(rev) > shortRev {
		rev = rev[:shortRev]
	}
	if modified == "true" {
		return rev + "-dirty"
	}
	return rev
}

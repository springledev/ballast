package catalog

import (
	"fmt"
	"path"
	"strings"
)

// ValidateExcludes checks a list of "skip these" patterns.
//
// A pattern is a file or folder name, or a path, that may use the
// wildcards * and ? (for example "*.log", "node_modules" or
// "cache/thumbs"). A pattern without a slash matches that name
// anywhere inside the backed-up folder; a pattern that starts with a
// slash matches from the top of the folder only. Patterns must not be
// empty or contain line breaks, and "!" (un-skip) is not supported.
func ValidateExcludes(patterns []string) error {
	for _, p := range patterns {
		if strings.TrimSpace(p) == "" {
			return fmt.Errorf("a skip pattern is empty; remove it or give a name such as \"*.log\"")
		}
		if strings.ContainsAny(p, "\n\r\x00") {
			return fmt.Errorf("skip pattern %q must be a single line", p)
		}
		if strings.HasPrefix(p, "!") {
			return fmt.Errorf("skip pattern %q: patterns that start with ! (un-skip) are not supported", p)
		}
		if _, err := path.Match(strings.TrimPrefix(p, "/"), ""); err != nil {
			return fmt.Errorf("skip pattern %q is not valid (check the [ ] brackets and backslashes)", p)
		}
		if strings.Contains(p, "{{") {
			return fmt.Errorf("skip pattern %q must not contain {{ }}", p)
		}
	}
	return nil
}

// AddExcludes appends patterns (typically the ones the user set on a
// service) to every files and hostPaths entry, so they apply on top of
// the catalog's own excludes.
func (e *Entry) AddExcludes(patterns []string) {
	if len(patterns) == 0 {
		return
	}
	for i := range e.Backup.Files {
		e.Backup.Files[i].Exclude = append(e.Backup.Files[i].Exclude, patterns...)
	}
	for i := range e.Backup.HostPaths {
		e.Backup.HostPaths[i].Exclude = append(e.Backup.HostPaths[i].Exclude, patterns...)
	}
}

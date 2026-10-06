package strategy

import (
	"path"
	"strings"
)

// excluded reports whether rel, a slash-separated path inside a
// backed-up folder (no leading slash), matches one of the skip
// patterns. A pattern with a leading slash matches from the top of the
// folder; any other pattern matches the end of the path, so "*.log"
// and "node_modules" match at any depth and "cache/thumbs" matches a
// "thumbs" folder inside any "cache" folder. It mirrors restic's own
// --exclude rules closely enough that every strategy skips the same
// things.
func excluded(patterns []string, rel string) bool {
	rel = strings.Trim(rel, "/")
	if rel == "" {
		return false
	}
	parts := strings.Split(rel, "/")
	for _, p := range patterns {
		anchored := strings.HasPrefix(p, "/")
		p = strings.Trim(p, "/")
		if p == "" {
			continue
		}
		if anchored {
			if ok, _ := path.Match(p, rel); ok {
				return true
			}
			continue
		}
		for i := range parts {
			if ok, _ := path.Match(p, strings.Join(parts[i:], "/")); ok {
				return true
			}
		}
	}
	return false
}

// tarExcludeArgs turns skip patterns into `tar --exclude` arguments
// for the folder mounted at ./<name> inside the helper container. The
// helper's tar (BusyBox) matches globs against the whole member name
// with * crossing slashes, so each pattern is expanded to the forms
// that cover the folder itself and everything below it.
func tarExcludeArgs(name string, patterns []string) []string {
	var args []string
	add := func(p string) { args = append(args, "--exclude", p) }
	base := "./" + name
	for _, p := range patterns {
		anchored := strings.HasPrefix(p, "/")
		p = strings.Trim(p, "/")
		if p == "" {
			continue
		}
		add(base + "/" + p)
		add(base + "/" + p + "/*")
		if !anchored {
			add(base + "/*/" + p)
			add(base + "/*/" + p + "/*")
		}
	}
	return args
}

// Package secret holds the one list of secret strings (repository
// passwords, notification URLs, API tokens) that must never reach a
// log line, an error message, or an API response (spec rule 2).
//
// A Set is shared by pointer between every component that redacts —
// the command runner, the pipeline, the scheduler, and the API — so a
// secret that first appears while Ballast is already running (a
// repository password generated for a new destination, a notification
// URL added after startup) is redacted everywhere the moment it is
// added, without restarting.
package secret

import (
	"os"
	"sort"
	"strings"
	"sync"
)

// Placeholder replaces every redacted secret.
const Placeholder = "[redacted]"

// Set is a concurrency-safe, growable list of secrets. A nil *Set is
// valid and redacts nothing.
type Set struct {
	parent *Set

	mu     sync.RWMutex
	values []string
}

// NewSet returns a Set holding values (empty strings are ignored).
func NewSet(values ...string) *Set {
	s := &Set{}
	s.Add(values...)
	return s
}

// Add records more secrets. Empty strings and duplicates are ignored.
func (s *Set) Add(values ...string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range values {
		if v == "" || contains(s.values, v) {
			continue
		}
		s.values = append(s.values, v)
	}
}

// AddFile records the contents of the secret file at path (surrounding
// whitespace trimmed, as restic itself trims a --password-file). A
// missing or unreadable file adds nothing and is not an error: the
// command that actually uses the file reports that problem loudly;
// this only makes sure whatever that command can read is redacted.
// Calling it again after the file changed adds the new value too.
func (s *Set) AddFile(path string) {
	if s == nil || path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	s.Add(strings.TrimSpace(string(data)))
}

// With returns a child Set that redacts everything s redacts —
// including secrets added to s later — plus extra. s itself is not
// changed, so a caller can scope extra secrets to one command
// without leaking them into every other user of s.
func (s *Set) With(extra ...string) *Set {
	c := NewSet(extra...)
	c.parent = s
	return c
}

// Values returns a snapshot of every secret in s and its parents.
func (s *Set) Values() []string {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	out := append([]string(nil), s.values...)
	s.mu.RUnlock()
	for _, v := range s.parent.Values() {
		if !contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

// Redact replaces every occurrence of every secret in text with
// Placeholder, in a single pass: replaced text is never scanned again,
// and where secrets overlap the longest wins, so a secret that is a
// prefix of another cannot leave the longer one's tail visible (e.g.
// "https://ntfy.sh/abc" and "https://ntfy.sh/abc-photos").
func (s *Set) Redact(text string) string {
	values := s.Values()
	if len(values) == 0 {
		return text
	}
	// strings.Replacer prefers earlier pairs at the same position.
	sort.SliceStable(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	pairs := make([]string, 0, 2*len(values))
	for _, v := range values {
		pairs = append(pairs, v, Placeholder)
	}
	return strings.NewReplacer(pairs...).Replace(text)
}

func contains(ss []string, v string) bool {
	for _, s := range ss {
		if s == v {
			return true
		}
	}
	return false
}

package secret

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestRedact(t *testing.T) {
	tests := []struct {
		name   string
		values []string
		in     string
		want   string
	}{
		{"no secrets", nil, "plain text", "plain text"},
		{"one secret", []string{"hunter2"}, "password hunter2 rejected", "password [redacted] rejected"},
		{"every occurrence", []string{"abc"}, "abc and abc", "[redacted] and [redacted]"},
		{"empty entry ignored", []string{"", "tok"}, "a tok b", "a [redacted] b"},
		{"prefix/substring: short added first", []string{"https://ntfy.sh/abc", "https://ntfy.sh/abc-photos"}, "url https://ntfy.sh/abc-photos", "url [redacted]"},
		{"secret inside the placeholder text", []string{"hunter2", "red"}, "hunter2 red", "[redacted] [redacted]"},
		{"short single-letter secret", []string{"A", "tok"}, "tok A", "[redacted] [redacted]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NewSet(tt.values...).Redact(tt.in); got != tt.want {
				t.Errorf("Redact(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestNilSetRedactsNothing(t *testing.T) {
	var s *Set
	s.Add("ignored")
	if got := s.Redact("ignored"); got != "ignored" {
		t.Errorf("nil Set Redact = %q, want input unchanged", got)
	}
}

// TestAddAfterSharing: a secret added after the Set was handed out is
// redacted by every holder — the whole point of sharing one Set.
func TestAddAfterSharing(t *testing.T) {
	shared := NewSet("first")
	holder := shared
	shared.Add("second")
	if got := holder.Redact("first second"); got != "[redacted] [redacted]" {
		t.Errorf("Redact = %q, want both secrets redacted", got)
	}
}

func TestWithScopesExtraSecrets(t *testing.T) {
	parent := NewSet("p")
	child := parent.With("c")
	parent.Add("later")

	if got := child.Redact("p c later"); got != "[redacted] [redacted] [redacted]" {
		t.Errorf("child Redact = %q, want parent (including later additions) and extra redacted", got)
	}
	if got := parent.Redact("c"); got != "c" {
		t.Errorf("parent Redact = %q, want the child's extra secret left alone", got)
	}
}

// TestWithPrefixChild: when parent has a shorter prefix secret and
// child has the longer one, the longer child secret should be redacted
// completely (not leaking the suffix).
func TestWithPrefixChild(t *testing.T) {
	parent := NewSet("https://ntfy.sh/abc")
	child := parent.With("https://ntfy.sh/abc-photos")

	if got := child.Redact("url https://ntfy.sh/abc-photos"); got != "url [redacted]" {
		t.Errorf("child Redact = %q, want entire longer URL redacted", got)
	}
}

func TestAddDeduplicates(t *testing.T) {
	s := NewSet("a", "a")
	s.Add("a", "b")
	if got := s.Values(); len(got) != 2 {
		t.Errorf("Values = %v, want 2 unique entries", got)
	}
}

func TestConcurrentAddAndRedact(t *testing.T) {
	s := NewSet()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); s.Add("x") }()
		go func() { defer wg.Done(); _ = s.Redact("x") }()
	}
	wg.Wait()
}

func TestAddFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	tests := []struct {
		name string
		path string
		in   string
		want string
	}{
		{"trailing newline trimmed", write("nl", "pw-one\n"), "x pw-one y", "x [redacted] y"},
		{"surrounding whitespace trimmed", write("ws", "  pw-two \n"), "pw-two", "[redacted]"},
		{"missing file adds nothing", filepath.Join(dir, "absent"), "absent", "absent"},
		{"empty file adds nothing", write("empty", "\n"), "text", "text"},
		{"empty path adds nothing", "", "text", "text"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewSet()
			s.AddFile(tt.path)
			if got := s.Redact(tt.in); got != tt.want {
				t.Errorf("Redact(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestAddFileAfterChange: re-reading a replaced file redacts the new
// value as well as the old one (output from before the change may
// still be in flight).
func TestAddFileAfterChange(t *testing.T) {
	p := filepath.Join(t.TempDir(), "pw")
	s := NewSet()
	for _, pw := range []string{"old-pw", "new-pw"} {
		if err := os.WriteFile(p, []byte(pw+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		s.AddFile(p)
	}
	if got := s.Redact("old-pw new-pw"); got != "[redacted] [redacted]" {
		t.Errorf("Redact = %q, want both values redacted", got)
	}
}

func TestNilSetAddFile(t *testing.T) {
	var s *Set
	s.AddFile("/nonexistent") // must not panic
}

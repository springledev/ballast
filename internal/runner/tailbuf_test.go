package runner

import (
	"strings"
	"testing"
)

// TestTailBufferKeepsTail drives the ring through its capacity edge
// cases: only the most recent cap bytes may survive, in order.
func TestTailBufferKeepsTail(t *testing.T) {
	cases := []struct {
		name   string
		cap    int
		writes []string
		want   string
	}{
		{"empty", 8, nil, ""},
		{"single write under cap", 8, []string{"abc"}, "abc"},
		{"single write exactly cap", 3, []string{"abc"}, "abc"},
		{"single write over cap keeps tail", 3, []string{"abcdef"}, "def"},
		{"several writes under cap", 8, []string{"ab", "cd"}, "abcd"},
		{"overflow drops the head", 4, []string{"abcd", "ef"}, "cdef"},
		{"repeated overflow keeps last bytes", 4, []string{"aaaa", "bbbb", "cc"}, "bbcc"},
		{"huge write after small ones", 5, []string{"xy", "123456789"}, "56789"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tb := &tailBuffer{cap: tc.cap}
			for _, w := range tc.writes {
				n, err := tb.Write([]byte(w))
				if err != nil {
					t.Fatalf("Write: %v", err)
				}
				if n != len(w) {
					t.Fatalf("Write reported %d, want %d", n, len(w))
				}
			}
			if tb.String() != tc.want {
				t.Errorf("tail = %q, want %q", tb.String(), tc.want)
			}
			if len(tb.String()) > tc.cap {
				t.Errorf("tail exceeds cap: %d > %d", len(tb.String()), tc.cap)
			}
		})
	}
}

// TestTailBufferLargeStream: a stream far beyond capacity stays
// bounded and ends with the stream's true end.
func TestTailBufferLargeStream(t *testing.T) {
	tb := &tailBuffer{cap: 100}
	chunk := strings.Repeat("x", 37)
	for i := 0; i < 1000; i++ {
		if _, err := tb.Write([]byte(chunk)); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if _, err := tb.Write([]byte("THE-END")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(tb.String()) > 100 {
		t.Errorf("tail = %d bytes, want <= 100", len(tb.String()))
	}
	if !strings.HasSuffix(tb.String(), "THE-END") {
		t.Errorf("tail must end with the final write; got %q", tb.String())
	}
}

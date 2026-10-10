package progress

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTracker(t *testing.T) {
	tests := []struct {
		name string
		run  func(tr *Tracker)
		want int
	}{
		{"empty", func(*Tracker) {}, 0},
		{"set", func(tr *Tracker) { tr.Set("a", Upload{Destination: "b2", Percent: 10}) }, 1},
		{"overwrite", func(tr *Tracker) {
			tr.Set("a", Upload{Destination: "b2", Percent: 10})
			tr.Set("a", Upload{Destination: "b2", Percent: 20})
		}, 1},
		{"two destinations", func(tr *Tracker) {
			tr.Set("a", Upload{Destination: "b2"})
			tr.Set("a", Upload{Destination: "local"})
		}, 2},
		{"cleared", func(tr *Tracker) {
			tr.Set("a", Upload{Destination: "b2"})
			tr.Clear("a", "b2")
		}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var tr Tracker
			tt.run(&tr)
			if got := len(tr.ForService("a")); got != tt.want {
				t.Errorf("len = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestNilTracker(t *testing.T) {
	var tr *Tracker
	tr.Set("a", Upload{})
	tr.Clear("a", "b")
	if _, ok := tr.Get("a", "b"); ok || tr.ForService("a") != nil {
		t.Error("nil tracker returned data")
	}
}

// TestUploadFieldsAreNumbersAndNames: the payload carries only a
// destination name and numbers.
func TestUploadFieldsAreNumbersAndNames(t *testing.T) {
	b, _ := json.Marshal(Upload{Destination: "b2", Percent: 43.9, BytesDone: 1, TotalBytes: 2})
	if !strings.Contains(string(b), `"destination":"b2"`) || strings.Contains(string(b), "path") {
		t.Errorf("json = %s", b)
	}
}

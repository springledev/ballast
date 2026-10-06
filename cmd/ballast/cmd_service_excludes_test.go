package main

import (
	"testing"

	"github.com/springledev/ballast/internal/inventory"
)

func TestHumanBytes(t *testing.T) {
	tests := []struct {
		n    int64
		want string
	}{{0, "0 B"}, {1023, "1023 B"}, {1024, "1.0 KB"}, {1536, "1.5 KB"}, {5 << 20, "5.0 MB"}, {3 << 30, "3.0 GB"}}
	for _, tt := range tests {
		if got := humanBytes(tt.n); got != tt.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}

func TestRetentionWords(t *testing.T) {
	tests := []struct {
		r    inventory.Retention
		want string
	}{
		{inventory.Retention{KeepDaily: 7, KeepWeekly: 4, KeepMonthly: 6}, "7 daily, 4 weekly and 6 monthly backups"},
		{inventory.Retention{KeepLast: 2}, "the newest 2, 0 daily, 0 weekly and 0 monthly backups"},
	}
	for _, tt := range tests {
		if got := retentionWords(tt.r); got != tt.want {
			t.Errorf("retentionWords = %q, want %q", got, tt.want)
		}
	}
}

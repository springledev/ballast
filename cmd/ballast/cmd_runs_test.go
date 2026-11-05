package main

import (
	"strings"
	"testing"
)

func TestRunsCommand(t *testing.T) {
	setupRealBackup(t)
	out, err := execCLI(t, "runs", "docs")
	if err != nil || !strings.Contains(out, "#1") {
		t.Fatalf("runs: %v (output %q)", err, out)
	}
	out, err = execCLI(t, "runs", "docs", "--json", "--limit", "1")
	if err != nil || !strings.Contains(out, `"runs"`) {
		t.Fatalf("runs --json: %v (output %q)", err, out)
	}
	if _, err := execCLI(t, "runs", "ghost"); err == nil {
		t.Error("runs for an unknown service: want an error")
	}
}

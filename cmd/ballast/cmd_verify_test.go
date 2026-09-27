package main

import (
	"strings"
	"testing"
)

func TestVerifyCommand(t *testing.T) {
	setupRealBackup(t)
	out, err := execCLI(t, "verify", "docs")
	if err != nil {
		t.Fatalf("verify: %v (output %q)", err, out)
	}
	if !strings.Contains(out, "docs") || !strings.Contains(out, "green") {
		t.Errorf("output = %q, want it to confirm docs verified green", out)
	}
	if !strings.Contains(out, "restic-read-back") {
		t.Errorf("output = %q, want the read-back check listed", out)
	}
}

func TestVerifyCommandAll(t *testing.T) {
	setupRealBackup(t)
	out, err := execCLI(t, "verify", "--all")
	if err != nil {
		t.Fatalf("verify --all: %v (output %q)", err, out)
	}
	if !strings.Contains(out, "docs") || !strings.Contains(out, "green") {
		t.Errorf("output = %q, want docs reported green", out)
	}
}

func TestVerifyCommandUnknownService(t *testing.T) {
	cliEnv(t, &spyRunner{})
	if _, err := execCLI(t, "verify", "ghost"); err == nil {
		t.Error("verify for an unconfigured service: want an error")
	}
}

func TestVerifyCommandRequiresServiceOrAll(t *testing.T) {
	cliEnv(t, &spyRunner{})
	if _, err := execCLI(t, "verify"); err == nil {
		t.Error("verify with neither a service nor --all: want an error")
	}
}

func TestVerifyCommandRejectsServiceAndAll(t *testing.T) {
	cliEnv(t, &spyRunner{})
	if _, err := execCLI(t, "verify", "docs", "--all"); err == nil {
		t.Error("verify with both a service and --all: want an error")
	}
}

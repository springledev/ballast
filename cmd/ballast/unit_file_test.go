package main

import (
	"os"
	"strings"
	"testing"
)

// TestShippedUnitHardening guards the directives the spec requires of
// deploy/ballast.service so a later edit cannot silently drop them.
func TestShippedUnitHardening(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/ballast.service")
	if err != nil {
		t.Fatalf("reading shipped unit: %v", err)
	}
	unit := string(raw)
	for _, want := range []string{
		"Restart=on-failure",
		"LoadCredential=ballast-api-token:",
		"ProtectSystem=strict",
		"PrivateTmp=true",
		"NoNewPrivileges=true",
		"ReadWritePaths=",
		"TimeoutStopSec=5min",
		"KillMode=mixed",
		"ExecStart=/usr/local/bin/ballast serve",
	} {
		t.Run(want, func(t *testing.T) {
			if !strings.Contains(unit, want) {
				t.Errorf("deploy/ballast.service is missing %q", want)
			}
		})
	}
}

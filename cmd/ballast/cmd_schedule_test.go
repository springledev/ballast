package main

import (
	"strings"
	"testing"
)

func TestScheduleSetEnableDisableList(t *testing.T) {
	cliEnv(t, &spyRunner{})

	out, err := execCLI(t, "schedule", "set", "immich", "0 3 * * *")
	if err != nil {
		t.Fatalf("schedule set: %v (output %q)", err, out)
	}
	if !strings.Contains(out, "immich") || !strings.Contains(out, "0 3 * * *") {
		t.Errorf("output = %q, want it to name the service and cron expression", out)
	}

	out, err = execCLI(t, "schedule", "list")
	if err != nil {
		t.Fatalf("schedule list: %v", err)
	}
	if !strings.Contains(out, "immich") || !strings.Contains(out, "enabled") {
		t.Errorf("list output = %q, want immich listed and enabled", out)
	}

	if _, err := execCLI(t, "schedule", "disable", "immich"); err != nil {
		t.Fatalf("schedule disable: %v", err)
	}
	out, err = execCLI(t, "schedule", "list")
	if err != nil {
		t.Fatalf("schedule list: %v", err)
	}
	if !strings.Contains(out, "disabled") {
		t.Errorf("list output after disable = %q, want disabled", out)
	}

	if _, err := execCLI(t, "schedule", "enable", "immich"); err != nil {
		t.Fatalf("schedule enable: %v", err)
	}
	out, err = execCLI(t, "schedule", "list")
	if err != nil {
		t.Fatalf("schedule list: %v", err)
	}
	if !strings.Contains(out, "enabled") || strings.Contains(out, "disabled") {
		t.Errorf("list output after re-enable = %q, want enabled only", out)
	}
}

func TestScheduleSetInvalidCron(t *testing.T) {
	cliEnv(t, &spyRunner{})
	_, err := execCLI(t, "schedule", "set", "immich", "not a cron expression")
	if err == nil {
		t.Error("schedule set with an invalid cron expression: want an error")
	}
}

func TestScheduleEnableUnknownService(t *testing.T) {
	cliEnv(t, &spyRunner{})
	_, err := execCLI(t, "schedule", "enable", "ghost")
	if err == nil {
		t.Error("schedule enable for a service with no schedule: want an error")
	}
}

package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/springledev/ballast/internal/engine"
	"github.com/springledev/ballast/internal/store"
)

func TestBackupCommandSuccess(t *testing.T) {
	r := &spyRunner{}
	cliEnv(t, r)
	out, err := execCLI(t, "backup", "immich")
	if err != nil {
		t.Fatalf("backup: %v (output %q)", err, out)
	}
	if !strings.Contains(out, "immich") || !strings.Contains(out, "verified") {
		t.Errorf("output = %q, want it to confirm the verified backup", out)
	}
	if len(r.ran) != 1 || r.ran[0] != "immich" {
		t.Errorf("ran = %v, want [immich]", r.ran)
	}
}

func TestBackupCommandFailure(t *testing.T) {
	cliEnv(t, &spyRunner{err: errors.New("boom")})
	out, err := execCLI(t, "backup", "immich")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("backup error = %v, want the failure reported", err)
	}
	_ = out
}

func TestBackupCommandAmberIsAnError(t *testing.T) {
	cliEnv(t, &spyRunner{state: engine.SUCCESS_UNVERIFIED})
	_, err := execCLI(t, "backup", "immich")
	if err == nil {
		t.Fatal("backup with SUCCESS_UNVERIFIED: want a non-nil error, not a silent success")
	}
}

// TestBackupCommandInterruptedPreviousRunIsWarningNotFailure: an
// earlier run left unfinished (Ballast was killed or rebooted) must
// not make a verified new backup look like a failure (spec §14 step
// 11 hard rule 6) — it should be reported as a warning on stderr, with
// the command still exiting 0.
func TestBackupCommandInterruptedPreviousRunIsWarningNotFailure(t *testing.T) {
	r := &spyRunner{}
	data := cliEnv(t, r)
	withDB(t, data, func(s *store.Store) {
		m, err := engine.StartRun(s.DB(), "immich")
		if err != nil {
			t.Fatalf("StartRun: %v", err)
		}
		if err := m.Transition(engine.PREPARING, ""); err != nil {
			t.Fatalf("transition: %v", err)
		}
	})

	out, err := execCLI(t, "backup", "immich")
	if err != nil {
		t.Fatalf("backup: %v (output %q), want success despite an interrupted earlier run", err, out)
	}
	if !strings.Contains(out, "immich") || !strings.Contains(out, "verified") {
		t.Errorf("output = %q, want it to confirm the verified backup", out)
	}
	if !strings.Contains(out, "warning") || !strings.Contains(out, "interrupted") {
		t.Errorf("output = %q, want the interrupted earlier run reported as a warning", out)
	}
	if len(r.ran) != 1 || r.ran[0] != "immich" {
		t.Errorf("ran = %v, want [immich]", r.ran)
	}
}

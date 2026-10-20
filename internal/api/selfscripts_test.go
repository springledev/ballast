package api

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/springledev/ballast/internal/catalog"
)

func scriptEntryYAML(service, script, verify string) string {
	return "apiVersion: ballast.dev/v1\nservice: " + service + "\nversion: 1\nbackup:\n  strategy: exec\n  exec:\n    script: " + script +
		"\n    verifyScript: " + verify + "\nverify:\n  method: exec-script\n"
}

func withEntries(t *testing.T, h *harness, entries map[string]string) {
	t.Helper()
	fsys := fstest.MapFS{}
	for name, y := range entries {
		fsys[name+".yaml"] = &fstest.MapFile{Data: []byte(y)}
	}
	reg, err := catalog.LoadRegistry(fsys, nil)
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	h.ops.Catalog = reg
}

func TestSelfBackupIncludesReferencedScripts(t *testing.T) {
	h := newHarness(t)
	h.addDestination("local")
	h.ops.WorkRoot = filepath.Join(t.TempDir(), "work")
	scripts := t.TempDir()
	backup := filepath.Join(scripts, "backup.sh")
	verify := filepath.Join(scripts, "check.sh")
	for _, p := range []string{backup, verify} {
		if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	missing := filepath.Join(scripts, "gone.sh")
	withEntries(t, h, map[string]string{
		"mine":  scriptEntryYAML("mine", backup, verify),
		"mine2": scriptEntryYAML("mine2", missing, verify), // duplicate verify script, one missing
	})

	res, err := h.ops.SelfBackup(context.Background(), "local")
	if err != nil {
		t.Fatalf("SelfBackup: %v", err)
	}
	sort.Strings(res.Files)
	want := []string{
		"ballast.db",
		"scripts" + backup,
		"scripts" + verify,
	}
	if strings.Join(res.Files, ",") != strings.Join(want, ",") {
		t.Fatalf("files = %v, want %v", res.Files, want)
	}
}

func TestSelfBackupLeavesCredentialsOut(t *testing.T) {
	tests := []struct {
		name string
		make func(t *testing.T, credDir string) string
	}{
		{"script inside the credentials folder", func(t *testing.T, credDir string) string {
			p := filepath.Join(credDir, "sneaky.sh")
			os.WriteFile(p, []byte("secret"), 0o600)
			return p
		}},
		{"credential file itself", func(t *testing.T, credDir string) string {
			p := filepath.Join(credDir, "restic-password")
			os.WriteFile(p, []byte("hunter2"), 0o600)
			return p
		}},
		{"symlink pointing into the credentials folder", func(t *testing.T, credDir string) string {
			target := filepath.Join(credDir, "b2-key")
			os.WriteFile(target, []byte("hunter2"), 0o600)
			link := filepath.Join(t.TempDir(), "looks-innocent.sh")
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			return link
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.addDestination("local")
			h.ops.WorkRoot = filepath.Join(t.TempDir(), "work")
			script := tt.make(t, h.credDir)
			withEntries(t, h, map[string]string{"mine": scriptEntryYAML("mine", script, script)})
			stage, err := h.ops.stageSelf(context.Background())
			if err != nil {
				t.Fatalf("stageSelf: %v", err)
			}
			defer stage.remove()
			for _, f := range stage.files {
				if strings.HasPrefix(f, "scripts") {
					t.Fatalf("credential-area file swept into the self-backup: %v", stage.files)
				}
			}
			warned := false
			for _, c := range h.ops.scriptDoctorChecks() {
				if c.Status == DoctorWarn && strings.Contains(c.Detail, "credential") {
					warned = true
				}
			}
			if !warned {
				t.Fatal("doctor gave no warning that the script is left out as a credential")
			}
		})
	}
}

func TestScriptDoctorChecks(t *testing.T) {
	h := newHarness(t)
	h.addDestination("local")
	dir := t.TempDir()
	ok := filepath.Join(dir, "ok.sh")
	os.WriteFile(ok, []byte("#!/bin/sh\n"), 0o755)
	gone := filepath.Join(dir, "gone.sh")
	withEntries(t, h, map[string]string{"mine": scriptEntryYAML("mine", ok, gone)})
	var okN, warnN int
	for _, c := range h.ops.scriptDoctorChecks() {
		switch c.Status {
		case DoctorOK:
			okN++
		case DoctorWarn:
			warnN++
			if !strings.Contains(c.Detail, gone) || !strings.Contains(c.Detail, "missing") && !strings.Contains(c.Detail, "not found") {
				t.Fatalf("warning does not name the missing script: %q", c.Detail)
			}
		}
	}
	if okN != 1 || warnN != 1 {
		t.Fatalf("ok=%d warn=%d, want 1 and 1", okN, warnN)
	}
}

// TestRunSelfBackupWithScriptsReachesVerifiedSuccess proves the scheduled
// self-backup that carries a custom entry's script still goes through
// VERIFYING (a read-back per destination) before SUCCESS.
func TestRunSelfBackupWithScriptsReachesVerifiedSuccess(t *testing.T) {
	h, _ := selfHarness(t)
	script := filepath.Join(t.TempDir(), "check.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	withEntries(t, h, map[string]string{"mine": scriptEntryYAML("mine", script, script)})

	state, err := h.ops.RunSelfBackup(context.Background())
	if err != nil || state != "SUCCESS" {
		t.Fatalf("RunSelfBackup = %v, %v; want SUCCESS", state, err)
	}
	backups := 0
	for _, l := range h.resticInvocations(t) {
		if strings.HasPrefix(l, "backup ") && strings.Contains(l, "--tag "+SelfBackupTag) {
			backups++
		}
	}
	if backups != 2 {
		t.Fatalf("want one tagged backup per destination (2), got %d", backups)
	}
	st := statusOf(t, h, SelfBackupService)
	if len(st.Verification) == 0 {
		t.Fatalf("no verification recorded: %+v", st)
	}
	if _, err := os.Stat(filepath.Join(h.ops.WorkRoot, "_self")); !os.IsNotExist(err) {
		t.Fatalf("staging left behind: %v", err)
	}
}

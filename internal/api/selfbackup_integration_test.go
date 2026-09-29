package api

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/restic"
	"github.com/springledev/ballast/internal/runner"
)

// TestSelfBackupRealResticIntegration runs a self-backup against a real
// local restic repository: the snapshot is read back (verified), the
// status DB in it is a usable SQLite file, and the credential's secret
// is nowhere in the snapshot. Doctor then flags a wrong password.
func TestSelfBackupRealResticIntegration(t *testing.T) {
	bin, err := exec.LookPath("restic")
	if err != nil {
		t.Skip("restic not installed")
	}
	h := newHarness(t)
	repoDir := filepath.Join(t.TempDir(), "repo")
	pwFile := filepath.Join(h.credDir, "ballast-restic-password.real")
	const password = "s3cret-repo-password"
	if err := os.WriteFile(pwFile, []byte(password+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := inventory.PutDestination(h.ops.DB, inventory.Destination{Name: "real", Location: repoDir, KeepDaily: 7, KeepWeekly: 4, KeepMonthly: 6}); err != nil {
		t.Fatal(err)
	}
	r := &runner.Runner{BaseEnv: append(os.Environ(), "RESTIC_CACHE_DIR="+t.TempDir())}
	open := func(pw string) func(inventory.Destination) (*restic.Repository, error) {
		return func(d inventory.Destination) (*restic.Repository, error) {
			return restic.Open(d.Location, restic.Options{Runner: r, PasswordFile: pw, ResticBin: bin})
		}
	}
	h.ops.Repo = open(pwFile)
	h.ops.Runner = r
	h.ops.ResticBin = bin
	h.ops.WorkRoot = filepath.Join(t.TempDir(), "work")
	repo, _ := h.ops.Repo(inventory.Destination{Location: repoDir})
	if err := repo.Init(context.Background()); err != nil {
		t.Fatalf("init: %v", err)
	}
	cfg := filepath.Join(t.TempDir(), "ballast.yaml")
	os.WriteFile(cfg, []byte("max_parallel_jobs: 2\n"), 0o600)
	h.ops.SelfBackupFiles = SelfBackupSources{ConfigFile: cfg}

	res, err := h.ops.SelfBackup(context.Background(), "real")
	if err != nil || !res.Verified {
		t.Fatalf("SelfBackup = %+v, %v; want verified", res, err)
	}
	out := filepath.Join(t.TempDir(), "restored")
	if _, err := r.Run(context.Background(), nil, nil, runner.Command{Name: bin, Args: []string{"restore", res.SnapshotID, "--target", out, "-r", repoDir, "--password-file", pwFile}}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	var found int
	filepath.Walk(out, func(p string, fi os.FileInfo, _ error) error {
		if fi == nil || fi.IsDir() {
			return nil
		}
		found++
		b, _ := os.ReadFile(p)
		if strings.Contains(string(b), password) {
			t.Errorf("repository password found in restored %s", p)
		}
		if filepath.Base(p) == "ballast.db" && !strings.HasPrefix(string(b), "SQLite format 3") {
			t.Errorf("restored ballast.db is not a SQLite file")
		}
		return nil
	})
	if found != 2 {
		t.Errorf("restored %d files, want 2 (ballast.db, ballast.yaml)", found)
	}

	// Doctor: a wrong password must be a failed destination line.
	h.ops.Repo = open(func() string {
		p := filepath.Join(h.credDir, "wrong")
		os.WriteFile(p, []byte("nope\n"), 0o600)
		return p
	}())
	rep, err := h.ops.Doctor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var bad bool
	for _, c := range rep.Checks {
		if c.Area == "destination" && c.Name == "real" && c.Status == DoctorFail {
			bad = true
			if strings.Contains(c.Detail, "nope") || strings.Contains(c.Detail, password) {
				t.Errorf("doctor detail leaks a password: %s", c.Detail)
			}
		}
	}
	if !bad || rep.Healthy() {
		t.Errorf("wrong password not flagged: %+v", rep.Checks)
	}
}

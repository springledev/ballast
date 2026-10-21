package api

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/scheduler"
)

func TestSelfBackupStagesAndVerifies(t *testing.T) {
	h := newHarness(t)
	h.addDestination("local")
	h.ops.WorkRoot = filepath.Join(t.TempDir(), "work")

	src := t.TempDir()
	cfg := filepath.Join(src, "ballast.yaml")
	cat := filepath.Join(src, "catalog.d")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(cfg, []byte("max_parallel_jobs: 2\n"), 0o600))
	must(os.MkdirAll(filepath.Join(cat, "sub"), 0o700))
	must(os.WriteFile(filepath.Join(cat, "mine.yaml"), []byte("x: 1\n"), 0o600))
	must(os.WriteFile(filepath.Join(cat, "sub", "deep.yaml"), []byte("y: 2\n"), 0o600))
	h.ops.SelfBackupFiles = SelfBackupSources{ConfigFile: cfg, CatalogDirs: []string{cat, filepath.Join(src, "missing")}}

	res, err := h.ops.SelfBackup(context.Background(), "local")
	if err != nil {
		t.Fatalf("SelfBackup: %v", err)
	}
	if !res.Verified || res.SnapshotID == "" {
		t.Fatalf("result = %+v, want verified with a snapshot id", res)
	}
	sort.Strings(res.Files)
	want := []string{"ballast.db", "ballast.yaml", "catalog.d/0/mine.yaml", "catalog.d/0/sub/deep.yaml"}
	if strings.Join(res.Files, ",") != strings.Join(want, ",") {
		t.Fatalf("files = %v, want %v", res.Files, want)
	}
	log := strings.Join(h.resticInvocations(t), "\n")
	if !strings.Contains(log, "backup") || !strings.Contains(log, "--tag "+SelfBackupTag) {
		t.Fatalf("restic calls missing tagged backup:\n%s", log)
	}
	if _, err := os.Stat(filepath.Join(h.ops.WorkRoot, "_self")); !os.IsNotExist(err) {
		t.Fatalf("staging directory left behind: %v", err)
	}
}

func TestSelfBackupErrors(t *testing.T) {
	h := newHarness(t)
	if _, err := h.ops.SelfBackup(context.Background(), "local"); err == nil {
		t.Fatal("no work root: want error")
	}
	h.ops.WorkRoot = t.TempDir()
	if _, err := h.ops.SelfBackup(context.Background(), "ghost"); err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("unknown destination error = %v", err)
	}
}

// TestSelfStagingIsPerRun: two staging folders never share a path, so
// one self-backup cannot wipe the files another is uploading.
func TestSelfStagingIsPerRun(t *testing.T) {
	h := newHarness(t)
	h.ops.WorkRoot = filepath.Join(t.TempDir(), "work")
	a, err := h.ops.stageSelf(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer a.remove()
	b, err := h.ops.stageSelf(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer b.remove()
	if a.dir == b.dir {
		t.Fatalf("both staging folders are %s", a.dir)
	}
	for _, st := range []*selfStaging{a, b} {
		if _, err := os.Stat(filepath.Join(st.dir, "ballast.db")); err != nil {
			t.Errorf("staging %s lost its database copy: %v", st.dir, err)
		}
	}
}

// TestSelfBackupRefusesConcurrentRun: a manual self-backup started
// while another is uploading is refused with a plain message, and the
// running one is left untouched and finishes verified.
func TestSelfBackupRefusesConcurrentRun(t *testing.T) {
	h := newHarness(t)
	h.addDestination("local")
	h.ops.WorkRoot = filepath.Join(t.TempDir(), "work")
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	h.ops.HoldRepository = func(context.Context, string, string) (func(), error) {
		once.Do(func() {
			close(entered)
			<-release
		})
		return func() {}, nil
	}

	type outcome struct {
		res SelfBackupResult
		err error
	}
	first := make(chan outcome, 1)
	go func() {
		res, err := h.ops.SelfBackup(context.Background(), "local")
		first <- outcome{res, err}
	}()
	<-entered

	_, err := h.ops.SelfBackup(context.Background(), "local")
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second self-backup error = %v, want it to say one is already running", err)
	}
	entries, _ := os.ReadDir(h.ops.WorkRoot)
	if len(entries) != 1 {
		t.Errorf("work folder holds %d entries during the first run, want just its own staging folder", len(entries))
	}
	close(release)
	got := <-first
	if got.err != nil || !got.res.Verified {
		t.Fatalf("first self-backup = %+v, %v; want verified", got.res, got.err)
	}
	if entries, _ := os.ReadDir(h.ops.WorkRoot); len(entries) != 0 {
		t.Errorf("staging left behind: %v", entries)
	}
}

// TestSelfBackupMustContainDatabase: a snapshot that reads back but
// lacks ballast.db is not verified.
func TestSelfBackupMustContainDatabase(t *testing.T) {
	t.Setenv("STUB_LS_OMIT_DB", "1")
	h := newHarness(t)
	h.addDestination("local")
	h.ops.WorkRoot = filepath.Join(t.TempDir(), "work")
	res, err := h.ops.SelfBackup(context.Background(), "local")
	if err == nil || !strings.Contains(err.Error(), "ballast.db") {
		t.Fatalf("err = %v, want it to name the missing ballast.db", err)
	}
	if res.Verified {
		t.Fatal("a snapshot without the database must not be verified")
	}
}

// legacySelfNamedService stores a service called ballast-self the way
// v0.5 allowed (PutService now refuses that name).
func legacySelfNamedService(t *testing.T, h *harness) {
	t.Helper()
	h.addDestination("local")
	if err := inventory.PutService(h.ops.DB, inventory.Service{Name: "old-name", CatalogEntry: "folder", Destination: "local", Params: map[string]string{"path": "/x"}}); err != nil {
		t.Fatal(err)
	}
	// A v0.5 database can hold this row; it has no other table rows to move.
	if _, err := h.ops.DB.Exec(`DELETE FROM service_targets WHERE service = 'old-name'`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ops.DB.Exec(`UPDATE services SET name = ? WHERE name = 'old-name'`, SelfBackupService); err != nil {
		t.Fatal(err)
	}
}

// TestReservedNameConflict: a service left over from v0.5 that is named
// like the self-backup is detected, with a plain-English fix; doctor
// fails on it, and a clean database reports nothing.
func TestReservedNameConflict(t *testing.T) {
	h := newHarness(t)
	if err := h.ops.ReservedNameConflict(); err != nil {
		t.Fatalf("clean database: %v", err)
	}
	legacySelfNamedService(t, h)
	err := h.ops.ReservedNameConflict()
	if err == nil || !strings.Contains(err.Error(), "ballast-self") || !strings.Contains(err.Error(), "another name") {
		t.Fatalf("conflict = %v, want a message naming ballast-self and telling the user to use another name", err)
	}
	h.ops.ResticBin = h.resticBin
	rep, derr := h.ops.Doctor(context.Background())
	if derr != nil {
		t.Fatal(derr)
	}
	found := false
	for _, c := range rep.Checks {
		if c.Name == "ballast-self" && c.Area == "service" {
			found = true
			if c.Status != DoctorFail {
				t.Errorf("doctor status = %s, want fail", c.Status)
			}
		}
	}
	if !found {
		t.Fatalf("doctor has no check for the reserved name: %+v", rep.Checks)
	}
}

// otherProcessOps builds a second Ops over the same database, work
// folder and lock directory, like a separate CLI or run-due process.
func otherProcessOps(h *harness) *Ops {
	o := &Ops{
		DB:             h.ops.DB,
		Catalog:        h.ops.Catalog,
		Coord:          &scheduler.Coordinator{DB: h.ops.DB, Locks: h.locks, Runner: h.runner, LockWait: -1},
		Repo:           h.ops.Repo,
		CredentialsDir: h.ops.CredentialsDir,
		WorkRoot:       h.ops.WorkRoot,
	}
	return o
}

// TestSelfBackupExcludesOtherProcesses: a self-backup in another
// process (its own Ops, same lock directory) is refused while one is
// uploading, and does not wipe the running one's staging folder.
func TestSelfBackupExcludesOtherProcesses(t *testing.T) {
	h := newHarness(t)
	h.addDestination("local")
	h.ops.WorkRoot = filepath.Join(t.TempDir(), "work")
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	h.ops.HoldRepository = func(context.Context, string, string) (func(), error) {
		once.Do(func() {
			close(entered)
			<-release
		})
		return func() {}, nil
	}
	first := make(chan error, 1)
	go func() {
		_, err := h.ops.SelfBackup(context.Background(), "local")
		first <- err
	}()
	<-entered

	other := otherProcessOps(h)
	if _, err := other.SelfBackup(context.Background(), "local"); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("self-backup from another process = %v, want refusal", err)
	}
	if entries, _ := os.ReadDir(h.ops.WorkRoot); len(entries) != 1 {
		t.Errorf("the running self-backup's staging folder was wiped: %d entries", len(entries))
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatalf("first self-backup: %v", err)
	}
}

// TestSelfBackupRefusedWhileScheduledRunHoldsLock: the scheduler's
// lock for the self-backup also blocks a manual one.
func TestSelfBackupRefusedWhileScheduledRunHoldsLock(t *testing.T) {
	h := newHarness(t)
	h.addDestination("local")
	h.ops.WorkRoot = filepath.Join(t.TempDir(), "work")
	h.holdRunLock(t, SelfBackupService)
	stale := filepath.Join(h.ops.WorkRoot, selfStagePrefix+"live")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ops.SelfBackup(context.Background(), "local"); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("err = %v, want refusal", err)
	}
	if _, err := os.Stat(stale); err != nil {
		t.Fatalf("a folder was cleared without holding the lock: %v", err)
	}
}

// TestSelfBackupClearsStaleStagingOnlyUnderLock: leftovers of a crashed
// run are removed once the lock is held.
func TestSelfBackupClearsStaleStagingOnlyUnderLock(t *testing.T) {
	h := newHarness(t)
	h.addDestination("local")
	h.ops.WorkRoot = filepath.Join(t.TempDir(), "work")
	stale := filepath.Join(h.ops.WorkRoot, selfStagePrefix+"old")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ops.SelfBackup(context.Background(), "local"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale staging folder not cleared: %v", err)
	}
}

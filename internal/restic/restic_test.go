package restic

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/springledev/keel/runner"
)

// resticStub is a fake restic CLI. It appends every invocation's argv
// to $STUB_LOG (one line per call), answers each subcommand with a
// golden fixture from $STUB_FIXTURE_DIR, and simulates failures via
// STUB_FAIL_CMD (subcommand name) and STUB_FAIL_CODE (exit status) or
// STUB_GARBAGE_CMD (emit unparseable output instead of the fixture).
// The tests are hermetic: no restic installation is required.
const resticStub = `#!/bin/sh
printf '%s\n' "$*" >> "$STUB_LOG"
cmd="$1"
if [ -n "$STUB_FAIL_CODE" ] && [ "$cmd" = "$STUB_FAIL_CMD" ]; then
  echo "Fatal: stub simulated $cmd failure" >&2
  exit "$STUB_FAIL_CODE"
fi
if [ -n "$STUB_GARBAGE_CMD" ] && [ "$cmd" = "$STUB_GARBAGE_CMD" ]; then
  echo "this is not the pinned json format"
  exit 0
fi
case "$cmd" in
  init) exit 0 ;;
  backup) cat "$STUB_FIXTURE_DIR/backup-events.jsonl" ;;
  snapshots) cat "$STUB_FIXTURE_DIR/snapshots.json" ;;
  ls) cat "$STUB_FIXTURE_DIR/ls.jsonl" ;;
  dump)
    # args: dump [--no-lock] <id> <path>
    for a in "$@"; do last="$a"; done
    printf 'content of %s\n' "$last"
    ;;
  restore) exit 0 ;;
  forget) cat "$STUB_FIXTURE_DIR/forget-keep-remove.json" ;;
  prune) exit 0 ;;
  version) cat "$STUB_FIXTURE_DIR/version.txt" ;;
  *) echo "unknown stub command: $cmd" >&2; exit 2 ;;
esac
exit 0
`

// stubEnv holds the stub's per-test wiring.
type stubEnv struct {
	bin     string
	logPath string
}

// newResticStub installs the restic stub and returns its path plus the
// invocation log file. Extra environment (STUB_FAIL_CMD=..., ...) is
// added by the test through withEnv.
func newResticStub(t *testing.T) stubEnv {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "restic.log")
	bin := filepath.Join(dir, "restic-stub")
	if err := os.WriteFile(bin, []byte(resticStub), 0o755); err != nil {
		t.Fatalf("writing stub: %v", err)
	}
	return stubEnv{bin: bin, logPath: logPath}
}

// openStub opens a repository against the stub with the golden
// fixtures wired in.
func (s stubEnv) openStub(t *testing.T) *Repository {
	t.Helper()
	return s.openStubFixtures(t, "testdata")
}

// openStubFixtures opens a repository against the stub serving
// fixtures from the named directory (e.g. "testdata/empty" for the
// empty-snapshot fixtures).
func (s stubEnv) openStubFixtures(t *testing.T, fixtureDir string) *Repository {
	t.Helper()
	fixtures, err := filepath.Abs(fixtureDir)
	if err != nil {
		t.Fatalf("resolving %s: %v", fixtureDir, err)
	}
	passFile := filepath.Join(t.TempDir(), "repo-password")
	if err := os.WriteFile(passFile, []byte("stub-password\n"), 0o600); err != nil {
		t.Fatalf("writing password file: %v", err)
	}
	repo, err := Open("/srv/backups", Options{
		Runner: &runner.Runner{BaseEnv: append(os.Environ(),
			"STUB_LOG="+s.logPath,
			"STUB_FIXTURE_DIR="+fixtures,
		)},
		PasswordFile: passFile,
		ResticBin:    s.bin,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return repo
}

// withEnv returns a copy of the repository whose runner carries extra
// stub-control environment (fail/garbage simulation).
func (s stubEnv) withEnv(t *testing.T, repo *Repository, extra ...string) *Repository {
	t.Helper()
	cp := *repo
	r := *repo.Runner
	r.BaseEnv = append(append([]string(nil), repo.Runner.BaseEnv...), extra...)
	cp.Runner = &r
	return &cp
}

// invocations returns the recorded restic argv lines.
func (s stubEnv) invocations(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(s.logPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("reading stub log: %v", err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

func TestOpenValidates(t *testing.T) {
	ok := &runner.Runner{}
	tests := []struct {
		name    string
		loc     string
		opts    Options
		wantErr string
	}{
		{name: "empty location", loc: "", opts: Options{Runner: ok}, wantErr: "location must not be empty"},
		{name: "nil runner", loc: "/srv/backups", opts: Options{}, wantErr: "command runner is required"},
		{name: "relative password file", loc: "/srv/backups", opts: Options{Runner: ok, PasswordFile: "repo-password"}, wantErr: "must be an absolute path"},
		{name: "malformed env", loc: "/srv/backups", opts: Options{Runner: ok, Env: []string{"NO-EQUALS"}}, wantErr: "KEY=VALUE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Open(tt.loc, tt.opts)
			if err == nil {
				t.Fatal("Open = nil error, want failure")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want substring %q", err, tt.wantErr)
			}
		})
	}

	if _, err := Open("/srv/backups", Options{Runner: ok}); err != nil {
		t.Errorf("Open with valid options: %v", err)
	}
}

// TestOpenEnforcesPasswordFilePermissions: the repository password
// file must exist, be regular, and be root-only 0600 (spec §5.3) —
// the same rules runner.FileEnv enforces for backend credentials.
// A world-readable password is a loud error, never a quiet proceed.
func TestOpenEnforcesPasswordFilePermissions(t *testing.T) {
	ok := &runner.Runner{}
	dir := t.TempDir()

	write := func(name string, perm os.FileMode) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("pw\n"), perm); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
		return p
	}

	tests := []struct {
		name    string
		path    string
		wantErr string
	}{
		{name: "missing file", path: filepath.Join(dir, "nope"), wantErr: "no such file"},
		{name: "world-readable", path: write("pw-0644", 0o644), wantErr: "group and other access is not allowed"},
		{name: "group-readable", path: write("pw-0640", 0o640), wantErr: "group and other access is not allowed"},
		{name: "directory", path: dir, wantErr: "not a regular file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Open("/srv/backups", Options{Runner: ok, PasswordFile: tt.path})
			if err == nil {
				t.Fatal("Open = nil error, want refusal")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want substring %q", err, tt.wantErr)
			}
		})
	}

	// 0600 is accepted; the error names the chmod fix.
	good := write("pw-0600", 0o600)
	if _, err := Open("/srv/backups", Options{Runner: ok, PasswordFile: good}); err != nil {
		t.Errorf("Open with 0600 password file: %v", err)
	}
	_, err := Open("/srv/backups", Options{Runner: ok, PasswordFile: write("pw-0644b", 0o644)})
	if err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("error = %v, want the chmod 600 hint", err)
	}
}

// TestStructLiteralDoesNotPanic: Repository's fields are exported, so
// a caller can bypass Open's validation with a struct literal. That
// must be a loud error pointing at Open — never a nil-pointer panic
// mid-backup.
func TestStructLiteralDoesNotPanic(t *testing.T) {
	literal := &Repository{Location: "/srv/backups"}
	if _, err := literal.Snapshots(context.Background(), SnapshotFilter{}); err == nil {
		t.Fatal("Snapshots on a runner-less literal = nil error, want refusal")
	} else if !strings.Contains(err.Error(), "restic.Open") {
		t.Errorf("error = %q, want it to point at restic.Open", err)
	}

	var buf bytes.Buffer
	if err := literal.Dump(context.Background(), "latest", "/a", &buf); err == nil {
		t.Fatal("Dump on a runner-less literal = nil error, want refusal")
	} else if !strings.Contains(err.Error(), "restic.Open") {
		t.Errorf("error = %q, want it to point at restic.Open", err)
	}

	empty := &Repository{}
	if _, err := empty.Version(context.Background()); err == nil {
		t.Fatal("Version on an empty literal = nil error, want refusal")
	}
}

// TestInitBuildsCommand pins the init argv: a bare `init`, with the
// repository location traveling in the environment (never argv).
func TestInitBuildsCommand(t *testing.T) {
	s := newResticStub(t)
	repo := s.openStub(t)
	if err := repo.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	log := s.invocations(t)
	if len(log) != 1 || log[0] != "init" {
		t.Errorf("invocations = %v, want [init]", log)
	}
}

// TestCommandEnvironment: the repository location and password file
// travel in the environment (never argv), so a process listing cannot
// leak them (spec: secrets never in logs).
func TestCommandEnvironment(t *testing.T) {
	s := newResticStub(t)
	repo := s.openStub(t)
	if _, err := repo.Version(context.Background()); err != nil {
		t.Fatalf("Version: %v", err)
	}
	log := s.invocations(t)
	if len(log) != 1 {
		t.Fatalf("invocations = %v, want exactly one", log)
	}
	// Argv must be just `version` — no location, no password.
	if log[0] != "version" {
		t.Errorf("argv = %q, want %q (repository credentials stay in the environment)", log[0], "version")
	}
	if !strings.HasPrefix(repo.Location, "/") {
		t.Errorf("Location = %q", repo.Location)
	}
	if repo.PasswordFile == "" {
		t.Error("PasswordFile must be set on the stub repository")
	}
}

// TestBackupBuildsCommand pins the backup argv: --json (pinned
// format), tags and host as flags, absolute paths as operands.
func TestBackupBuildsCommand(t *testing.T) {
	s := newResticStub(t)
	repo := s.openStub(t)
	dir := t.TempDir()

	res, err := repo.Backup(context.Background(), BackupOptions{
		Paths: []string{dir, dir + "/artifact.sql"},
		Tags:  []string{"nightly", "postgres"},
		Host:  "homelab",
	})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if res.SnapshotID == "" {
		t.Error("SnapshotID must come from the summary")
	}
	if res.FilesNew != 2 {
		t.Errorf("FilesNew = %d, want 2 (from the fixture summary)", res.FilesNew)
	}

	log := s.invocations(t)
	if len(log) != 1 {
		t.Fatalf("invocations = %v, want one", log)
	}
	wantParts := []string{"backup", "--json", "--host", "homelab", "--tag", "nightly", "--tag", "postgres", dir}
	for _, part := range wantParts {
		if !strings.Contains(log[0], part) {
			t.Errorf("argv = %q, want it to contain %q", log[0], part)
		}
	}
	// spec §8: pinned to --json; text tables (--compact) are never parsed.
	if strings.Contains(log[0], "--compact") {
		t.Errorf("argv = %q, must never use --compact (no Paths column, spec §8)", log[0])
	}
}

func TestBackupRejectsBadPaths(t *testing.T) {
	s := newResticStub(t)
	repo := s.openStub(t)
	if _, err := repo.Backup(context.Background(), BackupOptions{}); err == nil {
		t.Error("Backup with no paths = nil error, want failure")
	}
	if _, err := repo.Backup(context.Background(), BackupOptions{Paths: []string{"relative/path"}}); err == nil {
		t.Error("Backup with a relative path = nil error, want failure")
	}
	if len(s.invocations(t)) != 0 {
		t.Errorf("invocations = %v, want none (validation happens before restic runs)", s.invocations(t))
	}
}

// TestSnapshotsPinsJSONNotCompact (spec §8): the wrapper lists
// snapshots with --json and never --compact, and maps the filter onto
// restic's --host/--path/--tag/--latest flags.
func TestSnapshotsPinsJSONNotCompact(t *testing.T) {
	s := newResticStub(t)
	repo := s.openStub(t)
	dir := t.TempDir()

	snaps, err := repo.Snapshots(context.Background(), SnapshotFilter{
		Host:   "homelab",
		Paths:  []string{dir},
		Tags:   []string{"nightly"},
		Latest: 1,
	})
	if err != nil {
		t.Fatalf("Snapshots: %v", err)
	}
	if len(snaps) != 2 {
		t.Errorf("got %d snapshots, want 2 from the fixture", len(snaps))
	}
	if snaps[0].Paths[0] != "/tmp/ballast-test/src2" {
		t.Errorf("Paths lost in parsing: %v (spec §8 needs path-sets)", snaps[0].Paths)
	}

	log := s.invocations(t)
	if len(log) != 1 {
		t.Fatalf("invocations = %v, want one", log)
	}
	for _, part := range []string{"snapshots", "--json", "--no-lock", "--host", "homelab", "--path", dir, "--tag", "nightly", "--latest", "1"} {
		if !strings.Contains(log[0], part) {
			t.Errorf("argv = %q, want it to contain %q", log[0], part)
		}
	}
	if strings.Contains(log[0], "--compact") {
		t.Errorf("argv = %q, must never use --compact (drops the Paths column, spec §8)", log[0])
	}
}

func TestSnapshotsRejectsRelativeFilterPath(t *testing.T) {
	s := newResticStub(t)
	repo := s.openStub(t)
	if _, err := repo.Snapshots(context.Background(), SnapshotFilter{Paths: []string{"srv/data"}}); err == nil {
		t.Error("relative filter path = nil error, want failure")
	}
	if _, err := repo.Snapshots(context.Background(), SnapshotFilter{Latest: -1}); err == nil {
		t.Error("negative Latest = nil error, want failure")
	}
}

// TestLSAndDumpReadBack pins the read-back primitives (spec §7.3):
// ls lists the snapshot without locking the repository (a verification
// must never block a concurrent backup), dump streams the file.
func TestLSAndDumpReadBack(t *testing.T) {
	s := newResticStub(t)
	repo := s.openStub(t)

	entries, err := repo.LS(context.Background(), "latest", "/tmp/ballast-test/src2")
	if err != nil {
		t.Fatalf("LS: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("LS returned no entries, want the fixture listing")
	}

	var buf bytes.Buffer
	if err := repo.Dump(context.Background(), "latest", "/tmp/ballast-test/src2/one.txt", &buf); err != nil {
		t.Fatalf("Dump: %v", err)
	}
	if !strings.Contains(buf.String(), "content of /tmp/ballast-test/src2/one.txt") {
		t.Errorf("dumped %q, want the stub's file content", buf.String())
	}

	log := s.invocations(t)
	if len(log) != 2 {
		t.Fatalf("invocations = %v, want ls + dump", log)
	}
	if !strings.Contains(log[0], "ls --json --no-lock latest") {
		t.Errorf("ls argv = %q, want --json and --no-lock", log[0])
	}
	if !strings.Contains(log[1], "dump --no-lock latest") {
		t.Errorf("dump argv = %q, want --no-lock", log[1])
	}
}

func TestLSDumpValidateInput(t *testing.T) {
	s := newResticStub(t)
	repo := s.openStub(t)
	if _, err := repo.LS(context.Background(), "", ""); err == nil {
		t.Error("LS with empty snapshot = nil error, want failure")
	}
	if _, err := repo.LS(context.Background(), "latest", "relative"); err == nil {
		t.Error("LS with relative prefix = nil error, want failure")
	}
	var buf bytes.Buffer
	if err := repo.Dump(context.Background(), "latest", "relative", &buf); err == nil {
		t.Error("Dump with relative path = nil error, want failure")
	}
	if err := repo.Dump(context.Background(), "", "/a", &buf); err == nil {
		t.Error("Dump with empty snapshot = nil error, want failure")
	}
}

// TestRestore builds the restore argv and creates the target.
func TestRestore(t *testing.T) {
	s := newResticStub(t)
	repo := s.openStub(t)
	target := filepath.Join(t.TempDir(), "restore-here")

	if err := repo.Restore(context.Background(), RestoreOptions{
		SnapshotID: "abc123",
		TargetDir:  target,
		Paths:      []string{"/srv/data/file.txt"},
	}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	info, err := os.Stat(target)
	if err != nil || !info.IsDir() {
		t.Fatalf("target dir not created: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("target permissions = %04o, want 0700 (restored data may be sensitive)", perm)
	}

	log := s.invocations(t)
	if len(log) != 1 {
		t.Fatalf("invocations = %v, want one", log)
	}
	for _, part := range []string{"restore", "--no-lock", "abc123", "--target", target, "--include", "/srv/data/file.txt"} {
		if !strings.Contains(log[0], part) {
			t.Errorf("argv = %q, want it to contain %q", log[0], part)
		}
	}
}

func TestRestoreValidates(t *testing.T) {
	s := newResticStub(t)
	repo := s.openStub(t)
	dir := t.TempDir()
	tests := []struct {
		name    string
		opts    RestoreOptions
		wantErr string
	}{
		{name: "empty snapshot", opts: RestoreOptions{TargetDir: dir}, wantErr: "snapshot ID must not be empty"},
		{name: "empty target", opts: RestoreOptions{SnapshotID: "abc"}, wantErr: "target directory is required"},
		{name: "relative target", opts: RestoreOptions{SnapshotID: "abc", TargetDir: "some/where"}, wantErr: "must be absolute"},
		{name: "relative include", opts: RestoreOptions{SnapshotID: "abc", TargetDir: dir, Paths: []string{"file"}}, wantErr: "must be absolute"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := repo.Restore(context.Background(), tt.opts)
			if err == nil {
				t.Fatal("Restore = nil error, want failure")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want substring %q", err, tt.wantErr)
			}
		})
	}
}

// TestForgetBuildsPolicy maps the retention policy onto restic's
// --keep-* flags and reports kept/removed IDs.
func TestForgetBuildsPolicy(t *testing.T) {
	s := newResticStub(t)
	repo := s.openStub(t)

	res, err := repo.Forget(context.Background(), DefaultRetention(), ForgetOptions{})
	if err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if len(res.Kept) != 1 || len(res.Removed) != 1 {
		t.Errorf("result = %+v, want 1 kept and 1 removed from the fixture", res)
	}

	log := s.invocations(t)
	if len(log) != 1 {
		t.Fatalf("invocations = %v, want one", log)
	}
	// DefaultRetention is 7d/4w/6m (spec §5.2).
	for _, part := range []string{"forget", "--json", "--keep-daily", "7", "--keep-weekly", "4", "--keep-monthly", "6"} {
		if !strings.Contains(log[0], part) {
			t.Errorf("argv = %q, want it to contain %q", log[0], part)
		}
	}
	if strings.Contains(log[0], "--keep-last") {
		t.Errorf("argv = %q, want unset keep rules omitted", log[0])
	}
}

// TestForgetRefusesEmptyPolicy: a policy that keeps nothing would
// delete every snapshot — never what a retention rule means.
func TestForgetRefusesEmptyPolicy(t *testing.T) {
	s := newResticStub(t)
	repo := s.openStub(t)
	if _, err := repo.Forget(context.Background(), RetentionPolicy{}, ForgetOptions{}); err == nil {
		t.Fatal("Forget with empty policy = nil error, want refusal")
	} else if !strings.Contains(err.Error(), "delete every snapshot") {
		t.Errorf("error = %q, want the plain-English refusal", err)
	}
	if len(s.invocations(t)) != 0 {
		t.Errorf("invocations = %v, want none", s.invocations(t))
	}
}

func TestPrune(t *testing.T) {
	s := newResticStub(t)
	repo := s.openStub(t)
	if err := repo.Prune(context.Background()); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	log := s.invocations(t)
	if len(log) != 1 || log[0] != "prune" {
		t.Errorf("invocations = %v, want [prune]", log)
	}
}

// TestDumpFailurePropagates: a failing dump (e.g. the file vanished
// from the snapshot) fails loudly with the restic dump op named and
// the runner's exit detail preserved.
func TestDumpFailurePropagates(t *testing.T) {
	s := newResticStub(t)
	repo := s.withEnv(t, s.openStub(t), "STUB_FAIL_CMD=dump", "STUB_FAIL_CODE=1")
	var buf bytes.Buffer
	err := repo.Dump(context.Background(), "latest", "/tmp/ballast-test/src2/gone.txt", &buf)
	if err == nil {
		t.Fatal("Dump = nil error, want the simulated failure")
	}
	if !strings.Contains(err.Error(), "restic dump") {
		t.Errorf("error = %q, want the restic dump op named", err)
	}
	var ee *runner.ExitError
	if !errors.As(err, &ee) {
		t.Errorf("error = %v, want the runner.ExitError preserved in the chain", err)
	}
}

// TestReadBackEmptySnapshot: a snapshot whose path-set holds no
// regular files still verifies what it can — the listing proves the
// snapshot exists and its trees parse — and returns zero samples
// WITHOUT running a dump, so the verifier can tell "read back" from
// "listed only" (gate: unverifiable must be distinguishable).
func TestReadBackEmptySnapshot(t *testing.T) {
	s := newResticStub(t)
	repo := s.openStubFixtures(t, "testdata/empty")

	res, err := repo.ReadBack(context.Background(), "latest")
	if err != nil {
		t.Fatalf("ReadBack: %v", err)
	}
	if res.Snapshot.ShortID != "bcedfebd" {
		t.Errorf("Snapshot = %+v, want the empty fixture snapshot", res.Snapshot)
	}
	if len(res.Samples) != 0 {
		t.Errorf("Samples = %+v, want none: the path-set holds only directories", res.Samples)
	}

	log := s.invocations(t)
	if len(log) != 2 {
		t.Fatalf("invocations = %v, want ls + snapshots and no dump", log)
	}
	for _, line := range log {
		if strings.HasPrefix(line, "dump ") {
			t.Errorf("invocations = %v, want no dump for a file-less snapshot", log)
		}
	}
}

// TestReadBackSamplesOneFilePerPathSet: ls + one dump per path-set
// (spec §7.3), with the concrete snapshot attributed.
func TestReadBackSamplesOneFilePerPathSet(t *testing.T) {
	s := newResticStub(t)
	repo := s.openStub(t)

	res, err := repo.ReadBack(context.Background(), "latest")
	if err != nil {
		t.Fatalf("ReadBack: %v", err)
	}
	if res.Snapshot.ID == "" {
		t.Error("ReadBack must attribute the concrete snapshot")
	}
	if len(res.Samples) != 1 {
		t.Fatalf("Samples = %+v, want one sample (the fixture has one path-set with files)", res.Samples)
	}
	if res.Samples[0].Path != "/tmp/ballast-test/src2/one.txt" {
		t.Errorf("sampled %q, want the first regular file under the path-set", res.Samples[0].Path)
	}
	if res.Samples[0].Bytes <= 0 {
		t.Errorf("sampled %d bytes, want the dumped content", res.Samples[0].Bytes)
	}

	log := s.invocations(t)
	// snapshots + ls + dump.
	if len(log) != 3 {
		t.Fatalf("invocations = %v, want snapshots + ls + dump", log)
	}
	if !strings.HasPrefix(log[2], "dump ") {
		t.Errorf("third invocation = %q, want the dump", log[2])
	}
}

// TestExitSignals: the exit-10 ("no repository yet") and exit-12
// ("wrong password") signals are recognized through the
// explain()-wrapped, plain-English error a real caller actually sees
// (not just the raw *runner.ExitError), and never for each other or
// an unrelated failure.
func TestExitSignals(t *testing.T) {
	tests := []struct {
		code            string
		wantMissing     bool
		wantWrongPasswd bool
	}{
		{code: "10", wantMissing: true},
		{code: "12", wantWrongPasswd: true},
		{code: "1"},
	}
	for _, tt := range tests {
		t.Run("exit "+tt.code, func(t *testing.T) {
			s := newResticStub(t)
			repo := s.withEnv(t, s.openStub(t), "STUB_FAIL_CMD=snapshots", "STUB_FAIL_CODE="+tt.code)
			_, err := repo.Snapshots(context.Background(), SnapshotFilter{})
			if err == nil {
				t.Fatal("Snapshots = nil error, want the simulated failure")
			}
			if got := IsRepoMissing(err); got != tt.wantMissing {
				t.Errorf("IsRepoMissing(%v) = %v, want %v", err, got, tt.wantMissing)
			}
			if got := IsWrongPassword(err); got != tt.wantWrongPasswd {
				t.Errorf("IsWrongPassword(%v) = %v, want %v", err, got, tt.wantWrongPasswd)
			}
		})
	}
}

// TestVersionTooOld: releases before MinimumVersion cannot signal
// "no repository yet" distinctly, so they are refused for setup.
func TestVersionTooOld(t *testing.T) {
	tests := []struct {
		number string
		want   bool
	}{
		{"0.16.5", true},
		{"0.9.6", true},
		{"0.17.0", false},
		{"0.17.3", false},
		{"0.18.1", false},
		{"1.0.0", false},
		{"garbage", false},
	}
	for _, tt := range tests {
		t.Run(tt.number, func(t *testing.T) {
			if got := (Version{Number: tt.number}).TooOld(); got != tt.want {
				t.Errorf("Version{%q}.TooOld() = %v, want %v", tt.number, got, tt.want)
			}
		})
	}
	if msg := (Version{Number: "0.16.5"}).TooOldError().Error(); !strings.Contains(msg, MinimumVersion) {
		t.Errorf("TooOldError = %q, want it to name the minimum version", msg)
	}
}

// TestExitCodeTranslation: restic's documented exit statuses become
// plain-English causes (spec §10 beginner requirement) while the
// original error stays in the chain.
func TestExitCodeTranslation(t *testing.T) {
	tests := []struct {
		code int
		want string
	}{
		{code: 10, want: "no backup repository exists"},
		{code: 11, want: "another restic process"},
		{code: 12, want: "repository password"},
		{code: 1, want: "restic snapshots for repository"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			s := newResticStub(t)
			repo := s.withEnv(t, s.openStub(t),
				"STUB_FAIL_CMD=snapshots", "STUB_FAIL_CODE="+strconv.Itoa(tt.code))
			_, err := repo.Snapshots(context.Background(), SnapshotFilter{})
			if err == nil {
				t.Fatal("Snapshots = nil error, want the simulated failure")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want substring %q", err, tt.want)
			}
			var ee *runner.ExitError
			if !errors.As(err, &ee) {
				t.Errorf("error = %v, want the runner.ExitError preserved in the chain", err)
			}
		})
	}
}

// TestExplainMissingBinary: when restic itself is not installed (or
// not on $PATH), the error must say so in plain English (spec §10
// beginner requirement) instead of surfacing a raw "executable file
// not found" — this is what a brand-new install hits before ever
// having a real repository to name.
func TestExplainMissingBinary(t *testing.T) {
	// exec.Command resolves an unqualified name (like "restic") via
	// the REAL process environment's $PATH at LookPath time, not via
	// Runner.BaseEnv (BaseEnv only reaches the child once already
	// started) — t.Setenv on the process's actual PATH is the only way
	// to hermetically simulate "not installed", regardless of whether
	// this machine happens to have a real restic binary somewhere.
	t.Setenv("PATH", t.TempDir())
	repo, err := Open("/srv/backups", Options{Runner: &runner.Runner{}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_, err = repo.Snapshots(context.Background(), SnapshotFilter{})
	if err == nil {
		t.Fatal("Snapshots = nil error, want a missing-binary failure")
	}
	if !strings.Contains(err.Error(), "restic is not installed") {
		t.Errorf("error = %q, want it to say restic is not installed", err)
	}
}

// TestParseSurfacesOnGarbage: stub output that is not the pinned
// format fails safe (spec §8) — loudly, not as an empty listing.
func TestParseSurfacesOnGarbage(t *testing.T) {
	s := newResticStub(t)
	repo := s.withEnv(t, s.openStub(t), "STUB_GARBAGE_CMD=snapshots")
	_, err := repo.Snapshots(context.Background(), SnapshotFilter{})
	if err == nil {
		t.Fatal("Snapshots = nil error, want a parse failure")
	}
	var pe *ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("error = %v, want *ParseError", err)
	}
	if !strings.Contains(err.Error(), PinnedVersion) {
		t.Errorf("error = %q, want it to name the pinned version", err)
	}
}

// TestVersionAndWarning: the version line parses and the pinned
// series is recognized.
func TestVersionAndWarning(t *testing.T) {
	s := newResticStub(t)
	repo := s.openStub(t)
	v, err := repo.Version(context.Background())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if v.Number != "0.17.3" {
		t.Errorf("Number = %q, want 0.17.3", v.Number)
	}
	if w := v.Warning(); w != "" {
		t.Errorf("Warning = %q, want empty for the stub's pinned release", w)
	}
}

// TestExtendedOptionsEmittedBeforeSubcommand: restic's `-o key=value`
// backend options must precede the subcommand on the command line
// (restic itself only accepts them there), and every configured option
// is emitted. Built directly via the unexported command() builder
// (this test file is in package restic) rather than through the stub,
// since the stub's argv dispatch only understands bare subcommands.
func TestExtendedOptionsEmittedBeforeSubcommand(t *testing.T) {
	repo := &Repository{
		Location:        "/srv/backups",
		Runner:          &runner.Runner{},
		ExtendedOptions: []string{"sftp.args=-i /key", "b2.key=val"},
	}
	cmd := repo.command("init")
	want := []string{"-o", "sftp.args=-i /key", "-o", "b2.key=val", "init"}
	if strings.Join(cmd.Args, " ") != strings.Join(want, " ") {
		t.Errorf("argv = %v, want %v (extended options before the subcommand)", cmd.Args, want)
	}
}

// TestOpenRejectsInvalidExtendedOption: an option without "key=value"
// shape is refused up front, before restic ever runs.
func TestOpenRejectsInvalidExtendedOption(t *testing.T) {
	ok := &runner.Runner{}
	_, err := Open("/srv/backups", Options{Runner: ok, ExtendedOptions: []string{"no-equals-sign"}})
	if err == nil {
		t.Fatal("Open with a malformed extended option = nil error, want failure")
	}
	if !strings.Contains(err.Error(), "key=value") {
		t.Errorf("error = %q, want it to name the key=value shape", err)
	}
}

// TestBackendHint pins the well-known storage-provider phrases that get
// a plain-English hint prepended, and confirms unrelated stderr gets
// none (spec §10 beginner requirement, spec §8: never changes the
// underlying decision, only adds explanatory text).
func TestBackendHint(t *testing.T) {
	tests := []struct {
		name   string
		stderr string
		want   string
	}{
		{"invalid access key id", "Fatal: InvalidAccessKeyId: the key does not exist", "does not recognize the access key ID"},
		{"signature mismatch", "Fatal: SignatureDoesNotMatch", "secret access key (application key) is wrong"},
		{"no such bucket", "Fatal: NoSuchBucket: the bucket does not exist", "bucket does not exist"},
		{"publickey rejected", "Permission denied (publickey).", "rejected the SSH key"},
		{"no match", "Fatal: something else entirely went wrong", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := backendHint(tt.stderr); got == "" && tt.want != "" {
				t.Errorf("backendHint(%q) = %q, want it to contain %q", tt.stderr, got, tt.want)
			} else if tt.want == "" && got != "" {
				t.Errorf("backendHint(%q) = %q, want empty (no match)", tt.stderr, got)
			} else if tt.want != "" && !strings.Contains(got, tt.want) {
				t.Errorf("backendHint(%q) = %q, want substring %q", tt.stderr, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Integration tests against a real restic binary and a local-disk
// repository (spec §13: "restic: Test repo on local disk … snapshot
// read-back assertions"). They skip when restic is not installed, so
// `go test ./...` stays green everywhere; CI with restic present runs
// the full round-trip.
// ---------------------------------------------------------------------------

// requireRestic returns a real restic binary or skips the test.
func requireRestic(t *testing.T) string {
	t.Helper()
	if bin := os.Getenv("BALLAST_TEST_RESTIC"); bin != "" {
		return bin
	}
	bin, err := exec.LookPath("restic")
	if err != nil {
		t.Skip("restic not installed; set BALLAST_TEST_RESTIC or install restic to run integration tests")
	}
	return bin
}

// TestIntegrationLocalRepo is the full round-trip against a local
// test repository: init, backup, snapshots, ls, dump (read-back),
// restore, forget, prune, read-back.
func TestIntegrationLocalRepo(t *testing.T) {
	bin := requireRestic(t)
	ctx := context.Background()

	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("hello file one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "b.txt"), []byte("file two content\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	passFile := filepath.Join(t.TempDir(), "repo-password")
	if err := os.WriteFile(passFile, []byte("integration-test-password\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repoDir := filepath.Join(t.TempDir(), "repo")

	repo, err := Open(repoDir, Options{
		Runner:       &runner.Runner{},
		PasswordFile: passFile,
		ResticBin:    bin,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if err := repo.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	first, err := repo.Backup(ctx, BackupOptions{
		Paths: []string{src},
		Tags:  []string{"nightly"},
		Host:  "ballast-test",
	})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if first.SnapshotID == "" {
		t.Fatal("Backup reported no snapshot ID")
	}
	if first.FilesNew != 2 {
		t.Errorf("FilesNew = %d, want 2", first.FilesNew)
	}

	// A second backup is incremental: files_unmodified covers both.
	second, err := repo.Backup(ctx, BackupOptions{
		Paths: []string{src},
		Tags:  []string{"nightly"},
		Host:  "ballast-test",
	})
	if err != nil {
		t.Fatalf("second Backup: %v", err)
	}
	if second.FilesUnmodified != 2 {
		t.Errorf("FilesUnmodified = %d, want 2 on the incremental run", second.FilesUnmodified)
	}

	snaps, err := repo.Snapshots(ctx, SnapshotFilter{Host: "ballast-test", Paths: []string{src}})
	if err != nil {
		t.Fatalf("Snapshots: %v", err)
	}
	if len(snaps) != 2 {
		t.Fatalf("got %d snapshots, want 2", len(snaps))
	}
	if len(snaps[0].Paths) != 1 || snaps[0].Paths[0] != filepath.Clean(src) {
		t.Errorf("snapshot paths = %v, want [%s] (path-set must survive parsing, spec §8)", snaps[0].Paths, filepath.Clean(src))
	}

	latest, err := repo.Snapshots(ctx, SnapshotFilter{Host: "ballast-test", Paths: []string{src}, Latest: 1})
	if err != nil {
		t.Fatalf("Snapshots --latest: %v", err)
	}
	if len(latest) != 1 {
		t.Fatalf("Latest 1 = %d snapshots, want 1 (per path-set, spec §8)", len(latest))
	}

	entries, err := repo.LS(ctx, second.SnapshotID, filepath.Join(src, "sub"))
	if err != nil {
		t.Fatalf("LS: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Path == filepath.Join(src, "sub", "b.txt") && e.Type == "file" {
			found = true
		}
	}
	if !found {
		t.Errorf("LS under sub = %v, want it to contain sub/b.txt", entries)
	}

	var dumped bytes.Buffer
	if err := repo.Dump(ctx, second.SnapshotID, filepath.Join(src, "a.txt"), &dumped); err != nil {
		t.Fatalf("Dump: %v", err)
	}
	if dumped.String() != "hello file one\n" {
		t.Errorf("dumped %q, want the original content", dumped.String())
	}

	target := filepath.Join(t.TempDir(), "restored")
	if err := repo.Restore(ctx, RestoreOptions{SnapshotID: second.SnapshotID, TargetDir: target}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	restored, err := os.ReadFile(filepath.Join(target, src, "sub", "b.txt"))
	if err != nil {
		t.Fatalf("reading restored file: %v", err)
	}
	if string(restored) != "file two content\n" {
		t.Errorf("restored %q, want the original content", restored)
	}

	rb, err := repo.ReadBack(ctx, "latest")
	if err != nil {
		t.Fatalf("ReadBack: %v", err)
	}
	if len(rb.Samples) == 0 {
		t.Error("ReadBack sampled nothing, want at least one file per path-set")
	}

	// Retention: keep only the newest, then prune the old data.
	forgetRes, err := repo.Forget(ctx, RetentionPolicy{KeepLast: 1}, ForgetOptions{})
	if err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if len(forgetRes.Removed) != 1 {
		t.Errorf("Forget removed %v, want the one older snapshot", forgetRes.Removed)
	}
	if err := repo.Prune(ctx); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	left, err := repo.Snapshots(ctx, SnapshotFilter{})
	if err != nil {
		t.Fatalf("Snapshots after prune: %v", err)
	}
	if len(left) != 1 {
		t.Errorf("got %d snapshots after retention, want 1", len(left))
	}

	v, err := repo.Version(ctx)
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if v.Number == "" {
		t.Error("Version.Number must be set")
	}
	if w := v.Warning(); w != "" {
		t.Logf("restic version warning (informational): %s", w)
	}
}

// TestIntegrationWrongPassword: exit status 12 becomes the
// plain-English password error against a real restic too.
func TestIntegrationWrongPassword(t *testing.T) {
	bin := requireRestic(t)
	ctx := context.Background()

	passFile := filepath.Join(t.TempDir(), "repo-password")
	if err := os.WriteFile(passFile, []byte("first-password\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repoDir := filepath.Join(t.TempDir(), "repo")
	repo, err := Open(repoDir, Options{Runner: &runner.Runner{}, PasswordFile: passFile, ResticBin: bin})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := repo.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	if err := os.WriteFile(passFile, []byte("wrong-password\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Snapshots(ctx, SnapshotFilter{}); err == nil {
		t.Fatal("Snapshots with wrong password = nil error, want failure")
	} else if !strings.Contains(err.Error(), "password") {
		t.Errorf("error = %q, want the plain-English password hint", err)
	}
}

// TestIsBucketMissing checks that only an early-stopped "bucket does
// not exist" retry counts as a missing bucket, so setup tries restic
// init (which creates it) and nothing else is mistaken for one.
func TestIsBucketMissing(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"minio wording", &runner.AbortError{Cmd: "restic", Stderr: "Stat(<config/>) returned error, retrying after 1s: Stat: The specified bucket does not exist"}, true},
		{"aws code", fmt.Errorf("wrapped: %w", &runner.AbortError{Cmd: "restic", Stderr: "... NoSuchBucket ..."}), true},
		{"bad key abort", &runner.AbortError{Cmd: "restic", Stderr: "SignatureDoesNotMatch"}, false},
		{"exit error with same text", &runner.ExitError{Cmd: "restic", Code: 1, Stderr: "NoSuchBucket"}, false},
		{"nil", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsBucketMissing(tt.err); got != tt.want {
				t.Errorf("IsBucketMissing = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestForgetOptions: the service tag and group-by reach restic in the
// forms that mean "only this service's snapshots, as one series", and
// malformed options are refused before restic runs.
func TestForgetOptions(t *testing.T) {
	tests := []struct {
		name     string
		opts     ForgetOptions
		wantArgs []string // consecutive argv fragments
		notArgs  []string
		wantErr  string
	}{
		{name: "no scoping", opts: ForgetOptions{}, notArgs: []string{"--tag", "--group-by"}},
		{name: "one tag", opts: ForgetOptions{Tags: []string{"ballast-service:immich"}}, wantArgs: []string{"--tag ballast-service:immich"}},
		{name: "all tags in one flag", opts: ForgetOptions{Tags: []string{"ballast", "ballast-service:immich"}}, wantArgs: []string{"--tag ballast,ballast-service:immich"}},
		{name: "group by tags", opts: ForgetOptions{GroupBy: "tags"}, wantArgs: []string{"--group-by tags"}},
		{name: "group by host,paths", opts: ForgetOptions{GroupBy: "host,paths"}, wantArgs: []string{"--group-by host,paths"}},
		{name: "empty tag", opts: ForgetOptions{Tags: []string{""}}, wantErr: "must be non-empty"},
		{name: "comma in tag", opts: ForgetOptions{Tags: []string{"a,b"}}, wantErr: "must not contain a comma"},
		{name: "bad group-by", opts: ForgetOptions{GroupBy: "tags,time"}, wantErr: "host, paths or tags"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newResticStub(t)
			repo := s.openStub(t)
			_, err := repo.Forget(context.Background(), DefaultRetention(), tt.opts)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Forget error = %v, want %q", err, tt.wantErr)
				}
				if n := len(s.invocations(t)); n != 0 {
					t.Errorf("restic ran %d times, want none for invalid options", n)
				}
				return
			}
			if err != nil {
				t.Fatalf("Forget: %v", err)
			}
			log := s.invocations(t)
			if len(log) != 1 {
				t.Fatalf("invocations = %v, want one", log)
			}
			for _, part := range tt.wantArgs {
				if !strings.Contains(log[0], part) {
					t.Errorf("argv = %q, want %q", log[0], part)
				}
			}
			for _, part := range tt.notArgs {
				if strings.Contains(log[0], part) {
					t.Errorf("argv = %q, want no %q", log[0], part)
				}
			}
		})
	}
}

// TestRetryLockPlacement: --retry-lock follows the subcommand of
// every locking command, and is left off where restic never locks or
// where it conflicts with --no-lock.
func TestRetryLockPlacement(t *testing.T) {
	tests := []struct {
		name      string
		retryLock time.Duration
		args      []string
		want      []string
	}{
		{name: "backup", retryLock: 10 * time.Minute, args: []string{"backup", "--json", "/p"}, want: []string{"backup", "--retry-lock", "10m0s", "--json", "/p"}},
		{name: "forget", retryLock: time.Minute, args: []string{"forget", "--json"}, want: []string{"forget", "--retry-lock", "1m0s", "--json"}},
		{name: "prune", retryLock: time.Minute, args: []string{"prune"}, want: []string{"prune", "--retry-lock", "1m0s"}},
		{name: "no-lock read", retryLock: time.Minute, args: []string{"snapshots", "--json", "--no-lock"}, want: []string{"snapshots", "--json", "--no-lock"}},
		{name: "init never locks", retryLock: time.Minute, args: []string{"init"}, want: []string{"init"}},
		{name: "version never locks", retryLock: time.Minute, args: []string{"version"}, want: []string{"version"}},
		{name: "unset", args: []string{"prune"}, want: []string{"prune"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &Repository{Location: "/srv/backups", Runner: &runner.Runner{}, RetryLock: tt.retryLock, ExtendedOptions: []string{"k=v"}}
			got := repo.command(tt.args...).Args
			want := append([]string{"-o", "k=v"}, tt.want...)
			if !slices.Equal(got, want) {
				t.Errorf("argv = %q, want %q", got, want)
			}
		})
	}
}

// TestOpenCarriesRetryLock: Options.RetryLock reaches the repository.
func TestOpenCarriesRetryLock(t *testing.T) {
	repo, err := Open("/srv/backups", Options{Runner: &runner.Runner{}, RetryLock: DefaultRetryLock})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if repo.RetryLock != DefaultRetryLock {
		t.Errorf("RetryLock = %v, want %v", repo.RetryLock, DefaultRetryLock)
	}
}

// TestBackupPassesExcludes: skip patterns reach restic as --exclude,
// and a bad pattern is refused before restic runs.
func TestBackupPassesExcludes(t *testing.T) {
	s := newResticStub(t)
	repo := s.openStub(t)
	dir := t.TempDir()

	if _, err := repo.Backup(context.Background(), BackupOptions{Paths: []string{dir}, Excludes: []string{"*.log", "node_modules"}}); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	log := s.invocations(t)
	if len(log) != 1 || !strings.Contains(log[0], "--exclude *.log --exclude node_modules") {
		t.Fatalf("argv = %v, want both --exclude flags", log)
	}
	for _, bad := range []string{"", "a\nb"} {
		if _, err := repo.Backup(context.Background(), BackupOptions{Paths: []string{dir}, Excludes: []string{bad}}); err == nil {
			t.Errorf("pattern %q must be refused", bad)
		}
	}
}

// TestForgetKeepLast: keep-last is passed straight to restic forget and
// counts as a rule that keeps something.
func TestForgetKeepLast(t *testing.T) {
	s := newResticStub(t)
	repo := s.openStub(t)
	if _, err := repo.Forget(context.Background(), RetentionPolicy{KeepLast: 2}, ForgetOptions{}); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	log := s.invocations(t)
	if len(log) != 1 || !strings.Contains(log[0], "--keep-last 2") || strings.Contains(log[0], "--keep-daily") {
		t.Errorf("argv = %v, want only --keep-last 2", log)
	}
}

func TestProgressWriterReportsStatusLines(t *testing.T) {
	var got []BackupProgress
	w := &progressWriter{fn: func(p BackupProgress) { got = append(got, p) }}
	// Lines split across writes, a non-status line and a blank line.
	chunks := []string{
		`{"message_type":"status","percent_done":0.5,"total_bytes":200,"bytes_do`,
		`ne":100,"seconds_remaining":30}` + "\n\n",
		`{"message_type":"summary","snapshot_id":"abc"}` + "\n",
	}
	for _, c := range chunks {
		if n, err := w.Write([]byte(c)); err != nil || n != len(c) {
			t.Fatalf("Write = %d, %v", n, err)
		}
	}
	want := []BackupProgress{{PercentDone: 0.5, BytesDone: 100, TotalBytes: 200, SecondsRemaining: 30}}
	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("progress = %+v, want %+v", got, want)
	}
	if !strings.Contains(w.all.String(), `"snapshot_id":"abc"`) {
		t.Error("the full output must still be kept for parsing the summary")
	}
}

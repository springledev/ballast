package strategy

import (
	"context"
	"errors"
	"github.com/springledev/keel/dockerhost"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/springledev/ballast/internal/catalog"
	"github.com/springledev/keel/runner"
)

// TestDumpContainer runs a container dump against the docker stub and
// checks the artifact, the exact docker argv (including -e env
// forwarding), and the artifact's restrictive permissions.
func TestDumpContainer(t *testing.T) {
	dockerBin, logPath := newDockerStub(t)
	entry := &catalog.Entry{
		Service: "immich",
		Backup: catalog.Backup{
			Strategy: "dump",
			Dumps: []catalog.Dump{{
				Name:      "database",
				Container: "immich_postgres",
				Command:   []string{"pg_dumpall", "-U", "postgres"},
				Format:    "sql",
			}},
		},
	}
	s, err := For(entry, Options{Runner: stubRunner(logPath), Docker: dockerhost.NewLocal(dockerBin)})
	if err != nil {
		t.Fatalf("For: %v", err)
	}

	workDir := filepath.Join(t.TempDir(), "run")
	res, err := s.Backup(context.Background(), Request{
		WorkDir: workDir,
		Env:     []string{"PGPASSFILE=/run/credentials/pg"},
	})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	if len(res.Artifacts) != 1 {
		t.Fatalf("Artifacts = %v, want exactly one", res.Artifacts)
	}
	a := res.Artifacts[0]
	if a.Name != "database" || a.Format != "sql" {
		t.Errorf("Artifact = %+v, want Name=database Format=sql", a)
	}
	if filepath.Base(a.Path) != "database.sql" {
		t.Errorf("artifact file = %s, want database.sql", a.Path)
	}
	content, err := os.ReadFile(a.Path)
	if err != nil {
		t.Fatalf("reading artifact: %v", err)
	}
	if string(content) != "CREATE TABLE stub(id int);\n" {
		t.Errorf("artifact content = %q, want the stub dump", content)
	}
	info, err := os.Stat(a.Path)
	if err != nil {
		t.Fatalf("stat artifact: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("artifact permissions = %04o, want 0600 (dump output is sensitive)", perm)
	}

	// The work directory was created owner-only.
	winfo, err := os.Stat(workDir)
	if err != nil {
		t.Fatalf("stat work dir: %v", err)
	}
	if perm := winfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("work dir permissions = %04o, want 0700", perm)
	}

	// docker was invoked as: exec -e PGPASSFILE immich_postgres pg_dumpall -U postgres
	// (env key forwarded, value never in argv).
	log := readStubLog(t, logPath)
	want := "exec -e PGPASSFILE immich_postgres pg_dumpall -U postgres"
	if len(log) != 1 || log[0] != want {
		t.Errorf("docker invocations = %v, want [%q]", log, want)
	}
}

// TestDumpSecretEnvForwarded checks that a dump's catalog secretEnv
// (already resolved to a plaintext value, as catalog.Entry.ResolveSecrets
// would leave it) is forwarded into the container exactly like a
// non-secret env key — `docker exec -e KEY`, value never in argv —
// alongside the request's own env.
func TestDumpSecretEnvForwarded(t *testing.T) {
	dockerBin, logPath := newDockerStub(t)
	entry := &catalog.Entry{
		Service: "mysql",
		Backup: catalog.Backup{
			Strategy: "dump",
			Dumps: []catalog.Dump{{
				Name:      "database",
				Container: "mysql_1",
				Command:   []string{"mysqldump", "--all-databases"},
				Format:    "sql",
				SecretEnv: map[string]string{"MYSQL_PWD": "s3cr3t-pw"},
			}},
		},
	}
	s, err := For(entry, Options{Runner: stubRunner(logPath), Docker: dockerhost.NewLocal(dockerBin)})
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	res, err := s.Backup(context.Background(), Request{
		WorkDir: t.TempDir(),
		Env:     []string{"PGPASSFILE=/run/credentials/pg"},
	})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if len(res.Artifacts) != 1 {
		t.Fatalf("Artifacts = %v, want exactly one", res.Artifacts)
	}

	log := readStubLog(t, logPath)
	want := "exec -e PGPASSFILE -e MYSQL_PWD mysql_1 mysqldump --all-databases"
	if len(log) != 1 || log[0] != want {
		t.Errorf("docker invocations = %v, want [%q] (secret value must never appear in argv)", log, want)
	}
	if strings.Contains(log[0], "s3cr3t-pw") {
		t.Fatalf("docker argv leaked the secret value: %q", log[0])
	}
}

// TestDumpSecretEnvRedacted checks that a secretEnv value is redacted
// from the error a failing dump returns, even though the value never
// appears in argv and so is only reachable via the command's own
// stderr.
func TestDumpSecretEnvRedacted(t *testing.T) {
	dir := t.TempDir()
	hostStub := writeStub(t, dir, "leaky", "#!/bin/sh\necho \"auth failed for $DB_PASSWORD\" >&2\nexit 1\n")
	entry := &catalog.Entry{
		Service: "svc",
		Backup: catalog.Backup{
			Strategy: "dump",
			Dumps: []catalog.Dump{{
				Name:      "database",
				Command:   []string{hostStub},
				SecretEnv: map[string]string{"DB_PASSWORD": "top-secret-value"},
			}},
		},
	}
	s, err := For(entry, Options{Runner: &runner.Runner{}})
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	_, err = s.Backup(context.Background(), Request{WorkDir: t.TempDir()})
	if err == nil {
		t.Fatal("Backup = nil error, want the dump failure")
	}
	if strings.Contains(err.Error(), "top-secret-value") {
		t.Errorf("error leaked the secret value: %v", err)
	}
	if !strings.Contains(err.Error(), "[redacted]") {
		t.Errorf("error = %q, want the redaction placeholder", err)
	}
}

// TestDumpHostCommand runs a dump without a container: the command
// runs directly, with the request environment.
func TestDumpHostCommand(t *testing.T) {
	dir := t.TempDir()
	hostStub := writeStub(t, dir, "hostdump", "#!/bin/sh\nprintf 'env=%s\\n' \"$DUMP_MARKER\"\n")
	entry := &catalog.Entry{
		Service: "postgres",
		Backup: catalog.Backup{
			Strategy: "dump",
			Dumps:    []catalog.Dump{{Name: "database", Command: []string{hostStub}, Format: "sql"}},
		},
	}
	s, err := For(entry, Options{Runner: &runner.Runner{}})
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	res, err := s.Backup(context.Background(), Request{
		WorkDir: t.TempDir(),
		Env:     []string{"DUMP_MARKER=from-request-env"},
	})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	content, err := os.ReadFile(res.Artifacts[0].Path)
	if err != nil {
		t.Fatalf("reading artifact: %v", err)
	}
	if string(content) != "env=from-request-env\n" {
		t.Errorf("artifact content = %q, want the request env visible to the command", content)
	}
}

// TestDumpFailurePropagates checks that a failing dump command fails
// the backup with the service and dump named, and that the runner's
// exit-status detail survives wrapping.
func TestDumpFailurePropagates(t *testing.T) {
	dockerBin, logPath := newDockerStub(t)
	entry := &catalog.Entry{
		Service: "immich",
		Backup: catalog.Backup{
			Strategy: "dump",
			Dumps: []catalog.Dump{{
				Name:      "database",
				Container: "fail-exec",
				Command:   []string{"pg_dumpall"},
			}},
		},
	}
	s, err := For(entry, Options{Runner: stubRunner(logPath), Docker: dockerhost.NewLocal(dockerBin)})
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	_, err = s.Backup(context.Background(), Request{WorkDir: t.TempDir()})
	if err == nil {
		t.Fatal("Backup = nil error, want the dump failure")
	}
	if !strings.Contains(err.Error(), "immich") || !strings.Contains(err.Error(), `"database"`) {
		t.Errorf("error = %q, want it to name service immich and dump \"database\"", err)
	}
	var exitErr *runner.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("error = %v, want it to wrap a runner.ExitError", err)
	}
	if exitErr.Code != 1 {
		t.Errorf("exit code = %d, want 1", exitErr.Code)
	}
}

// TestDumpIncludesFilePaths checks the Immich shape: a dump entry
// with backup.files returns those paths for direct upload, and a bad
// path fails the run.
func TestDumpIncludesFilePaths(t *testing.T) {
	dockerBin, logPath := newDockerStub(t)
	library := t.TempDir()
	entry := &catalog.Entry{
		Service: "immich",
		Backup: catalog.Backup{
			Strategy: "dump",
			Dumps:    []catalog.Dump{{Name: "database", Container: "db", Command: []string{"pg_dumpall"}}},
			Files:    []catalog.FileSpec{{Path: library}},
		},
	}
	s, err := For(entry, Options{Runner: stubRunner(logPath), Docker: dockerhost.NewLocal(dockerBin)})
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	res, err := s.Backup(context.Background(), Request{WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if len(res.Paths) != 1 || res.Paths[0] != library {
		t.Errorf("Paths = %v, want [%s]", res.Paths, library)
	}

	entry.Backup.Files = []catalog.FileSpec{{Path: filepath.Join(library, "missing")}}
	if _, err := s.Backup(context.Background(), Request{WorkDir: t.TempDir()}); err == nil {
		t.Error("Backup with a missing files path = nil error, want failure")
	}
}

// TestDumpRejectsEmptyWorkDir makes sure a misconfigured run fails
// loudly instead of writing artifacts into a surprising place.
func TestDumpRejectsEmptyWorkDir(t *testing.T) {
	entry := &catalog.Entry{
		Service: "svc",
		Backup: catalog.Backup{
			Strategy: "dump",
			Dumps:    []catalog.Dump{{Name: "database", Command: []string{"true"}}},
		},
	}
	s, err := For(entry, Options{Runner: &runner.Runner{}})
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	if _, err := s.Backup(context.Background(), Request{}); err == nil {
		t.Error("Backup with empty work dir = nil error, want failure")
	}
}

// TestDumpRejectsUncreatableWorkDir points the work dir at a path
// that cannot be created (a file is in the way) and expects a loud
// error, not a panic.
func TestDumpRejectsUncreatableWorkDir(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("file"), 0o644); err != nil {
		t.Fatal(err)
	}
	entry := &catalog.Entry{
		Service: "svc",
		Backup: catalog.Backup{
			Strategy: "dump",
			Dumps:    []catalog.Dump{{Name: "database", Command: []string{"true"}}},
		},
	}
	s, err := For(entry, Options{Runner: &runner.Runner{}})
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	_, err = s.Backup(context.Background(), Request{WorkDir: filepath.Join(blocker, "run")})
	if err == nil {
		t.Fatal("Backup = nil error, want the work-dir creation failure")
	}
	if !strings.Contains(err.Error(), "svc") {
		t.Errorf("error = %q, want it to name the service", err)
	}
}

// TestDumpDuplicateArtifactName runs two dumps that map to the same
// artifact file: the second must fail instead of silently overwriting
// the first dump's output.
func TestDumpDuplicateArtifactName(t *testing.T) {
	dir := t.TempDir()
	hostStub := writeStub(t, dir, "hostdump", "#!/bin/sh\nprintf 'x\\n'\n")
	entry := &catalog.Entry{
		Service: "svc",
		Backup: catalog.Backup{
			Strategy: "dump",
			Dumps: []catalog.Dump{
				{Name: "database", Command: []string{hostStub}, Format: "sql"},
				{Name: "database", Command: []string{hostStub}, Format: "sql"},
			},
		},
	}
	s, err := For(entry, Options{Runner: &runner.Runner{}})
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	_, err = s.Backup(context.Background(), Request{WorkDir: t.TempDir()})
	if err == nil {
		t.Fatal("Backup = nil error, want the duplicate-artifact failure")
	}
	if !strings.Contains(err.Error(), "creating artifact file") {
		t.Errorf("error = %q, want it to explain the artifact file collision", err)
	}
}

// pctStub is a fake `pct` CLI for pct-exec (Proxmox container exec).
// It appends every invocation's argv to $STUB_LOG and echoes the command
// output on success.
const pctStub = `#!/bin/sh
printf '%s\n' "$*" >> "$STUB_LOG"
case "$1" in
  exec)
    # pct exec <vmid> -- <command...>
    # Simulate successful execution with a dump
    printf 'CREATE TABLE stub(id int);\n'
    ;;
  *)
    echo "pct stub: unsupported command $1" >&2
    exit 1
    ;;
esac
exit 0
`

// newPctStub installs the pct stub in a temp directory and returns its
// path plus the path to the log file it records invocations in.
func newPctStub(t *testing.T) (bin, logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "pct.log")
	bin = writeStub(t, dir, "pct-stub", pctStub)
	return bin, logPath
}

// TestDumpPctExec tests the pct-exec transport: the dump command is
// invoked via `pct exec <vmid> -- <command>` instead of `docker exec`.
func TestDumpPctExec(t *testing.T) {
	pctBin, logPath := newPctStub(t)
	entry := &catalog.Entry{
		Service: "postgres-guest",
		Backup: catalog.Backup{
			Strategy: "dump",
			Dumps: []catalog.Dump{{
				Name:      "database",
				Container: "100",
				Transport: "pct-exec",
				Command:   []string{"pg_dumpall", "-U", "postgres"},
				Format:    "sql",
			}},
		},
	}
	s, err := For(entry, Options{Runner: stubRunner(logPath), PctBin: pctBin})
	if err != nil {
		t.Fatalf("For: %v", err)
	}

	workDir := filepath.Join(t.TempDir(), "run")
	res, err := s.Backup(context.Background(), Request{WorkDir: workDir})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	if len(res.Artifacts) != 1 {
		t.Fatalf("Artifacts = %v, want exactly one", res.Artifacts)
	}
	a := res.Artifacts[0]
	if a.Name != "database" || a.Format != "sql" {
		t.Errorf("Artifact = %+v, want Name=database Format=sql", a)
	}
	if filepath.Base(a.Path) != "database.sql" {
		t.Errorf("artifact file = %s, want database.sql", a.Path)
	}
	content, err := os.ReadFile(a.Path)
	if err != nil {
		t.Fatalf("reading artifact: %v", err)
	}
	if string(content) != "CREATE TABLE stub(id int);\n" {
		t.Errorf("artifact content = %q, want the stub dump", content)
	}

	// Verify the pct invocation: pct exec 100 -- pg_dumpall -U postgres
	log := readStubLog(t, logPath)
	want := "exec 100 -- pg_dumpall -U postgres"
	if len(log) != 1 || log[0] != want {
		t.Errorf("pct invocations = %v, want [%q]", log, want)
	}
}

// TestDumpPctExecRejectsEnv checks that a pct-exec dump fails loudly
// when given environment variables, since pct exec cannot forward
// them and they would otherwise be silently dropped.
func TestDumpPctExecRejectsEnv(t *testing.T) {
	pctBin, logPath := newPctStub(t)
	entry := &catalog.Entry{
		Service: "svc",
		Backup: catalog.Backup{
			Strategy: "dump",
			Dumps: []catalog.Dump{{
				Name:      "database",
				Container: "101",
				Transport: "pct-exec",
				Command:   []string{"pg_dumpall"},
			}},
		},
	}
	s, err := For(entry, Options{Runner: stubRunner(logPath), PctBin: pctBin})
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	_, err = s.Backup(context.Background(), Request{WorkDir: t.TempDir(), Env: []string{"MYVAR=myvalue"}})
	if err == nil || !strings.Contains(err.Error(), "cannot pass environment variables") {
		t.Fatalf("Backup error = %v, want pct-exec env rejection", err)
	}
	if strings.Contains(err.Error(), "myvalue") {
		t.Errorf("error leaked env value: %v", err)
	}
	if log := readStubLog(t, logPath); len(log) != 0 {
		t.Errorf("pct invocations = %v, want none", log)
	}
}

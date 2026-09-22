package strategy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/springledev/ballast/internal/catalog"
	"github.com/springledev/ballast/internal/runner"
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
	s, err := For(entry, Options{Runner: stubRunner(logPath), DockerBin: dockerBin})
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
	s, err := For(entry, Options{Runner: stubRunner(logPath), DockerBin: dockerBin})
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
	s, err := For(entry, Options{Runner: stubRunner(logPath), DockerBin: dockerBin})
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

package verify

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/springledev/ballast/internal/strategy"
)

// runCheck is a small helper: build the checker through For (so the
// registry's validation is exercised on every test) and run it.
func runCheck(t *testing.T, opts Options, image string, filter []string, req Request) Result {
	t.Helper()
	entry := containerRestoreEntry(image, filter...)
	checks, err := For(entry, opts)
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	c := checks[0]
	if req.Service == "" {
		req.Service = entry.Service
	}
	return c.Check(context.Background(), req)
}

// TestContainerRestorePass runs the happy path against the docker
// stub and asserts the exact docker argv sequence: network isolation
// (--network none), the server binary passed as the run command so
// the image's own entrypoint performs its first-run initialization
// (spec §7.2 MUST — see the ContainerRestore "spec deviation" note),
// readiness probe, restore client reading the dump on stdin, and the
// guaranteed destroy.
func TestContainerRestorePass(t *testing.T) {
	opts, logPath, withEnv := stubSetup(t)
	capture := logPath + ".capture"
	withEnv("STUB_CAPTURE=" + capture)

	dump := "CREATE TABLE t(id int);\nINSERT INTO t VALUES (1);\n"
	artifact := sqlArtifact(t, "database", dump)

	res := runCheck(t, opts, "postgres:16", nil, Request{
		Artifacts: []strategy.Artifact{artifact},
	})
	if !res.Passed {
		t.Fatalf("Check() failed: %q", res.Detail)
	}
	if res.Warning != "" {
		t.Errorf("Check() warning = %q, want none on the happy path", res.Warning)
	}

	log := readStubLog(t, logPath)
	name := containerName(t, log)
	want := []string{
		"run --detach --network none --name " + name + " -e POSTGRES_HOST_AUTH_METHOD=trust postgres:16",
		"exec " + name + " pg_isready -q -U postgres",
		// waitReady re-confirms a first successful probe after a short
		// settle delay (a real postgres/mysql image briefly runs a
		// temporary init server before the real one starts).
		"exec " + name + " pg_isready -q -U postgres",
		"exec -i " + name + " psql -v ON_ERROR_STOP=0 -q -U postgres",
		"rm --force " + name,
	}
	if len(log) != len(want) {
		t.Fatalf("docker invocations = %v, want exactly %v", log, want)
	}
	for i := range want {
		if log[i] != want[i] {
			t.Errorf("invocation %d = %q, want %q", i, log[i], want[i])
		}
	}

	// The dump travelled into the container on stdin.
	got, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("reading captured stdin: %v", err)
	}
	if string(got) != dump {
		t.Errorf("restore stdin = %q, want the dump content", got)
	}

	// The name is a generated throwaway, never user-controlled.
	if !strings.HasPrefix(name, "ballast-verify-") {
		t.Errorf("container name = %q, want the ballast-verify- prefix", name)
	}
}

// TestContainerRestoreMySQLDialect checks the image-driven dialect
// dispatch: mariadb/mysql images get mysqld as the run command, the
// empty-password run flag (no secret in argv), mysqladmin readiness,
// and the mysql client.
func TestContainerRestoreMySQLDialect(t *testing.T) {
	opts, logPath, _ := stubSetup(t)
	artifact := sqlArtifact(t, "database", "CREATE TABLE t(id int);\n")

	res := runCheck(t, opts, "mariadb:11", nil, Request{
		Artifacts: []strategy.Artifact{artifact},
	})
	if !res.Passed {
		t.Fatalf("Check() failed: %q", res.Detail)
	}
	log := readStubLog(t, logPath)
	name := containerName(t, log)
	want := []string{
		"run --detach --network none --name " + name + " -e MYSQL_ALLOW_EMPTY_PASSWORD=yes mariadb:11",
		"exec " + name + " mysqladmin -s ping",
		"exec " + name + " mysqladmin -s ping",
		"exec -i " + name + " mysql -u root",
		"rm --force " + name,
	}
	for i := range want {
		if log[i] != want[i] {
			t.Errorf("invocation %d = %q, want %q", i, log[i], want[i])
		}
	}
}

// TestContainerRestoreRequestEnv: Request.Env reaches every spawned
// docker command — it is the plumbing for DOCKER_HOST-style settings
// and credential-directory paths. The stub writes its argv log where
// STUB_LOG points, so steering it through Request.Env proves every
// invocation (inspect, run, both execs, rm) received the override.
func TestContainerRestoreRequestEnv(t *testing.T) {
	opts, logPath, _ := stubSetup(t)
	altLog := logPath + ".env"
	artifact := sqlArtifact(t, "database", "SELECT 1;\n")

	res := runCheck(t, opts, "postgres:16", nil, Request{
		Artifacts:       []strategy.Artifact{artifact},
		SourceContainer: "db", // exercises the inspect call site too
		Env:             []string{"STUB_LOG=" + altLog},
	})
	if !res.Passed {
		t.Fatalf("Check() failed: %q", res.Detail)
	}
	if got := readStubLog(t, logPath); got != nil {
		t.Errorf("default log received %v, want every invocation steered by Request.Env", got)
	}
	log := readStubLog(t, altLog)
	if len(log) != 6 {
		t.Fatalf("steered log = %v, want 6 invocations (inspect, run, probe, settle-confirm probe, restore, rm)", log)
	}
}

// TestContainerRestoreEmptyRequestService: a caller that forgets
// Request.Service still gets per-service error attribution — the
// checker falls back to the catalog entry's service name.
func TestContainerRestoreEmptyRequestService(t *testing.T) {
	opts, _, _ := stubSetup(t)
	entry := containerRestoreEntry("postgres:16")
	checks, err := For(entry, opts)
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	c := checks[0]
	// Bypass runCheck (which fills Service in): no Service, no
	// artifacts — the failure must still name the service.
	res := c.Check(context.Background(), Request{})
	if res.Passed {
		t.Fatal("Check() passed with no artifacts, want failure")
	}
	if !strings.Contains(res.Detail, "service "+entry.Service+":") {
		t.Errorf("Check() detail = %q, want it to name the catalog entry's service %q", res.Detail, entry.Service)
	}
}

// TestContainerRestoreImageDriftWarning enforces the spec §7.2 MUST:
// a live container whose image differs from the test-restore image
// produces a warning, not a silent pass.
func TestContainerRestoreImageDriftWarning(t *testing.T) {
	opts, _, withEnv := stubSetup(t)
	withEnv("STUB_SOURCE_IMAGE=postgres:15")
	artifact := sqlArtifact(t, "database", "SELECT 1;\n")

	res := runCheck(t, opts, "postgres:16", nil, Request{
		Artifacts:       []strategy.Artifact{artifact},
		SourceContainer: "immich_postgres",
	})
	if !res.Passed {
		t.Fatalf("Check() failed: %q", res.Detail)
	}
	if !strings.Contains(res.Warning, "immich_postgres") ||
		!strings.Contains(res.Warning, "postgres:15") ||
		!strings.Contains(res.Warning, "postgres:16") {
		t.Errorf("Check() warning = %q, want it to name the container and both images", res.Warning)
	}
}

// TestContainerRestoreNoSourceContainer skips the drift comparison
// when no source container is known.
func TestContainerRestoreNoSourceContainer(t *testing.T) {
	opts, logPath, _ := stubSetup(t)
	artifact := sqlArtifact(t, "database", "SELECT 1;\n")

	res := runCheck(t, opts, "postgres:16", nil, Request{
		Artifacts: []strategy.Artifact{artifact},
	})
	if !res.Passed {
		t.Fatalf("Check() failed: %q", res.Detail)
	}
	for _, line := range readStubLog(t, logPath) {
		if strings.HasPrefix(line, "inspect ") {
			t.Errorf("unexpected docker inspect without a source container: %q", line)
		}
	}
}

// TestContainerRestoreInspectFailureWarning: when the drift
// comparison itself fails, verification continues with a warning.
func TestContainerRestoreInspectFailureWarning(t *testing.T) {
	opts, _, withEnv := stubSetup(t)
	withEnv("STUB_FAIL_INSPECT=1")
	artifact := sqlArtifact(t, "database", "SELECT 1;\n")

	res := runCheck(t, opts, "postgres:16", nil, Request{
		Artifacts:       []strategy.Artifact{artifact},
		SourceContainer: "db",
	})
	if !res.Passed {
		t.Fatalf("Check() failed: %q", res.Detail)
	}
	if !strings.Contains(res.Warning, "could not compare") {
		t.Errorf("Check() warning = %q, want the comparison failure mentioned", res.Warning)
	}
}

// TestContainerRestoreBenignStderr: whitelist-filtered stderr lines
// pass (spec §7.2: only catalog-declared benign patterns are ignored).
func TestContainerRestoreBenignStderr(t *testing.T) {
	opts, _, withEnv := stubSetup(t)
	withEnv(`STUB_RESTORE_STDERR=ERROR:  role "postgres" already exists`)
	artifact := sqlArtifact(t, "database", "SELECT 1;\n")

	res := runCheck(t, opts, "postgres:16", []string{"already exists"}, Request{
		Artifacts: []strategy.Artifact{artifact},
	})
	if !res.Passed {
		t.Fatalf("Check() failed: %q (benign line must be filtered)", res.Detail)
	}
	if !strings.Contains(res.Output, "already exists") {
		t.Errorf("Check() output = %q, want the benign line recorded", res.Output)
	}
}

// TestContainerRestoreUnfilteredFailure: anything NOT on the
// whitelist fails the check — the normative whitelist MUST.
func TestContainerRestoreUnfilteredFailure(t *testing.T) {
	opts, logPath, withEnv := stubSetup(t)
	withEnv(`STUB_RESTORE_STDERR=ERROR:  null value in column "bio" violates not-null constraint`)
	artifact := sqlArtifact(t, "database", "INSERT INTO t(bio) VALUES (NULL);\n")

	res := runCheck(t, opts, "postgres:16", []string{"already exists"}, Request{
		Artifacts: []strategy.Artifact{artifact},
	})
	if res.Passed {
		t.Fatal("Check() passed with an unfiltered stderr line, want failure")
	}
	if !strings.Contains(res.Detail, "not-null constraint") {
		t.Errorf("Check() detail = %q, want the offending stderr line", res.Detail)
	}
	// The container was still destroyed (spec §7.2 cleanup MUST).
	log := readStubLog(t, logPath)
	name := containerName(t, log)
	if log[len(log)-1] != "rm --force "+name {
		t.Errorf("last docker invocation = %q, want the guaranteed destroy", log[len(log)-1])
	}
}

// TestContainerRestoreMixedStderr: benign and unfiltered lines
// together still fail, and both appear in the recorded output.
func TestContainerRestoreMixedStderr(t *testing.T) {
	opts, _, withEnv := stubSetup(t)
	withEnv(`STUB_RESTORE_STDERR=` + "ERROR:  already exists\n" + `ERROR:  syntax error at or near "SELEC"`)
	artifact := sqlArtifact(t, "database", "SELEC 1;\n")

	res := runCheck(t, opts, "postgres:16", []string{"already exists"}, Request{
		Artifacts: []strategy.Artifact{artifact},
	})
	if res.Passed {
		t.Fatal("Check() passed with an unfiltered stderr line, want failure")
	}
	if !strings.Contains(res.Detail, "syntax error") {
		t.Errorf("Check() detail = %q, want the unfiltered line", res.Detail)
	}
	if !strings.Contains(res.Output, "already exists") || !strings.Contains(res.Output, "syntax error") {
		t.Errorf("Check() output = %q, want both lines recorded", res.Output)
	}
}

// TestContainerRestoreStartFailure: docker run failing fails the
// check, and cleanup is still attempted.
func TestContainerRestoreStartFailure(t *testing.T) {
	opts, logPath, withEnv := stubSetup(t)
	withEnv("STUB_FAIL_RUN=1")
	artifact := sqlArtifact(t, "database", "SELECT 1;\n")

	res := runCheck(t, opts, "postgres:16", nil, Request{
		Artifacts: []strategy.Artifact{artifact},
	})
	if res.Passed {
		t.Fatal("Check() passed when docker run failed, want failure")
	}
	if !strings.Contains(res.Detail, "could not start") {
		t.Errorf("Check() detail = %q, want the start failure", res.Detail)
	}
	log := readStubLog(t, logPath)
	if last := log[len(log)-1]; !strings.HasPrefix(last, "rm --force ") {
		t.Errorf("last docker invocation = %q, want the guaranteed destroy", last)
	}
}

// TestContainerRestoreReadyTimeout: a container that never answers
// its readiness probe fails within the timeout and is destroyed.
func TestContainerRestoreReadyTimeout(t *testing.T) {
	opts, logPath, withEnv := stubSetup(t)
	withEnv("STUB_READY=no")
	artifact := sqlArtifact(t, "database", "SELECT 1;\n")

	entry := containerRestoreEntry("postgres:16")
	checks, err := For(entry, opts)
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	c := checks[0]
	cr := c.(*ContainerRestore)
	cr.ReadyTimeout = 450 * time.Millisecond

	start := time.Now()
	res := cr.Check(context.Background(), Request{
		Service:   entry.Service,
		Artifacts: []strategy.Artifact{artifact},
	})
	elapsed := time.Since(start)
	if res.Passed {
		t.Fatal("Check() passed with a never-ready container, want failure")
	}
	if !strings.Contains(res.Detail, "did not come up") {
		t.Errorf("Check() detail = %q, want the readiness timeout", res.Detail)
	}
	if elapsed > 5*time.Second {
		t.Errorf("Check() took %s, want it bounded by the readiness timeout", elapsed)
	}
	log := readStubLog(t, logPath)
	if last := log[len(log)-1]; !strings.HasPrefix(last, "rm --force ") {
		t.Errorf("last docker invocation = %q, want the guaranteed destroy", last)
	}
}

// TestContainerRestoreNoDumps: a run without SQL artifacts has
// nothing to restore and must fail — verification of nothing is never
// a pass.
func TestContainerRestoreNoDumps(t *testing.T) {
	opts, _, _ := stubSetup(t)
	tar := artifactFile(t, "data.tar", "tar", []byte("x"))

	res := runCheck(t, opts, "postgres:16", nil, Request{
		Artifacts: []strategy.Artifact{tar},
	})
	if res.Passed {
		t.Fatal("Check() passed without any SQL dump, want failure")
	}
	if !strings.Contains(res.Detail, "no SQL dump") {
		t.Errorf("Check() detail = %q, want it to say there was nothing to restore", res.Detail)
	}
}

// TestContainerRestoreExitWithBenignStderr: a non-zero restore client
// whose stderr is fully whitelist-matched passes, but leaves a
// warning so the oddity is visible.
func TestContainerRestoreExitWithBenignStderr(t *testing.T) {
	opts, _, withEnv := stubSetup(t)
	withEnv(`STUB_RESTORE_STDERR=ERROR:  already exists`, "STUB_RESTORE_EXIT=1")
	artifact := sqlArtifact(t, "database", "SELECT 1;\n")

	res := runCheck(t, opts, "postgres:16", []string{"already exists"}, Request{
		Artifacts: []strategy.Artifact{artifact},
	})
	if !res.Passed {
		t.Fatalf("Check() failed: %q", res.Detail)
	}
	if !strings.Contains(res.Warning, "exited with an error") {
		t.Errorf("Check() warning = %q, want the exit-status oddity noted", res.Warning)
	}
}

// TestContainerRestoreHardFailure: a restore client dying with no
// stderr at all fails with the exit error named.
func TestContainerRestoreHardFailure(t *testing.T) {
	opts, _, withEnv := stubSetup(t)
	withEnv("STUB_RESTORE_EXIT=1")
	artifact := sqlArtifact(t, "database", "SELECT 1;\n")

	res := runCheck(t, opts, "postgres:16", nil, Request{
		Artifacts: []strategy.Artifact{artifact},
	})
	if res.Passed {
		t.Fatal("Check() passed with a dead restore client, want failure")
	}
	if !strings.Contains(res.Detail, "database") {
		t.Errorf("Check() detail = %q, want the failing artifact named", res.Detail)
	}
}

// TestContainerRestoreCleanupFailure: cleanup failure surfaces as a
// warning without masking the verdict.
func TestContainerRestoreCleanupFailure(t *testing.T) {
	opts, _, withEnv := stubSetup(t)
	withEnv("STUB_FAIL_RM=1")
	artifact := sqlArtifact(t, "database", "SELECT 1;\n")

	res := runCheck(t, opts, "postgres:16", nil, Request{
		Artifacts: []strategy.Artifact{artifact},
	})
	if !res.Passed {
		t.Fatalf("Check() failed: %q", res.Detail)
	}
	if !strings.Contains(res.Warning, "could not be removed") {
		t.Errorf("Check() warning = %q, want the cleanup failure reported", res.Warning)
	}
}

// TestContainerRestoreMultipleDumps: every SQL artifact is fed into
// the same throwaway container.
func TestContainerRestoreMultipleDumps(t *testing.T) {
	opts, logPath, _ := stubSetup(t)
	a := sqlArtifact(t, "database", "CREATE TABLE a(id int);\n")
	b := sqlArtifact(t, "extensions", "CREATE EXTENSION IF NOT EXISTS pgcrypto;\n")

	res := runCheck(t, opts, "postgres:16", nil, Request{
		Artifacts: []strategy.Artifact{a, b},
	})
	if !res.Passed {
		t.Fatalf("Check() failed: %q", res.Detail)
	}
	if !strings.Contains(res.Detail, "2 dump artifact(s)") {
		t.Errorf("Check() detail = %q, want both dumps counted", res.Detail)
	}
	execs := 0
	for _, line := range readStubLog(t, logPath) {
		if strings.Contains(line, "psql ") {
			execs++
		}
	}
	if execs != 2 {
		t.Errorf("psql restore invocations = %d, want 2 (one per dump)", execs)
	}
}

// TestSplitFiltered unit-tests the whitelist filter itself.
func TestSplitFiltered(t *testing.T) {
	tests := []struct {
		name         string
		lines        []string
		whitelist    []string
		wantBenign   int
		wantLeftover int
	}{
		{"all filtered", []string{"ERROR: already exists", "ERROR: already exists"}, []string{"already exists"}, 2, 0},
		{"none filtered", []string{"ERROR: boom"}, []string{"already exists"}, 0, 1},
		{"empty whitelist fails everything", []string{"ERROR: already exists"}, nil, 0, 1},
		{"empty patterns ignored", []string{"ERROR: x"}, []string{""}, 0, 1},
		{"substring match", []string{"DETAIL: table already exists, skipping"}, []string{"already exists"}, 1, 0},
		{"empty lines dropped before filter", []string{}, []string{"x"}, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			benign, leftover := splitFiltered(tt.lines, tt.whitelist)
			if len(benign) != tt.wantBenign || len(leftover) != tt.wantLeftover {
				t.Errorf("splitFiltered() = %d benign, %d leftover; want %d, %d",
					len(benign), len(leftover), tt.wantBenign, tt.wantLeftover)
			}
		})
	}
}

// TestDialectFor checks image-name dispatch keeps catalog as data:
// any postgres/mysql-family image works without Go code changes.
func TestDialectFor(t *testing.T) {
	tests := []struct {
		image    string
		client   string
		readyArg string
	}{
		{"postgres:16", "psql", "pg_isready"},
		{"ghcr.io/immich-app/postgres:14-vectorchord0.4.3-pgvectors0.2.0", "psql", "pg_isready"},
		{"postgres:17-alpine", "psql", "pg_isready"},
		{"mysql:8", "mysql", "mysqladmin"},
		{"mariadb:11", "mysql", "mysqladmin"},
		{"lscr.io/linuxserver/mariadb:latest", "mysql", "mysqladmin"},
	}
	for _, tt := range tests {
		t.Run(tt.image, func(t *testing.T) {
			d := dialectFor(tt.image)
			if d.client[0] != tt.client {
				t.Errorf("client = %q, want %q", d.client[0], tt.client)
			}
			if d.readyArgs[0] != tt.readyArg {
				t.Errorf("ready probe = %q, want %q", d.readyArgs[0], tt.readyArg)
			}
		})
	}
}

// TestContainerRestoreMatchingImageNoWarning: a live container
// running exactly the verify image produces no drift warning.
func TestContainerRestoreMatchingImageNoWarning(t *testing.T) {
	opts, _, withEnv := stubSetup(t)
	withEnv("STUB_SOURCE_IMAGE=postgres:16")
	artifact := sqlArtifact(t, "database", "SELECT 1;\n")

	res := runCheck(t, opts, "postgres:16", nil, Request{
		Artifacts:       []strategy.Artifact{artifact},
		SourceContainer: "db",
	})
	if !res.Passed {
		t.Fatalf("Check() failed: %q", res.Detail)
	}
	if res.Warning != "" {
		t.Errorf("Check() warning = %q, want none when the images match", res.Warning)
	}
}

// TestContainerRestoreCancelDuringReady: cancelling the job while the
// readiness loop polls fails the check (never hangs), and cleanup
// still runs on the detached context.
func TestContainerRestoreCancelDuringReady(t *testing.T) {
	opts, logPath, withEnv := stubSetup(t)
	withEnv("STUB_READY=no")
	artifact := sqlArtifact(t, "database", "SELECT 1;\n")

	entry := containerRestoreEntry("postgres:16")
	checks, err := For(entry, opts)
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	c := checks[0]
	cr := c.(*ContainerRestore)
	cr.ReadyTimeout = time.Minute // must not be reached

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()
	res := cr.Check(ctx, Request{
		Service:   entry.Service,
		Artifacts: []strategy.Artifact{artifact},
	})
	if res.Passed {
		t.Fatal("Check() passed with a cancelled context, want failure")
	}
	if !strings.Contains(res.Detail, "did not come up") {
		t.Errorf("Check() detail = %q, want the readiness failure", res.Detail)
	}
	log := readStubLog(t, logPath)
	if last := log[len(log)-1]; !strings.HasPrefix(last, "rm --force ") {
		t.Errorf("last docker invocation = %q, want cleanup even on cancellation", last)
	}
}

// TestContainerRestoreCancelDuringReadySettleDelay: cancelling the
// context while waitReady is re-confirming a first successful probe
// (the readySettleDelay pause, not the between-attempts poll sleep)
// must return promptly instead of waiting out the delay — a regression
// test for sleepOrDone actually selecting on ctx.Done() rather than
// blindly time.Sleep-ing.
func TestContainerRestoreCancelDuringReadySettleDelay(t *testing.T) {
	opts, logPath, _ := stubSetup(t) // default STUB_READY: every probe succeeds
	artifact := sqlArtifact(t, "database", "SELECT 1;\n")

	entry := containerRestoreEntry("postgres:16")
	checks, err := For(entry, opts)
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	c := checks[0]
	cr := c.(*ContainerRestore)
	cr.ReadyTimeout = time.Minute // must not be reached

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	res := cr.Check(ctx, Request{
		Service:   entry.Service,
		Artifacts: []strategy.Artifact{artifact},
	})
	elapsed := time.Since(start)

	if res.Passed {
		t.Fatal("Check() passed with a cancelled context, want failure")
	}
	if !strings.Contains(res.Detail, "did not come up") {
		t.Errorf("Check() detail = %q, want the readiness failure", res.Detail)
	}
	if elapsed >= readySettleDelay {
		t.Errorf("Check() took %s to notice cancellation, want well under the %s settle delay", elapsed, readySettleDelay)
	}
	log := readStubLog(t, logPath)
	if last := log[len(log)-1]; !strings.HasPrefix(last, "rm --force ") {
		t.Errorf("last docker invocation = %q, want cleanup even on cancellation", last)
	}
}

// TestContainerRestoreUnreadableArtifact: an artifact that vanished
// between backup and verification fails loudly.
func TestContainerRestoreUnreadableArtifact(t *testing.T) {
	opts, _, _ := stubSetup(t)
	missing := strategy.Artifact{
		Name:   "database",
		Path:   "/nonexistent/ballast-verify-missing.sql",
		Format: "sql",
	}
	res := runCheck(t, opts, "postgres:16", nil, Request{
		Artifacts: []strategy.Artifact{missing},
	})
	if res.Passed {
		t.Fatal("Check() passed with an unreadable artifact, want failure")
	}
	if !strings.Contains(res.Detail, "database") {
		t.Errorf("Check() detail = %q, want the artifact named", res.Detail)
	}
}

// TestContainerRestoreWarningsJoin: independent warnings (image drift
// plus cleanup failure) are both recorded, not overwritten.
func TestContainerRestoreWarningsJoin(t *testing.T) {
	opts, _, withEnv := stubSetup(t)
	withEnv("STUB_SOURCE_IMAGE=postgres:15", "STUB_FAIL_RM=1")
	artifact := sqlArtifact(t, "database", "SELECT 1;\n")

	res := runCheck(t, opts, "postgres:16", nil, Request{
		Artifacts:       []strategy.Artifact{artifact},
		SourceContainer: "db",
	})
	if !res.Passed {
		t.Fatalf("Check() failed: %q", res.Detail)
	}
	if !strings.Contains(res.Warning, "runs image postgres:15") ||
		!strings.Contains(res.Warning, "could not be removed") {
		t.Errorf("Check() warning = %q, want both the drift and the cleanup failure", res.Warning)
	}
}

// TestSQLArtifacts checks artifact selection: "sql" and unspecified
// formats are restored, everything else is left for other checkers.
func TestSQLArtifacts(t *testing.T) {
	in := []strategy.Artifact{
		{Name: "a", Path: "/x", Format: "sql"},
		{Name: "b", Path: "/y"},
		{Name: "c", Path: "/z", Format: "SQL"},
		{Name: "d", Path: "/w", Format: "tar"},
	}
	got := sqlArtifacts(in)
	if len(got) != 3 {
		t.Errorf("sqlArtifacts() picked %d artifacts, want 3 (sql, unspecified, SQL)", len(got))
	}
	for _, g := range got {
		if g.Name == "d" {
			t.Errorf("sqlArtifacts() wrongly picked the tar artifact")
		}
	}
}

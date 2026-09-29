package verify

import (
	"github.com/springledev/keel/dockerhost"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/springledev/ballast/internal/catalog"
	"github.com/springledev/ballast/internal/strategy"
	"github.com/springledev/keel/runner"
)

// dockerStub is a fake docker CLI covering every subcommand the
// container-restore checker runs: inspect, run, exec (readiness
// probes and restore clients), rm. Behaviour is steered through
// environment variables so tests stay hermetic — no docker daemon is
// needed:
//
//	STUB_LOG             one argv line per invocation (always)
//	STUB_SOURCE_IMAGE    `inspect --format {{.Image}}` answers this
//	STUB_FAIL_INSPECT    inspect exits 1
//	STUB_FAIL_RUN        run exits 1
//	STUB_READY=no        readiness probes exit 1
//	STUB_CAPTURE         restore client's stdin is written here
//	STUB_RESTORE_STDERR  restore client writes this to stderr
//	STUB_RESTORE_EXIT    restore client's exit status
//	STUB_FAIL_RM         rm exits 1
const dockerStub = `#!/bin/sh
printf '%s\n' "$*" >> "$STUB_LOG"
cmd="$1"

case "$cmd" in
  inspect)
    if [ -n "$STUB_FAIL_INSPECT" ]; then
      echo "stub: inspect failed" >&2
      exit 1
    fi
    printf '%s\n' "$STUB_SOURCE_IMAGE"
    exit 0
    ;;
  run)
    if [ -n "$STUB_FAIL_RUN" ]; then
      echo "stub: run failed" >&2
      exit 1
    fi
    exit 0
    ;;
  exec)
    case "$*" in
      *pg_isready*|*mysqladmin*)
        if [ "$STUB_READY" = "no" ]; then
          exit 1
        fi
        exit 0
        ;;
      *psql*|*mysql*)
        if [ -n "$STUB_CAPTURE" ]; then
          cat > "$STUB_CAPTURE"
        else
          cat > /dev/null
        fi
        printf '%s\n' "$STUB_RESTORE_STDERR" >&2
        exit "${STUB_RESTORE_EXIT:-0}"
        ;;
      *)
        exit 0
        ;;
    esac
    ;;
  rm)
    if [ -n "$STUB_FAIL_RM" ]; then
      echo "stub: rm failed" >&2
      exit 1
    fi
    exit 0
    ;;
esac
exit 0
`

// stubSetup installs the docker stub and returns the options to give
// For, the invocation log path, and a function adding stub-steering
// environment to the runner's BaseEnv.
func stubSetup(t *testing.T) (opts Options, logPath string, withEnv func(...string)) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "docker.log")
	bin := filepath.Join(dir, "docker-stub")
	if err := os.WriteFile(bin, []byte(dockerStub), 0o755); err != nil {
		t.Fatalf("writing docker stub: %v", err)
	}
	r := &runner.Runner{BaseEnv: append(os.Environ(), "STUB_LOG="+logPath)}
	return Options{Runner: r, Docker: dockerhost.NewLocal(bin)}, logPath, func(extra ...string) {
		r.BaseEnv = append(append([]string(nil), r.BaseEnv...), extra...)
	}
}

// readStubLog returns the recorded docker invocations, one per line.
func readStubLog(t *testing.T, logPath string) []string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("reading stub log: %v", err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

// sqlArtifact writes a dump artifact file and returns its Artifact.
func sqlArtifact(t *testing.T, name, content string) strategy.Artifact {
	t.Helper()
	path := filepath.Join(t.TempDir(), name+".sql")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing artifact: %v", err)
	}
	return strategy.Artifact{Name: name, Path: path, Format: "sql"}
}

// containerRestoreEntry builds a minimal entry with container-restore
// verification.
func containerRestoreEntry(image string, filter ...string) *catalog.Entry {
	return &catalog.Entry{
		Service: "immich",
		Verify: catalog.Verify{
			Method:       "container-restore",
			Image:        image,
			FilterStderr: filter,
		},
	}
}

// containerName extracts the throwaway container name from the
// recorded `docker run` invocation.
func containerName(t *testing.T, log []string) string {
	t.Helper()
	for _, line := range log {
		fields := strings.Fields(line)
		for i, f := range fields {
			if f == "--name" && i+1 < len(fields) {
				return fields[i+1]
			}
		}
	}
	t.Fatalf("stub log has no docker run --name invocation: %v", log)
	return ""
}

// TestForValidation walks the For registry's entry-validation rules:
// every method rejects a malformed entry with a plain-English error
// naming the service, unregistered methods fail loudly (never
// silently skip verification), and well-formed entries are accepted.
func TestForValidation(t *testing.T) {
	r := &runner.Runner{}
	archiveEntry := &catalog.Entry{Service: "svc", Verify: catalog.Verify{Method: "archive-integrity"}}
	containerEntry := containerRestoreEntry("postgres:16")
	smokeEntry := &catalog.Entry{Service: "svc", Verify: catalog.Verify{
		Method: "restart-smoke",
		Smoke:  &catalog.Smoke{Container: "svc_app"},
	}}

	tests := []struct {
		name       string
		entry      *catalog.Entry
		opts       Options
		wantErr    string // substring; empty means success
		wantMethod string // Method() of the sole checker on success
	}{
		{name: "nil entry", entry: nil, opts: Options{Runner: r}, wantErr: "must not be nil"},
		{name: "archive integrity ok", entry: archiveEntry, opts: Options{}, wantMethod: MethodArchiveIntegrity},
		{name: "archive integrity needs no runner", entry: archiveEntry, opts: Options{Runner: r}, wantMethod: MethodArchiveIntegrity},
		{
			name:    "container-restore without runner",
			entry:   containerEntry,
			opts:    Options{},
			wantErr: "db-restore-drill verification needs a command runner",
		},
		{
			name:  "container-restore without image",
			entry: &catalog.Entry{Service: "immich", Verify: catalog.Verify{Method: "container-restore"}},
			opts:  Options{Runner: r}, wantErr: "needs verify.image",
		},
		{name: "container-restore ok (legacy name)", entry: containerEntry, opts: Options{Runner: r}, wantMethod: MethodDBRestoreDrill},
		{name: "restart-smoke ok", entry: smokeEntry, opts: Options{Runner: r}, wantMethod: MethodRestartSmoke},
		{
			name:  "read-back is not available",
			entry: &catalog.Entry{Service: "svc", Verify: catalog.Verify{Method: "read-back"}},
			opts:  Options{Runner: r}, wantErr: `verification method "read-back" is not available`,
		},
		{
			name:  "exec-script is not available",
			entry: &catalog.Entry{Service: "svc", Verify: catalog.Verify{Method: "exec-script"}},
			opts:  Options{Runner: r}, wantErr: `verification method "exec-script" is not available`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkers, err := For(tt.entry, tt.opts)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("For() = error %v, want nil", err)
				}
				if len(checkers) != 1 {
					t.Fatalf("For() = %d checkers, want implementation", len(checkers))
				}
				if got := checkers[0].Method(); got != tt.wantMethod {
					t.Errorf("Method() = %q, want %q", got, tt.wantMethod)
				}
				return
			}
			if err == nil {
				t.Fatalf("For() = nil error, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("For() error = %q, want substring %q", err, tt.wantErr)
			}
			if tt.entry != nil && !strings.Contains(err.Error(), tt.entry.Service) {
				t.Errorf("For() error = %q, want it to name the service %q", err, tt.entry.Service)
			}
		})
	}
}

// TestForUnknownMethod checks that an unregistered method's error also
// lists the methods that ARE available, so the failure is actionable.
func TestForUnknownMethod(t *testing.T) {
	entry := &catalog.Entry{Service: "svc", Verify: catalog.Verify{Method: "read-back"}}
	_, err := For(entry, Options{})
	if err == nil {
		t.Fatal("For() = nil error, want an error naming the unavailable method")
	}
	if !strings.Contains(err.Error(), "read-back") {
		t.Errorf("For() error = %q, want it to name the method", err)
	}
	if !strings.Contains(err.Error(), MethodArchiveIntegrity) {
		t.Errorf("For() error = %q, want it to list the available methods", err)
	}
}

// TestForEmptyMethodList: a catalog entry declaring no verification
// method at all is a hard error, never a silent no-op check list.
func TestForEmptyMethodList(t *testing.T) {
	entry := &catalog.Entry{Service: "svc"}
	checkers, err := For(entry, Options{})
	if err == nil {
		t.Fatal("For() = nil error, want an error for a declared-nothing entry")
	}
	if checkers != nil {
		t.Errorf("For() checkers = %v, want nil on error", checkers)
	}
	if !strings.Contains(err.Error(), "svc") {
		t.Errorf("For() error = %q, want it to name the service", err)
	}
}

// TestForMultipleMethods: an entry declaring several methods gets one
// checker per method, in catalog order, and every one must have
// succeeded to build (a factory failure for method N aborts the whole
// list rather than returning a partial one).
func TestForMultipleMethods(t *testing.T) {
	entry := &catalog.Entry{
		Service: "svc",
		Verify: catalog.Verify{
			Method:  "restart-smoke",
			Methods: []string{"archive-integrity", "container-restore"},
			Image:   "postgres:16",
			Smoke:   &catalog.Smoke{Container: "svc_app"},
		},
	}
	checkers, err := For(entry, Options{Runner: &runner.Runner{}})
	if err != nil {
		t.Fatalf("For() = error %v, want nil", err)
	}
	want := []string{MethodRestartSmoke, MethodArchiveIntegrity, MethodDBRestoreDrill}
	if len(checkers) != len(want) {
		t.Fatalf("For() = %d checkers, want %d", len(checkers), len(want))
	}
	for i, m := range want {
		if got := checkers[i].Method(); got != m {
			t.Errorf("checker[%d].Method() = %q, want %q (catalog order)", i, got, m)
		}
	}
}

// TestForLegacyMethodMapping: the pre-registry "container-restore"
// name still resolves, reporting itself as "db-restore-drill".
func TestForLegacyMethodMapping(t *testing.T) {
	entry := containerRestoreEntry("postgres:16")
	checkers, err := For(entry, Options{Runner: &runner.Runner{}})
	if err != nil {
		t.Fatalf("For() = error %v, want nil", err)
	}
	if len(checkers) != 1 {
		t.Fatalf("For() = %d checkers, want 1", len(checkers))
	}
	if got := checkers[0].Method(); got != MethodDBRestoreDrill {
		t.Errorf("Method() = %q, want %q", got, MethodDBRestoreDrill)
	}
}

// TestForDuplicateMethodsDedupe: declaring the same method twice (once
// as Method, once in Methods, or twice in Methods) yields one checker,
// not two runs of the same check.
func TestForDuplicateMethodsDedupe(t *testing.T) {
	entry := &catalog.Entry{
		Service: "svc",
		Verify: catalog.Verify{
			Method:  "archive-integrity",
			Methods: []string{"archive-integrity", "archive-integrity"},
		},
	}
	checkers, err := For(entry, Options{})
	if err != nil {
		t.Fatalf("For() = error %v, want nil", err)
	}
	if len(checkers) != 1 {
		t.Fatalf("For() = %d checkers, want 1 (deduped)", len(checkers))
	}
}

// TestRegistryCustomRegister: Register lets a caller add or replace a
// method's factory, and Methods reports it.
func TestRegistryCustomRegister(t *testing.T) {
	r := &Registry{}
	called := false
	r.Register("custom-check", func(entry *catalog.Entry, opts Options) (Checker, error) {
		called = true
		return ArchiveIntegrity{}, nil
	})
	if got := r.Methods(); len(got) != 1 || got[0] != "custom-check" {
		t.Fatalf("Methods() = %v, want [custom-check]", got)
	}
	entry := &catalog.Entry{Service: "svc", Verify: catalog.Verify{Method: "custom-check"}}
	checkers, err := r.For(entry, Options{})
	if err != nil {
		t.Fatalf("For() = error %v, want nil", err)
	}
	if !called {
		t.Error("custom factory was not invoked")
	}
	if len(checkers) != 1 {
		t.Fatalf("For() = %d checkers, want 1", len(checkers))
	}

	// Register replaces an existing factory (e.g. overriding a
	// built-in for a test double).
	r2 := DefaultRegistry()
	replaced := false
	r2.Register(MethodArchiveIntegrity, func(entry *catalog.Entry, opts Options) (Checker, error) {
		replaced = true
		return ArchiveIntegrity{}, nil
	})
	if _, err := r2.For(&catalog.Entry{Service: "svc", Verify: catalog.Verify{Method: MethodArchiveIntegrity}}, Options{}); err != nil {
		t.Fatalf("For() = error %v, want nil", err)
	}
	if !replaced {
		t.Error("Register did not replace the built-in factory")
	}
}

// TestRestartSmokeFactoryValidation covers newRestartSmoke's entry
// checks: a method that cannot run must be a hard error, never a
// silently-skipped check.
func TestRestartSmokeFactoryValidation(t *testing.T) {
	r := &runner.Runner{}
	tests := []struct {
		name    string
		entry   *catalog.Entry
		opts    Options
		wantErr string
	}{
		{
			name:    "missing smoke config",
			entry:   &catalog.Entry{Service: "svc", Verify: catalog.Verify{Method: "restart-smoke"}},
			opts:    Options{Runner: r},
			wantErr: "needs verify.smoke.container",
		},
		{
			name: "smoke without container",
			entry: &catalog.Entry{Service: "svc", Verify: catalog.Verify{
				Method: "restart-smoke", Smoke: &catalog.Smoke{},
			}},
			opts:    Options{Runner: r},
			wantErr: "needs verify.smoke.container",
		},
		{
			name: "missing runner",
			entry: &catalog.Entry{Service: "svc", Verify: catalog.Verify{
				Method: "restart-smoke", Smoke: &catalog.Smoke{Container: "svc_app"},
			}},
			opts:    Options{},
			wantErr: "restart-smoke verification needs a command runner",
		},
		{
			name: "bad timeout",
			entry: &catalog.Entry{Service: "svc", Verify: catalog.Verify{
				Method: "restart-smoke",
				Smoke:  &catalog.Smoke{Container: "svc_app", Timeout: "not-a-duration"},
			}},
			opts:    Options{Runner: r},
			wantErr: "not a valid duration",
		},
		{
			name: "http and tcp both set",
			entry: &catalog.Entry{Service: "svc", Verify: catalog.Verify{
				Method: "restart-smoke",
				Smoke: &catalog.Smoke{
					Container: "svc_app",
					HTTP:      &catalog.SmokeHTTP{Port: 80},
					TCP:       &catalog.SmokeTCP{Port: 80},
				},
			}},
			opts:    Options{Runner: r},
			wantErr: "not both",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := For(tt.entry, tt.opts)
			if err == nil {
				t.Fatalf("For() = nil error, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("For() error = %q, want substring %q", err, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.entry.Service) {
				t.Errorf("For() error = %q, want it to name the service %q", err, tt.entry.Service)
			}
		})
	}
}

// TestDockerBinaryDefault mirrors the strategy package's check: the
// docker CLI name defaults to "docker" and honours the override.
func TestDockerBinaryDefault(t *testing.T) {
	if got := (Options{}).Docker.Bin(); got != "docker" {
		t.Errorf("default docker = %q, want docker", got)
	}
	if got := (Options{Docker: dockerhost.NewLocal("/stub/docker")}).Docker.Bin(); got != "/stub/docker" {
		t.Errorf("docker = %q, want /stub/docker", got)
	}
}

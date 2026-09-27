package verify

import (
	"github.com/springledev/ballast/internal/dockerhost"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/springledev/ballast/internal/catalog"
	"github.com/springledev/ballast/internal/runner"
	"github.com/springledev/ballast/internal/strategy"
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
// naming the service, unimplemented methods fail loudly (never
// silently skip verification), and well-formed entries are accepted.
func TestForValidation(t *testing.T) {
	r := &runner.Runner{}
	archiveEntry := &catalog.Entry{Service: "svc", Verify: catalog.Verify{Method: "archive-integrity"}}
	containerEntry := containerRestoreEntry("postgres:16")

	tests := []struct {
		name    string
		entry   *catalog.Entry
		opts    Options
		wantErr string // substring; empty means success
	}{
		{name: "nil entry", entry: nil, opts: Options{Runner: r}, wantErr: "must not be nil"},
		{name: "archive integrity ok", entry: archiveEntry, opts: Options{}},
		{name: "archive integrity needs no runner", entry: archiveEntry, opts: Options{Runner: r}},
		{
			name:    "container-restore without runner",
			entry:   containerEntry,
			opts:    Options{},
			wantErr: "container-restore verification needs a command runner",
		},
		{
			name:  "container-restore without image",
			entry: &catalog.Entry{Service: "immich", Verify: catalog.Verify{Method: "container-restore"}},
			opts:  Options{Runner: r}, wantErr: "needs verify.image",
		},
		{name: "container-restore ok", entry: containerEntry, opts: Options{Runner: r}},
		{
			name:  "read-back not implemented yet",
			entry: &catalog.Entry{Service: "svc", Verify: catalog.Verify{Method: "read-back"}},
			opts:  Options{Runner: r}, wantErr: "not implemented yet",
		},
		{
			name:  "restart-smoke not implemented yet",
			entry: &catalog.Entry{Service: "svc", Verify: catalog.Verify{Method: "restart-smoke"}},
			opts:  Options{Runner: r}, wantErr: "not implemented yet",
		},
		{
			name:  "exec-script not implemented yet",
			entry: &catalog.Entry{Service: "svc", Verify: catalog.Verify{Method: "exec-script"}},
			opts:  Options{Runner: r}, wantErr: "not implemented yet",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := For(tt.entry, tt.opts)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("For() = error %v, want nil", err)
				}
				if c == nil {
					t.Fatal("For() = nil checker, want implementation")
				}
				if c.Method() != tt.entry.Verify.Method {
					t.Errorf("Method() = %q, want %q", c.Method(), tt.entry.Verify.Method)
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

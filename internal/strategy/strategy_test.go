package strategy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/springledev/ballast/internal/catalog"
	"github.com/springledev/ballast/internal/runner"
)

// writeStub writes an executable stub script and returns its absolute
// path. Stubs stand in for docker and for user exec scripts, so the
// strategy tests are hermetic: no docker daemon is needed.
func writeStub(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("writing stub %s: %v", path, err)
	}
	return path
}

// dockerStub is a fake docker CLI. It appends every invocation's argv
// to $STUB_LOG (one line per call), fails when any argument is
// "fail-<subcommand>" (so "fail-stop" only breaks docker stop), and
// answers `docker exec ...` with a small SQL dump on stdout.
const dockerStub = `#!/bin/sh
printf '%s\n' "$*" >> "$STUB_LOG"
cmd="$1"
for a in "$@"; do
  if [ "$a" = "fail-$cmd" ]; then
    echo "stub: simulated $cmd failure" >&2
    exit 1
  fi
done
case "$cmd" in
  exec) printf 'CREATE TABLE stub(id int);\n' ;;
esac
exit 0
`

// newDockerStub installs the docker stub in dir and returns its path
// plus the path of the log file it records invocations in.
func newDockerStub(t *testing.T) (bin, logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "docker.log")
	bin = writeStub(t, dir, "docker-stub", dockerStub)
	return bin, logPath
}

// stubRunner returns a runner whose environment carries STUB_LOG so
// the docker stub records into logPath.
func stubRunner(logPath string) *runner.Runner {
	return &runner.Runner{BaseEnv: append(os.Environ(), "STUB_LOG="+logPath)}
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

// entryFor builds a minimal catalog entry for one strategy.
func entryFor(strategy string, mutate func(*catalog.Entry)) *catalog.Entry {
	e := &catalog.Entry{Service: "svc", Backup: catalog.Backup{Strategy: strategy}}
	if mutate != nil {
		mutate(e)
	}
	return e
}

// TestForValidation walks the entry-validation rules of For: every
// strategy rejects a malformed entry with a plain-English error that
// names the service, and accepts a well-formed one.
func TestForValidation(t *testing.T) {
	r := &runner.Runner{}
	withFiles := func(e *catalog.Entry) {
		e.Backup.Files = []catalog.FileSpec{{Path: "/srv/data"}}
	}
	withDumps := func(e *catalog.Entry) {
		e.Backup.Dumps = []catalog.Dump{{Name: "database", Command: []string{"pg_dumpall"}}}
	}

	tests := []struct {
		name    string
		entry   *catalog.Entry
		opts    Options
		wantErr string // substring; empty means success
	}{
		{
			name:    "nil entry",
			entry:   nil,
			opts:    Options{Runner: r},
			wantErr: "must not be nil",
		},
		{
			name:    "unknown strategy",
			entry:   entryFor("teleport", nil),
			opts:    Options{Runner: r},
			wantErr: `unknown backup strategy "teleport"`,
		},
		{
			name:    "files without files",
			entry:   entryFor("files", nil),
			opts:    Options{Runner: r},
			wantErr: "files strategy needs at least one backup.files entry",
		},
		{
			name:  "files ok",
			entry: entryFor("files", withFiles),
			opts:  Options{}, // files spawns no commands: runner not required
		},
		{
			name:    "dump without runner",
			entry:   entryFor("dump", withDumps),
			opts:    Options{},
			wantErr: "dump strategy needs a command runner",
		},
		{
			name:    "dump without dumps",
			entry:   entryFor("dump", nil),
			opts:    Options{Runner: r},
			wantErr: "dump strategy needs at least one backup.dumps entry",
		},
		{
			name: "dump with path-separator name",
			entry: entryFor("dump", func(e *catalog.Entry) {
				e.Backup.Dumps = []catalog.Dump{{Name: "../escape", Command: []string{"x"}}}
			}),
			opts:    Options{Runner: r},
			wantErr: `must be a plain file name`,
		},
		{
			name: "dump with empty command",
			entry: entryFor("dump", func(e *catalog.Entry) {
				e.Backup.Dumps = []catalog.Dump{{Name: "database"}}
			}),
			opts:    Options{Runner: r},
			wantErr: `dump "database" has an empty command`,
		},
		{
			name: "dump with empty name",
			entry: entryFor("dump", func(e *catalog.Entry) {
				e.Backup.Dumps = []catalog.Dump{{Command: []string{"x"}}}
			}),
			opts:    Options{Runner: r},
			wantErr: "must not be empty",
		},
		{
			name: "dump with path-separator format",
			entry: entryFor("dump", func(e *catalog.Entry) {
				e.Backup.Dumps = []catalog.Dump{{Name: "database", Command: []string{"x"}, Format: "a/b"}}
			}),
			opts:    Options{Runner: r},
			wantErr: "must not contain a path separator",
		},
		{
			name:  "dump ok",
			entry: entryFor("dump", withDumps),
			opts:  Options{Runner: r},
		},
		{
			name:    "stop-start without runner",
			entry:   entryFor("stop-start", nil),
			opts:    Options{},
			wantErr: "stop-start strategy needs a command runner",
		},
		{
			name:    "stop-start without containers",
			entry:   entryFor("stop-start", withFiles),
			opts:    Options{Runner: r},
			wantErr: "stop-start strategy needs backup.stopStart.containers",
		},
		{
			name: "stop-start without files",
			entry: entryFor("stop-start", func(e *catalog.Entry) {
				e.Backup.StopStart = &catalog.StopStart{Containers: []string{"vw"}}
			}),
			opts:    Options{Runner: r},
			wantErr: "needs at least one backup.files data directory",
		},
		{
			name: "stop-start ok",
			entry: entryFor("stop-start", func(e *catalog.Entry) {
				e.Backup.StopStart = &catalog.StopStart{Containers: []string{"vw"}}
				withFiles(e)
			}),
			opts: Options{Runner: r},
		},
		{
			name: "exec without runner",
			entry: entryFor("exec", func(e *catalog.Entry) {
				e.Backup.Exec = &catalog.ExecBackup{Script: "/opt/backup.sh"}
			}),
			opts:    Options{},
			wantErr: "exec strategy needs a command runner",
		},
		{
			name:    "exec without script",
			entry:   entryFor("exec", nil),
			opts:    Options{Runner: r},
			wantErr: "exec strategy needs backup.exec.script",
		},
		{
			name: "exec with relative script",
			entry: entryFor("exec", func(e *catalog.Entry) {
				e.Backup.Exec = &catalog.ExecBackup{Script: "backup.sh"}
			}),
			opts:    Options{Runner: r},
			wantErr: "must be an absolute path",
		},
		{
			name: "exec ok",
			entry: entryFor("exec", func(e *catalog.Entry) {
				e.Backup.Exec = &catalog.ExecBackup{Script: "/opt/backup.sh"}
			}),
			opts: Options{Runner: r},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := For(tt.entry, tt.opts)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("For() = error %v, want nil", err)
				}
				if s == nil {
					t.Fatal("For() = nil strategy, want implementation")
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

// TestDockerBinaryDefault checks the docker CLI name defaults to
// "docker" and honours the override (used by tests to inject stubs).
func TestDockerBinaryDefault(t *testing.T) {
	if got := (Options{}).docker(); got != "docker" {
		t.Errorf("default docker binary = %q, want %q", got, "docker")
	}
	if got := (Options{DockerBin: "/stub/docker"}).docker(); got != "/stub/docker" {
		t.Errorf("overridden docker binary = %q, want %q", got, "/stub/docker")
	}
}

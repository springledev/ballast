package strategy

import (
	"archive/tar"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/springledev/ballast/internal/catalog"
)

// tarEntry is one read-back member of a tar archive, for assertions.
type tarEntry struct {
	Typeflag byte
	Linkname string
	Body     string
}

// readTar reads an archive into a name → entry map.
func readTar(t *testing.T, path string) map[string]tarEntry {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("opening archive: %v", err)
	}
	defer f.Close()
	out := map[string]tarEntry{}
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("reading archive: %v", err)
		}
		e := tarEntry{Typeflag: hdr.Typeflag, Linkname: hdr.Linkname}
		if hdr.Typeflag == tar.TypeReg {
			body, err := io.ReadAll(tr)
			if err != nil {
				t.Fatalf("reading %s from archive: %v", hdr.Name, err)
			}
			e.Body = string(body)
		}
		out[hdr.Name] = e
	}
	return out
}

// stopStartFixture builds a data directory with a regular file, a
// nested file, a symlink, and a socket (which cannot be archived and
// must be reported as skipped).
func stopStartFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	data := filepath.Join(root, "data")
	if err := os.MkdirAll(filepath.Join(data, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "db.sqlite3"), []byte("sqlite-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "sub", "nested.txt"), []byte("nested"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("db.sqlite3", filepath.Join(data, "link")); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", filepath.Join(data, "control.sock"))
	if err != nil {
		t.Fatalf("creating test socket: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	return data
}

func stopStartEntry(dataDir string, containers ...string) *catalog.Entry {
	return &catalog.Entry{
		Service: "vaultwarden",
		Backup: catalog.Backup{
			Strategy:  "stop-start",
			StopStart: &catalog.StopStart{Containers: containers},
			Files:     []catalog.FileSpec{{Path: dataDir}},
		},
	}
}

// TestStopStartHappyPath verifies the whole cycle: containers are
// stopped in catalog order and started again, and the tar artifact
// holds the data directory's files, directories and symlinks.
func TestStopStartHappyPath(t *testing.T) {
	dockerBin, logPath := newDockerStub(t)
	data := stopStartFixture(t)
	entry := stopStartEntry(data, "vw", "vw-db")

	s, err := For(entry, Options{Runner: stubRunner(logPath), DockerBin: dockerBin})
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	res, err := s.Backup(context.Background(), Request{WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	// Containers stopped in order, then started in order.
	log := readStubLog(t, logPath)
	want := []string{"stop vw", "stop vw-db", "start vw", "start vw-db"}
	if strings.Join(log, "|") != strings.Join(want, "|") {
		t.Errorf("docker invocations = %v, want %v", log, want)
	}

	// One tar artifact named data.tar.
	if len(res.Artifacts) != 1 {
		t.Fatalf("Artifacts = %v, want exactly one", res.Artifacts)
	}
	a := res.Artifacts[0]
	if a.Name != "data" || a.Format != "tar" || filepath.Base(a.Path) != "data.tar" {
		t.Errorf("Artifact = %+v, want data.tar (Name=data Format=tar)", a)
	}
	info, err := os.Stat(a.Path)
	if err != nil {
		t.Fatalf("stat artifact: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("artifact permissions = %04o, want 0600", perm)
	}

	// Archive content: everything under the data dir, rooted at its
	// base name.
	members := readTar(t, a.Path)
	if e, ok := members["data/"]; !ok || e.Typeflag != tar.TypeDir {
		t.Errorf("archive missing data/ dir entry: %v", members)
	}
	if e, ok := members["data/db.sqlite3"]; !ok || e.Body != "sqlite-bytes" {
		t.Errorf("archive data/db.sqlite3 = %+v, want body %q", e, "sqlite-bytes")
	}
	if e, ok := members["data/sub/nested.txt"]; !ok || e.Body != "nested" {
		t.Errorf("archive data/sub/nested.txt = %+v, want body %q", e, "nested")
	}
	if e, ok := members["data/link"]; !ok || e.Typeflag != tar.TypeSymlink || e.Linkname != "db.sqlite3" {
		t.Errorf("archive data/link = %+v, want symlink to db.sqlite3", e)
	}

	// The socket was skipped and reported, not silently dropped.
	if len(res.Skipped) != 1 || filepath.Base(res.Skipped[0]) != "control.sock" {
		t.Errorf("Skipped = %v, want the control.sock path", res.Skipped)
	}
	if _, ok := members["data/control.sock"]; ok {
		t.Error("archive contains the socket; it must be skipped")
	}
}

// TestStopStartRestartsOnArchiveFailure kills the archive step (the
// data path does not exist) and asserts the containers are started
// again anyway — a failed backup must never leave a service down.
func TestStopStartRestartsOnArchiveFailure(t *testing.T) {
	dockerBin, logPath := newDockerStub(t)
	entry := stopStartEntry(filepath.Join(t.TempDir(), "missing"), "vw")

	s, err := For(entry, Options{Runner: stubRunner(logPath), DockerBin: dockerBin})
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	_, err = s.Backup(context.Background(), Request{WorkDir: t.TempDir()})
	if err == nil {
		t.Fatal("Backup = nil error, want the archive failure")
	}
	if !strings.Contains(err.Error(), "vaultwarden") {
		t.Errorf("error = %q, want it to name the service", err)
	}
	log := readStubLog(t, logPath)
	want := []string{"stop vw", "start vw"}
	if strings.Join(log, "|") != strings.Join(want, "|") {
		t.Errorf("docker invocations = %v, want %v (restart despite failure)", log, want)
	}
}

// TestStopStartPartialStopFailure stops two containers where the
// second stop fails: the first container must be started again before
// the error returns.
func TestStopStartPartialStopFailure(t *testing.T) {
	dockerBin, logPath := newDockerStub(t)
	data := stopStartFixture(t)
	entry := stopStartEntry(data, "vw", "fail-stop", "never-reached")

	s, err := For(entry, Options{Runner: stubRunner(logPath), DockerBin: dockerBin})
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	_, err = s.Backup(context.Background(), Request{WorkDir: t.TempDir()})
	if err == nil {
		t.Fatal("Backup = nil error, want the stop failure")
	}
	if !strings.Contains(err.Error(), "fail-stop") {
		t.Errorf("error = %q, want it to name the failing container", err)
	}
	log := readStubLog(t, logPath)
	want := []string{"stop vw", "stop fail-stop", "start vw"}
	if strings.Join(log, "|") != strings.Join(want, "|") {
		t.Errorf("docker invocations = %v, want %v", log, want)
	}
}

// TestStopStartReportsRestartFailure makes a container fail to start
// and asserts the restart failure is surfaced loudly, joined with the
// run's outcome rather than masking it.
func TestStopStartReportsRestartFailure(t *testing.T) {
	dockerBin, logPath := newDockerStub(t)
	data := stopStartFixture(t)
	entry := stopStartEntry(data, "fail-start")

	s, err := For(entry, Options{Runner: stubRunner(logPath), DockerBin: dockerBin})
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	_, err = s.Backup(context.Background(), Request{WorkDir: t.TempDir()})
	if err == nil {
		t.Fatal("Backup = nil error, want the restart failure reported")
	}
	if !strings.Contains(err.Error(), "starting container fail-start") {
		t.Errorf("error = %q, want it to name the container that would not start", err)
	}
	log := readStubLog(t, logPath)
	want := []string{"stop fail-start", "start fail-start"}
	if strings.Join(log, "|") != strings.Join(want, "|") {
		t.Errorf("docker invocations = %v, want %v", log, want)
	}
}

// TestStopStartCancelledRun cancels the run before it starts: the
// stop fails as cancelled, and no container is left stopped.
func TestStopStartCancelledRun(t *testing.T) {
	dockerBin, logPath := newDockerStub(t)
	data := stopStartFixture(t)
	entry := stopStartEntry(data, "vw")

	s, err := For(entry, Options{Runner: stubRunner(logPath), DockerBin: dockerBin})
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = s.Backup(ctx, Request{WorkDir: t.TempDir()})
	if err == nil {
		t.Fatal("Backup = nil error, want cancellation")
	}
	// The stop never completed, so nothing may be left stopped and no
	// start is needed; what matters is the error says what happened.
	for _, line := range readStubLog(t, logPath) {
		if strings.HasPrefix(line, "start ") {
			t.Errorf("unexpected docker invocation after failed stop: %v", line)
		}
	}
}

// TestStopStartRejectsBadConfig covers the request/entry validation
// branches: empty work dir and relative data paths must fail loudly.
func TestStopStartRejectsBadConfig(t *testing.T) {
	dockerBin, logPath := newDockerStub(t)
	data := stopStartFixture(t)

	t.Run("empty work dir", func(t *testing.T) {
		s, err := For(stopStartEntry(data, "vw"), Options{Runner: stubRunner(logPath), DockerBin: dockerBin})
		if err != nil {
			t.Fatalf("For: %v", err)
		}
		if _, err := s.Backup(context.Background(), Request{}); err == nil {
			t.Error("Backup with empty work dir = nil error, want failure")
		}
	})

	t.Run("relative data path", func(t *testing.T) {
		entry := stopStartEntry("relative/data", "vw")
		s, err := For(entry, Options{Runner: stubRunner(logPath), DockerBin: dockerBin})
		if err != nil {
			t.Fatalf("For: %v", err)
		}
		_, err = s.Backup(context.Background(), Request{WorkDir: t.TempDir()})
		if err == nil {
			t.Fatal("Backup = nil error, want the relative-path failure")
		}
		if !strings.Contains(err.Error(), "must be absolute") {
			t.Errorf("error = %q, want it to explain absolute paths", err)
		}
		// The container was stopped before the path check failed, so
		// the guaranteed restart must have run.
		log := readStubLog(t, logPath)
		if len(log) != 2 || log[0] != "stop vw" || log[1] != "start vw" {
			t.Errorf("docker invocations = %v, want [stop vw start vw]", log)
		}
	})
}

// TestTarHelpersCancelled covers the cancellation checks inside the
// tar walk: a cancelled context aborts archiving promptly.
func TestTarHelpersCancelled(t *testing.T) {
	data := stopStartFixture(t)
	entry := stopStartEntry(data, "vw")
	s := &StopStart{entry: entry, opts: Options{}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	dest := filepath.Join(t.TempDir(), "data.tar")
	if _, err := s.tarPaths(ctx, dest); err == nil {
		t.Error("tarPaths with cancelled context = nil error, want cancellation")
	}
}

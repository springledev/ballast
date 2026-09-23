package verify

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/springledev/ballast/internal/strategy"
)

// writeTar builds a small tar archive with one member.
func writeTar(t *testing.T, path string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create tar: %v", err)
	}
	defer f.Close()
	w := tar.NewWriter(f)
	body := []byte("hello ballast")
	if err := w.WriteHeader(&tar.Header{Name: "file.txt", Mode: 0o644, Size: int64(len(body))}); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := w.Write(body); err != nil {
		t.Fatalf("tar write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
}

// writeGzip builds a small gzip stream.
func writeGzip(t *testing.T, path string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create gzip: %v", err)
	}
	defer f.Close()
	w := gzip.NewWriter(f)
	if _, err := w.Write([]byte("compressed ballast")); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
}

// writeTarGz builds a gzip-wrapped tar archive.
func writeTarGz(t *testing.T, path string) {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	body := []byte("payload")
	if err := tw.WriteHeader(&tar.Header{Name: "a", Mode: 0o644, Size: int64(len(body))}); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatalf("tar write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write tgz: %v", err)
	}
}

// writeZip builds a small zip archive with one member.
func writeZip(t *testing.T, path string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create zip: %v", err)
	}
	defer f.Close()
	w := zip.NewWriter(f)
	m, err := w.Create("file.txt")
	if err != nil {
		t.Fatalf("zip create: %v", err)
	}
	if _, err := m.Write([]byte("zip ballast")); err != nil {
		t.Fatalf("zip write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
}

// writeZipWithCorruptMember builds a zip whose member's compressed
// data has one byte flipped, so reading it fails the CRC check while
// the central directory still parses.
func writeZipWithCorruptMember(t *testing.T, path string) {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	m, err := w.Create("file.txt")
	if err != nil {
		t.Fatalf("zip create: %v", err)
	}
	if _, err := m.Write([]byte("zip ballast corruption target payload")); err != nil {
		t.Fatalf("zip write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	data := buf.Bytes()
	// The local header is 30 bytes + 8 name bytes; flipping a byte a
	// little into it lands in the compressed payload, not in the
	// header or the central directory.
	data[44] ^= 0xFF
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write corrupt zip: %v", err)
	}
}

// artifactFile writes raw bytes as an artifact.
func artifactFile(t *testing.T, name, format string, data []byte) strategy.Artifact {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	return strategy.Artifact{Name: name, Path: path, Format: format}
}

// TestArchiveIntegrityFormats runs the per-format table: valid
// artifacts pass and structurally broken ones fail with a naming
// error.
func TestArchiveIntegrityFormats(t *testing.T) {
	dir := t.TempDir()

	tarPath := filepath.Join(dir, "good.tar")
	writeTar(t, tarPath)
	gzPath := filepath.Join(dir, "good.gz")
	writeGzip(t, gzPath)
	tgzPath := filepath.Join(dir, "good.tgz")
	writeTarGz(t, tgzPath)
	zipPath := filepath.Join(dir, "good.zip")
	writeZip(t, zipPath)

	// Corrupted variants.
	badTar := artifactFile(t, "bad.tar", "tar", []byte("not a tar at all"))
	badTgz := artifactFile(t, "bad.tgz", "tar.gz", []byte("this is not gzip"))
	badGz := artifactFile(t, "bad.gz", "gzip", []byte("this is not gzip"))
	badZip := artifactFile(t, "bad.zip", "zip", []byte("PK this is not a zip"))

	// A gzip stream truncated mid-way: the header parses but the CRC
	// at the end never verifies.
	truncGzBytes, err := os.ReadFile(gzPath)
	if err != nil {
		t.Fatalf("read gzip: %v", err)
	}
	truncGz := artifactFile(t, "trunc.gz", "gz", truncGzBytes[:len(truncGzBytes)-5])

	// A gzip-wrapped tar whose tar layer is broken inside valid gzip.
	var corruptTarBuf bytes.Buffer
	gw := gzip.NewWriter(&corruptTarBuf)
	if _, err := gw.Write([]byte("definitely not tar headers")); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	badTarGz := artifactFile(t, "badintar.tgz", "tgz", corruptTarBuf.Bytes())

	// A valid tar wrapped in a gzip stream whose trailer is corrupt:
	// the tar layer parses to its end-of-archive marker, so only
	// draining the gzip stream to its CRC32/ISIZE check catches this.
	tgzBytes, err := os.ReadFile(tgzPath)
	if err != nil {
		t.Fatalf("read tgz: %v", err)
	}
	badTrailerBytes := append([]byte(nil), tgzBytes...)
	badTrailerBytes[len(badTrailerBytes)-1] ^= 0xFF // gzip trailer ISIZE
	badTrailerTgz := artifactFile(t, "badtrailer.tgz", "tar.gz", badTrailerBytes)

	// Junk appended after a valid gzip-wrapped tar: the first stream
	// verifies, but the trailing bytes are not a valid continuation
	// stream — again only draining past the tar end marker sees it.
	trailGarbageTgz := artifactFile(t, "trailgarbage.tgz", "tgz",
		append(append([]byte(nil), tgzBytes...), []byte("not-a-gzip-stream")...))

	// A zip member whose CRC does not match its content (zip's own
	// integrity layer must catch it).
	badMemberZip := filepath.Join(dir, "badmember.zip")
	writeZipWithCorruptMember(t, badMemberZip)

	sql := artifactFile(t, "database.sql", "sql", []byte("CREATE TABLE t(id int);\n"))
	emptySql := artifactFile(t, "empty.sql", "sql", nil)
	unknown := artifactFile(t, "data.bin", "weird-format", []byte("some bytes"))
	noFormat := artifactFile(t, "dump.out", "", []byte("untyped dump content"))

	tests := []struct {
		name     string
		artifact strategy.Artifact
		wantErr  string // substring; empty means the check passes
	}{
		{"tar ok", strategy.Artifact{Name: "data.tar", Path: tarPath, Format: "tar"}, ""},
		{"tar.gz ok", strategy.Artifact{Name: "data.tgz", Path: tgzPath, Format: "tgz"}, ""},
		{"gzip ok", strategy.Artifact{Name: "data.gz", Path: gzPath, Format: "gz"}, ""},
		{"zip ok", strategy.Artifact{Name: "data.zip", Path: zipPath, Format: "zip"}, ""},
		{"tar broken", badTar, "tar headers"},
		{"tar.gz not gzip", badTgz, "gzip header"},
		{"tar.gz with broken tar inside", badTarGz, "tar headers"},
		{"tar.gz with corrupt gzip trailer", badTrailerTgz, "gzip stream"},
		{"tar.gz with trailing garbage after the stream", trailGarbageTgz, "gzip stream"},
		{"gzip broken", badGz, "gzip header"},
		{"gzip truncated fails CRC", truncGz, "gzip stream"},
		{"zip broken", badZip, "zip"},
		{"zip member CRC mismatch", strategy.Artifact{Name: "badmember.zip", Path: badMemberZip, Format: "zip"}, "zip member"},
		{"sql ok", sql, ""},
		{"sql empty fails", emptySql, "is empty"},
		{"unknown format passes floor check", unknown, ""},
		{"unspecified format passes floor check", noFormat, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := ArchiveIntegrity{}.Check(context.Background(), Request{
				Service:   "svc",
				Artifacts: []strategy.Artifact{tt.artifact},
			})
			if tt.wantErr == "" {
				if !res.Passed {
					t.Fatalf("Check() failed = %q, want pass", res.Detail)
				}
				return
			}
			if res.Passed {
				t.Fatalf("Check() passed, want failure containing %q", tt.wantErr)
			}
			if !strings.Contains(res.Detail, tt.wantErr) {
				t.Errorf("Check() detail = %q, want substring %q", res.Detail, tt.wantErr)
			}
			if !strings.Contains(res.Detail, "svc") {
				t.Errorf("Check() detail = %q, want it to name the service", res.Detail)
			}
		})
	}
}

// TestArchiveIntegrityMissingAndNonRegular covers the floor checks:
// missing files and directories-as-artifacts fail loudly.
func TestArchiveIntegrityMissingAndNonRegular(t *testing.T) {
	dir := t.TempDir()
	res := ArchiveIntegrity{}.Check(context.Background(), Request{
		Service:   "svc",
		Artifacts: []strategy.Artifact{{Name: "gone", Path: filepath.Join(dir, "missing"), Format: "sql"}},
	})
	if res.Passed || !strings.Contains(res.Detail, "could not be read") {
		t.Errorf("missing artifact: passed=%v detail=%q, want a \"could not be read\" failure", res.Passed, res.Detail)
	}

	res = ArchiveIntegrity{}.Check(context.Background(), Request{
		Service:   "svc",
		Artifacts: []strategy.Artifact{{Name: "dir", Path: dir, Format: "tar"}},
	})
	if res.Passed || !strings.Contains(res.Detail, "not a regular file") {
		t.Errorf("directory artifact: passed=%v detail=%q, want a \"not a regular file\" failure", res.Passed, res.Detail)
	}
}

// TestArchiveIntegrityNoArtifacts refuses to verify an empty run —
// "no artifacts" can never mean "verified" (spec §4.1).
func TestArchiveIntegrityNoArtifacts(t *testing.T) {
	res := ArchiveIntegrity{}.Check(context.Background(), Request{Service: "svc"})
	if res.Passed {
		t.Fatal("Check() with no artifacts passed, want failure")
	}
	if !strings.Contains(res.Detail, "no artifacts") {
		t.Errorf("Check() detail = %q, want it to say nothing was produced", res.Detail)
	}
}

// TestArchiveIntegrityAggregates checks multiple artifacts are all
// inspected, the passing detail counts them, the output lists them,
// and the first failure stops the run with the artifact named.
func TestArchiveIntegrityAggregates(t *testing.T) {
	a := artifactFile(t, "a.sql", "sql", []byte("SELECT 1;\n"))
	b := artifactFile(t, "b.sql", "sql", []byte("SELECT 2;\n"))

	res := ArchiveIntegrity{}.Check(context.Background(), Request{
		Service:   "svc",
		Artifacts: []strategy.Artifact{a, b},
	})
	if !res.Passed {
		t.Fatalf("Check() = %q, want pass", res.Detail)
	}
	if !strings.Contains(res.Detail, "2 artifact(s)") {
		t.Errorf("Check() detail = %q, want the artifact count", res.Detail)
	}
	if !strings.Contains(res.Output, "a.sql") || !strings.Contains(res.Output, "b.sql") {
		t.Errorf("Check() output = %q, want both artifacts listed", res.Output)
	}

	broken := artifactFile(t, "broken.tar", "tar", []byte("junk"))
	res = ArchiveIntegrity{}.Check(context.Background(), Request{
		Service:   "svc",
		Artifacts: []strategy.Artifact{a, broken, b},
	})
	if res.Passed {
		t.Fatal("Check() passed with a broken middle artifact, want failure")
	}
	if !strings.Contains(res.Detail, `"broken.tar"`) {
		t.Errorf("Check() detail = %q, want the failing artifact named", res.Detail)
	}
}

// TestArchiveIntegrityCancel stops mid-run when the context is
// cancelled and never reports a pass.
func TestArchiveIntegrityCancel(t *testing.T) {
	a := artifactFile(t, "a.sql", "sql", []byte("SELECT 1;\n"))
	b := artifactFile(t, "b.sql", "sql", []byte("SELECT 2;\n"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := ArchiveIntegrity{}.Check(ctx, Request{
		Service:   "svc",
		Artifacts: []strategy.Artifact{a, b},
	})
	if res.Passed {
		t.Fatal("Check() passed with a cancelled context, want failure")
	}
	if !strings.Contains(res.Detail, "cancelled") {
		t.Errorf("Check() detail = %q, want it to name the cancellation", res.Detail)
	}
}

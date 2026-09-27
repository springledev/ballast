package verify

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/springledev/ballast/internal/strategy"
)

// ArchiveIntegrity implements the mandatory-minimum check of
// spec §7.1: every artifact of the run is readable and, where the
// format is a container format, structurally sound:
//
//   - tar / tar.gz / tgz: headers walk to EOF (gzip-wrapped tar is
//     uncompressed first, so the checksum is verified too);
//   - gz / gzip: the gzip stream's CRC is verified to EOF;
//   - zip: every member opens and fully decompresses;
//   - anything else (sql, text, unspecified): the file must exist,
//     be regular, and be non-empty — real proof for database dumps
//     comes from db-restore-drill, this is the floor every entry
//     must clear (spec §6 schema rules).
//
// It uses the standard library for the structure checks — verifying a
// tar/gzip/zip file is not "rebuilding restic" — and spawns no
// commands at all.
type ArchiveIntegrity struct{}

// Method implements Checker.
func (ArchiveIntegrity) Method() string { return "archive-integrity" }

// Check inspects every artifact and aggregates the outcome.
func (c ArchiveIntegrity) Check(ctx context.Context, req Request) Result {
	start := time.Now()
	res := Result{Method: c.Method()}
	if len(req.Artifacts) == 0 {
		res.Detail = "service " + req.Service + ": the backup produced no artifacts, so there is nothing to verify"
		res.Duration = time.Since(start)
		return res
	}

	var checked []string
	for _, a := range req.Artifacts {
		if err := ctx.Err(); err != nil {
			res.Detail = "service " + req.Service + ": verification cancelled: " + err.Error()
			res.Output = strings.Join(checked, "\n")
			res.Duration = time.Since(start)
			return res
		}
		n, err := inspect(a)
		if err != nil {
			res.Detail = fmt.Sprintf("service %s: artifact %q (%s): %v", req.Service, a.Name, a.Path, err)
			res.Output = strings.Join(checked, "\n")
			res.Duration = time.Since(start)
			return res
		}
		checked = append(checked, fmt.Sprintf("%s (%s, %d bytes)", a.Name, label(a.Format), n))
	}
	res.Passed = true
	res.Detail = fmt.Sprintf("service %s: %d artifact(s) checked OK", req.Service, len(req.Artifacts))
	res.Output = strings.Join(checked, "\n")
	res.Duration = time.Since(start)
	return res
}

// label renders a format for human output ("" → "untyped").
func label(format string) string {
	if format == "" {
		return "untyped format"
	}
	return format + " format"
}

// inspect verifies one artifact and returns its size in bytes.
func inspect(a strategy.Artifact) (int64, error) {
	if a.Path == "" {
		return 0, fmt.Errorf("has no file path (the strategy returned an empty artifact path)")
	}
	switch strings.ToLower(a.Format) {
	case "tar":
		return inspectTar(a.Path, false)
	case "tar.gz", "tgz":
		return inspectTar(a.Path, true)
	case "gz", "gzip":
		return inspectGzip(a.Path)
	case "zip":
		return inspectZip(a.Path)
	default:
		return inspectPlain(a.Path)
	}
}

// mustBeRegularFile checks the archive-integrity floor that every
// format shares, and returns the file size.
func mustBeRegularFile(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, fmt.Errorf("could not be read: %w", err)
	}
	if !info.Mode().IsRegular() {
		return 0, fmt.Errorf("is not a regular file (a directory, device, or symlink cannot be a backup artifact)")
	}
	if info.Size() == 0 {
		return 0, fmt.Errorf("is empty — an empty artifact means the dump captured nothing")
	}
	return info.Size(), nil
}

// inspectPlain applies the floor check only (non-container formats).
func inspectPlain(path string) (int64, error) {
	return mustBeRegularFile(path)
}

// inspectTar walks every tar header to EOF, optionally through gzip.
func inspectTar(path string, gz bool) (int64, error) {
	size, err := mustBeRegularFile(path)
	if err != nil {
		return 0, err
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("could not be opened: %w", err)
	}
	defer f.Close()

	var r io.Reader = f
	var g *gzip.Reader
	if gz {
		gr, err := gzip.NewReader(f)
		if err != nil {
			return 0, fmt.Errorf("gzip header: %w", err)
		}
		defer gr.Close()
		r = gr
		g = gr
	}
	tr := tar.NewReader(r)
	for {
		if _, err := tr.Next(); err == io.EOF {
			break
		} else if err != nil {
			return 0, fmt.Errorf("tar headers: %w", err)
		}
	}
	if g != nil {
		// tar.Reader stops at the tar end-of-archive marker without
		// consuming the rest of the gzip stream, so the trailer's
		// CRC32/ISIZE is not yet verified: drain to EOF. Without this
		// a corrupt trailer or trailing compressed data passes green.
		if _, err := io.Copy(io.Discard, g); err != nil {
			return 0, fmt.Errorf("gzip stream: %w", err)
		}
	}
	return size, nil
}

// inspectGzip reads the whole stream: the gzip trailer's CRC is
// verified on the final read, so any corruption fails the check.
func inspectGzip(path string) (int64, error) {
	size, err := mustBeRegularFile(path)
	if err != nil {
		return 0, err
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("could not be opened: %w", err)
	}
	defer f.Close()

	g, err := gzip.NewReader(f)
	if err != nil {
		return 0, fmt.Errorf("gzip header: %w", err)
	}
	if _, err := io.Copy(io.Discard, g); err != nil {
		return 0, fmt.Errorf("gzip stream: %w", err)
	}
	return size, nil
}

// inspectZip opens the archive and fully decompresses every member.
func inspectZip(path string) (int64, error) {
	size, err := mustBeRegularFile(path)
	if err != nil {
		return 0, err
	}
	z, err := zip.OpenReader(path)
	if err != nil {
		return 0, fmt.Errorf("zip archive: %w", err)
	}
	defer z.Close()
	for _, m := range z.File {
		rc, err := m.Open()
		if err != nil {
			return 0, fmt.Errorf("zip member %q: %w", m.Name, err)
		}
		_, err = io.Copy(io.Discard, rc)
		rc.Close()
		if err != nil {
			return 0, fmt.Errorf("zip member %q: %w", m.Name, err)
		}
	}
	return size, nil
}

package strategy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/springledev/ballast/internal/catalog"
)

// Files implements the files strategy (spec §4.2): the catalog's
// backup.files paths are backed up directly by the uploader, so the
// strategy only validates them and hands them back. Nothing is
// written to the work directory.
type Files struct {
	entry *catalog.Entry
	opts  Options
}

// Preflight checks, for hostPaths entries, that every folder exists on
// the service's Docker host (spec §19.2). Local files are checked by
// Backup itself.
func (s *Files) Preflight(ctx context.Context) error {
	if len(s.entry.Backup.HostPaths) == 0 {
		return nil
	}
	_, err := s.opts.prepareHostPaths(ctx, s.entry.Service, s.entry.Backup.HostPaths)
	return err
}

// Backup validates every configured path and returns it for upload.
func (s *Files) Backup(ctx context.Context, req Request) (*Result, error) {
	if len(s.entry.Backup.HostPaths) > 0 {
		return s.backupHostPaths(ctx, req)
	}
	paths, err := validatePaths(s.entry.Service, s.entry.Backup.Files)
	if err != nil {
		return nil, err
	}
	var excludes []string
	for _, f := range s.entry.Backup.Files {
		excludes = append(excludes, f.Exclude...)
	}
	return &Result{Paths: paths, PathParts: pathParts(s.entry.Backup.Files), Excludes: excludes}, nil
}

// DefaultPathPart is the part name of a folder the catalog did not
// name.
const DefaultPathPart = "files"

// pathParts maps each cleaned backup.files path to its part name.
func pathParts(files []catalog.FileSpec) map[string]string {
	out := make(map[string]string, len(files))
	for _, f := range files {
		name := f.Name
		if name == "" {
			name = DefaultPathPart
		}
		out[filepath.Clean(f.Path)] = name
	}
	return out
}

// backupHostPaths archives the service's Docker-host folders live,
// through the helper container, into one tar artifact.
func (s *Files) backupHostPaths(ctx context.Context, req Request) (*Result, error) {
	if err := ensureWorkDir(req.WorkDir); err != nil {
		return nil, fmt.Errorf("service %s: %w", s.entry.Service, err)
	}
	srcs, err := s.opts.prepareHostPaths(ctx, s.entry.Service, s.entry.Backup.HostPaths)
	if err != nil {
		return nil, err
	}
	a, err := s.opts.archiveHostPaths(ctx, req.WorkDir, srcs)
	if err != nil {
		return nil, fmt.Errorf("service %s: archiving host folders: %w", s.entry.Service, err)
	}
	return &Result{Artifacts: []Artifact{a}}, nil
}

// validatePaths checks that every catalog file path is absolute and
// exists, and returns the cleaned paths. Absolute paths are required
// on purpose: under systemd or cron a relative path would silently
// back up the wrong directory, which is exactly the failure a backup
// tool must not have.
func validatePaths(service string, files []catalog.FileSpec) ([]string, error) {
	paths := make([]string, 0, len(files))
	for _, f := range files {
		p := f.Path
		if p == "" {
			return nil, fmt.Errorf("service %s: a backup.files entry has an empty path", service)
		}
		if !filepath.IsAbs(p) {
			return nil, fmt.Errorf("service %s: backup path %q must be absolute (e.g. /srv/data), not relative", service, p)
		}
		p = filepath.Clean(p)
		if _, err := os.Stat(p); err != nil {
			return nil, fmt.Errorf("service %s: backup path %s: %w", service, p, err)
		}
		paths = append(paths, p)
	}
	return paths, nil
}

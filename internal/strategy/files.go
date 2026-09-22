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
}

// Backup validates every configured path and returns it for upload.
func (s *Files) Backup(_ context.Context, req Request) (*Result, error) {
	paths, err := validatePaths(s.entry.Service, s.entry.Backup.Files)
	if err != nil {
		return nil, err
	}
	return &Result{Paths: paths}, nil
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

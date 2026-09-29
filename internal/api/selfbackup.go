package api

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/restic"
)

// SelfBackupTag marks snapshots of Ballast's own state.
const SelfBackupTag = "ballast-self"

// SelfBackupSources names Ballast's own files, beyond the status DB.
type SelfBackupSources struct {
	// ConfigFile is ballast.yaml. Skipped when empty or missing.
	ConfigFile string
	// CatalogDirs are the user catalog directories.
	CatalogDirs []string
}

// SelfBackupResult reports one self-backup.
type SelfBackupResult struct {
	Destination string `json:"destination"`
	SnapshotID  string `json:"snapshot_id"`
	// Files lists what the snapshot holds, relative to its root.
	Files []string `json:"files"`
	// Verified is true only when the snapshot was read back from the
	// repository (spec rule 1); a self-backup is never reported good
	// without it.
	Verified bool `json:"verified"`
}

// SelfBackup saves Ballast's own status DB (a consistent copy made with
// SQLite's VACUUM INTO), its config file and its user catalog entries to
// a destination, then reads the snapshot back. Credential files are
// deliberately left out: they are secrets (spec rule 2) and the operator
// provisions them; the database holds none. A snapshot that cannot be
// read back is returned as an error.
func (o *Ops) SelfBackup(ctx context.Context, destination string) (SelfBackupResult, error) {
	res := SelfBackupResult{Destination: destination}
	if o.WorkRoot == "" {
		return res, fmt.Errorf("self-backup: no work directory is configured (internal wiring error)")
	}
	dest, err := inventory.GetDestination(o.DB, destination)
	if err != nil {
		return res, wrapf("destination "+destination, err)
	}
	repo, err := o.repoFor(dest)
	if err != nil {
		return res, wrapf("self-backup to "+destination, err)
	}

	stage := filepath.Join(o.WorkRoot, "_self")
	if err := os.RemoveAll(stage); err != nil {
		return res, fmt.Errorf("self-backup: clearing %s: %w", stage, err)
	}
	if err := os.MkdirAll(stage, 0o700); err != nil {
		return res, fmt.Errorf("self-backup: creating %s: %w", stage, err)
	}
	defer os.RemoveAll(stage)

	dbCopy := filepath.Join(stage, "ballast.db")
	if _, err := o.DB.ExecContext(ctx, "VACUUM INTO ?", dbCopy); err != nil {
		return res, fmt.Errorf("self-backup: copying the status database: %w", err)
	}
	if err := os.Chmod(dbCopy, 0o600); err != nil {
		return res, fmt.Errorf("self-backup: %w", err)
	}
	if cf := o.SelfBackupFiles.ConfigFile; cf != "" {
		if err := copyFile(cf, filepath.Join(stage, "ballast.yaml")); err != nil && !os.IsNotExist(err) {
			return res, fmt.Errorf("self-backup: copying %s: %w", cf, err)
		}
	}
	for i, dir := range o.SelfBackupFiles.CatalogDirs {
		if err := copyTree(dir, filepath.Join(stage, "catalog.d", fmt.Sprint(i))); err != nil && !os.IsNotExist(err) {
			return res, fmt.Errorf("self-backup: copying catalog %s: %w", dir, err)
		}
	}

	err = filepath.WalkDir(stage, func(p string, d fs.DirEntry, werr error) error {
		if werr == nil && !d.IsDir() {
			rel, _ := filepath.Rel(stage, p)
			res.Files = append(res.Files, rel)
		}
		return werr
	})
	if err != nil {
		return res, fmt.Errorf("self-backup: %w", err)
	}

	br, err := repo.Backup(ctx, restic.BackupOptions{Paths: []string{stage}, Tags: []string{SelfBackupTag}, Host: "ballast-self"})
	if err != nil {
		return res, o.redactErr(fmt.Errorf("self-backup to %s: %w", destination, err))
	}
	res.SnapshotID = br.SnapshotID
	if _, err := repo.ReadBack(ctx, br.SnapshotID); err != nil {
		return res, o.redactErr(fmt.Errorf("self-backup to %s: saved as snapshot %s but it could not be read back, so it is NOT verified: %w", destination, br.SnapshotID, err))
	}
	res.Verified = true
	return res, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// copyTree copies regular files under src to dst, skipping anything
// else (symlinks, sockets).
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(src, p)
		if err != nil || strings.HasPrefix(rel, "..") {
			return fmt.Errorf("unexpected path %s", p)
		}
		return copyFile(p, filepath.Join(dst, rel))
	})
}

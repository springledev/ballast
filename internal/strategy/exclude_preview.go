package strategy

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/springledev/ballast/internal/catalog"
)

// maxPreviewItems caps how many skipped items a preview lists.
const maxPreviewItems = 50

// SkippedItem is one file or folder a backup would leave out.
type SkippedItem struct {
	// Path is the absolute path on the machine that holds it.
	Path string `json:"path"`
	// Bytes is its size (the sum of everything inside, for a folder).
	Bytes int64 `json:"bytes"`
	// Files is how many files it holds (1 for a single file).
	Files int `json:"files"`
	// Folder is true when Path is a folder.
	Folder bool `json:"folder"`
}

// ExcludePreview describes what the skip patterns would leave out of a
// backup. Nothing is read beyond file names and sizes.
type ExcludePreview struct {
	// Skipped lists the biggest skipped items first (at most 50).
	Skipped []SkippedItem `json:"skipped"`
	// SkippedCount, SkippedBytes: everything skipped, even items beyond
	// the listed ones.
	SkippedCount int   `json:"skipped_count"`
	SkippedBytes int64 `json:"skipped_bytes"`
	// KeptBytes is the size of everything that would still be backed up.
	KeptBytes int64 `json:"kept_bytes"`
	// Note is a plain-English remark when the preview is incomplete,
	// for example for folders on another Docker host.
	Note string `json:"note,omitempty"`
}

// PreviewExcludes walks the entry's backup.files folders on this
// machine and reports what its skip patterns would leave out. Folders
// on a Docker host (hostPaths) are not read here; the result says so.
func PreviewExcludes(entry *catalog.Entry) (*ExcludePreview, error) {
	out := &ExcludePreview{Skipped: []SkippedItem{}}
	for _, f := range entry.Backup.Files {
		root := filepath.Clean(f.Path)
		if !filepath.IsAbs(root) {
			return nil, fmt.Errorf("service %s: backup path %q must be absolute (e.g. /srv/data), not relative", entry.Service, f.Path)
		}
		if err := previewOne(root, f.Exclude, out); err != nil {
			return nil, fmt.Errorf("service %s: previewing %s: %w", entry.Service, root, err)
		}
	}
	if len(entry.Backup.HostPaths) > 0 {
		out.Note = "Folders on another Docker host cannot be previewed here; the same patterns are applied when the backup runs."
	}
	sort.SliceStable(out.Skipped, func(i, j int) bool { return out.Skipped[i].Bytes > out.Skipped[j].Bytes })
	if len(out.Skipped) > maxPreviewItems {
		out.Skipped = out.Skipped[:maxPreviewItems]
	}
	return out, nil
}

// previewOne walks root, adding skipped items and kept bytes to out.
func previewOne(root string, patterns []string, out *ExcludePreview) error {
	if _, err := os.Stat(root); err != nil {
		return err
	}
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if p != root {
			inner, err := filepath.Rel(root, p)
			if err == nil && excluded(patterns, filepath.ToSlash(inner)) {
				item := SkippedItem{Path: p, Folder: d.IsDir()}
				if d.IsDir() {
					item.Bytes, item.Files = dirSize(p)
				} else if info, err := d.Info(); err == nil {
					item.Bytes, item.Files = info.Size(), 1
				}
				out.Skipped = append(out.Skipped, item)
				out.SkippedCount++
				out.SkippedBytes += item.Bytes
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				out.KeptBytes += info.Size()
			}
		}
		return nil
	})
}

// dirSize totals the regular files under dir; unreadable entries count
// as zero, since this only feeds an estimate.
func dirSize(dir string) (bytes int64, files int) {
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			bytes += info.Size()
			files++
		}
		return nil
	})
	return bytes, files
}

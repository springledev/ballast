package strategy

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/springledev/ballast/internal/catalog"
)

// TestFilesBackup checks that the files strategy validates the
// catalog paths and hands them back for upload untouched.
func TestFilesBackup(t *testing.T) {
	dir := t.TempDir()
	entry := &catalog.Entry{
		Service: "photos",
		Backup: catalog.Backup{
			Strategy: "files",
			Files:    []catalog.FileSpec{{Path: dir}},
		},
	}
	s, err := For(entry, Options{})
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	res, err := s.Backup(context.Background(), Request{WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if !reflect.DeepEqual(res.Paths, []string{dir}) {
		t.Errorf("Paths = %v, want [%s]", res.Paths, dir)
	}
	if len(res.Artifacts) != 0 {
		t.Errorf("Artifacts = %v, want none (files strategy produces no artifacts)", res.Artifacts)
	}
}

// TestFilesBackupRejectsBadPaths is the table of path mistakes the
// strategy must catch loudly instead of backing up the wrong thing.
func TestFilesBackupRejectsBadPaths(t *testing.T) {
	existing := t.TempDir()
	tests := []struct {
		name    string
		paths   []string
		wantErr string
	}{
		{name: "relative path", paths: []string{"srv/data"}, wantErr: "must be absolute"},
		{name: "empty path", paths: []string{""}, wantErr: "empty path"},
		{name: "missing path", paths: []string{existing + "/nope"}, wantErr: "no such file"},
		{name: "one bad among good", paths: []string{existing, "relative"}, wantErr: "must be absolute"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			specs := make([]catalog.FileSpec, 0, len(tt.paths))
			for _, p := range tt.paths {
				specs = append(specs, catalog.FileSpec{Path: p})
			}
			entry := &catalog.Entry{Service: "photos", Backup: catalog.Backup{Strategy: "files", Files: specs}}
			s, err := For(entry, Options{})
			if err != nil {
				t.Fatalf("For: %v", err)
			}
			_, err = s.Backup(context.Background(), Request{WorkDir: t.TempDir()})
			if err == nil {
				t.Fatalf("Backup = nil error, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Backup error = %q, want substring %q", err, tt.wantErr)
			}
			if !strings.Contains(err.Error(), "photos") {
				t.Errorf("Backup error = %q, want it to name the service", err)
			}
		})
	}
}

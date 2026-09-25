package catalog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

const minimalEntry = `
apiVersion: ballast.dev/v1
service: %s
version: 1
backup:
  strategy: files
  files:
    - path: "/data"
verify:
  method: archive-integrity
`

func mapFS(files map[string]string) fstest.MapFS {
	fsys := fstest.MapFS{}
	for name, content := range files {
		fsys[name] = &fstest.MapFile{Data: []byte(content)}
	}
	return fsys
}

func TestLoadRegistryBundledEntriesPresent(t *testing.T) {
	bundled := mapFS(map[string]string{
		"postgres.yaml": fmtEntry("postgres"),
		"mysql.yaml":    fmtEntry("mysql"),
		"folder.yaml":   fmtEntry("folder"),
	})
	r, err := LoadRegistry(bundled, nil)
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	for _, svc := range []string{"postgres", "mysql", "folder"} {
		if _, ok := r.Get(svc); !ok {
			t.Errorf("bundled entry %q not found", svc)
		}
	}
	if len(r.List()) != 3 {
		t.Errorf("List() len = %d, want 3", len(r.List()))
	}
}

func TestLoadRegistryUserOverridesBundled(t *testing.T) {
	bundled := mapFS(map[string]string{"postgres.yaml": fmtEntry("postgres")})
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "postgres.yaml"), fmtEntryWithMarker("postgres", "custom-marker"))

	r, err := LoadRegistry(bundled, []string{dir})
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	e, ok := r.Get("postgres")
	if !ok {
		t.Fatal("postgres entry missing")
	}
	if e.Backup.Files[0].Path != "custom-marker" {
		t.Errorf("entry not overridden by user file: %+v", e)
	}
}

func TestLoadRegistryDuplicateUserEntryErrors(t *testing.T) {
	bundled := mapFS(map[string]string{})
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	writeFile(t, filepath.Join(dir1, "svc.yaml"), fmtEntry("svc"))
	writeFile(t, filepath.Join(dir2, "svc.yaml"), fmtEntry("svc"))

	_, err := LoadRegistry(bundled, []string{dir1, dir2})
	if err == nil {
		t.Fatal("LoadRegistry: want an error for duplicate user entries")
	}
	if !strings.Contains(err.Error(), dir1) || !strings.Contains(err.Error(), dir2) {
		t.Errorf("error = %v, want it to name both paths", err)
	}
}

func TestLoadRegistryInvalidFileErrors(t *testing.T) {
	bundled := mapFS(map[string]string{})
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	writeFile(t, path, "not: valid: catalog: entry:")

	_, err := LoadRegistry(bundled, []string{dir})
	if err == nil {
		t.Fatal("LoadRegistry: want an error for an invalid catalog file")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error = %v, want it to name the invalid path", err)
	}
}

func TestLoadRegistryMissingUserDirIsFine(t *testing.T) {
	bundled := mapFS(map[string]string{"postgres.yaml": fmtEntry("postgres")})
	missing := filepath.Join(t.TempDir(), "does-not-exist")

	r, err := LoadRegistry(bundled, []string{missing})
	if err != nil {
		t.Fatalf("LoadRegistry with missing user dir: %v", err)
	}
	if _, ok := r.Get("postgres"); !ok {
		t.Error("bundled entry should still load when a user dir is missing")
	}
}

func TestRegistryGetReturnsIndependentCopies(t *testing.T) {
	bundled := mapFS(map[string]string{"postgres.yaml": fmtEntry("postgres")})
	r, err := LoadRegistry(bundled, nil)
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	a, _ := r.Get("postgres")
	a.Backup.Files[0].Path = "mutated"
	b, _ := r.Get("postgres")
	if b.Backup.Files[0].Path == "mutated" {
		t.Error("Get must return an independent copy, not a shared pointer")
	}
}

func fmtEntry(service string) string {
	return strings.Replace(minimalEntry, "%s", service, 1)
}

func fmtEntryWithMarker(service, marker string) string {
	return strings.Replace(strings.Replace(minimalEntry, "%s", service, 1), `"/data"`, `"`+marker+`"`, 1)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

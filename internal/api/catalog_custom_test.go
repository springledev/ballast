package api

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/springledev/ballast/internal/catalog"
)

// Every shipped template must be a valid entry as-is, so a beginner who
// picks one and clicks install gets a working starting point.
func TestCatalogTemplatesAreValid(t *testing.T) {
	h := newHarness(t)
	templates := h.ops.CatalogTemplates()
	if len(templates) != 4 {
		t.Fatalf("templates = %d, want database, folder, volume and script", len(templates))
	}
	for _, tpl := range templates {
		t.Run(tpl.ID, func(t *testing.T) {
			if _, err := catalog.Load([]byte(tpl.YAML)); err != nil {
				t.Fatalf("template %s does not validate: %v", tpl.ID, err)
			}
			if v := h.ops.ValidateCatalogYAML(tpl.YAML); !v.Valid {
				t.Errorf("ValidateCatalogYAML = %+v, want valid", v)
			}
		})
	}
}

func TestValidateCatalogYAML(t *testing.T) {
	h := newHarness(t)
	_, loadErr := catalog.Load([]byte("service: x\n"))
	if loadErr == nil {
		t.Fatal("test setup: expected Load to reject the entry")
	}
	tests := []struct {
		name      string
		text      string
		wantValid bool
		wantErr   string
	}{
		{"same message as catalog validate", "service: x\n", false, loadErr.Error()},
		{"not yaml", "a: [", false, "parsing YAML"},
		{"clashes with built-in", strings.Replace(catalogTemplates[3].YAML, "my-script", "folder", 1), false, "built-in"},
		{"fine", catalogTemplates[3].YAML, true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := h.ops.ValidateCatalogYAML(tt.text)
			if v.Valid != tt.wantValid {
				t.Fatalf("Valid = %v (%s), want %v", v.Valid, v.Error, tt.wantValid)
			}
			if !strings.Contains(v.Error, tt.wantErr) {
				t.Errorf("Error = %q, want it to contain %q", v.Error, tt.wantErr)
			}
		})
	}
}

func TestInstallCatalogYAML(t *testing.T) {
	h := newHarness(t)
	dir := filepath.Join(t.TempDir(), "catalog.d")
	h.ops.CatalogDirs = []string{dir}

	res, err := h.ops.InstallCatalogYAML(catalogTemplates[3].YAML)
	if err != nil {
		t.Fatalf("InstallCatalogYAML: %v", err)
	}
	if res.Service != "my-script" || res.Path != filepath.Join(dir, "my-script.yaml") {
		t.Errorf("result = %+v", res)
	}
	if _, err := os.Stat(res.Path); err != nil {
		t.Errorf("entry file not written: %v", err)
	}
	if _, ok := h.ops.Catalog.Get("my-script"); !ok {
		t.Error("installed entry is not visible without a restart")
	}
	// A fresh start reads the same folder without complaint.
	if _, err := catalog.LoadRegistry(fstest.MapFS{}, []string{dir}); err != nil {
		t.Errorf("reloading catalog folder: %v", err)
	}

	// Re-installing updates in place.
	if _, err := h.ops.InstallCatalogYAML(catalogTemplates[3].YAML); err != nil {
		t.Errorf("re-install: %v", err)
	}
	// Built-in names and bad text are refused and write nothing.
	for _, text := range []string{strings.Replace(catalogTemplates[3].YAML, "my-script", "folder", 1), "service: x\n"} {
		_, err := h.ops.InstallCatalogYAML(text)
		if !errors.Is(err, ErrValidation) {
			t.Errorf("InstallCatalogYAML(%q) = %v, want a validation error", text[:12], err)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("catalog dir has %d files, want only my-script.yaml", len(entries))
	}

	h.ops.CatalogDirs = nil
	if _, err := h.ops.InstallCatalogYAML(catalogTemplates[3].YAML); !errors.Is(err, ErrValidation) {
		t.Errorf("no catalog dir: err = %v, want validation error", err)
	}
}

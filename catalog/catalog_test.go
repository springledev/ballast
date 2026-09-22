package catalog

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	ballastcatalog "github.com/springledev/ballast/internal/catalog"
)

// refRE finds template references ({{ .Params.NAME }} /
// {{ .Services.NAME }}) in raw catalog YAML.
var refRE = regexp.MustCompile(`\{\{\s*\.(Params|Services)\.([A-Za-z0-9_]+)\s*\}\}`)

// bundledEntries lists every YAML catalog entry in this directory.
func bundledEntries(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob("*.yaml")
	if err != nil {
		t.Fatalf("listing bundled catalog entries: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("no bundled catalog entries found (expected at least one *.yaml)")
	}
	return paths
}

// TestBundledEntriesValidate validates every bundled entry against
// the catalog-v1 JSON schema (spec §13: every bundled catalog entry
// is schema-validated) and checks the file name matches the service
// name, so `ballast catalog list` output stays predictable.
func TestBundledEntriesValidate(t *testing.T) {
	for _, path := range bundledEntries(t) {
		t.Run(path, func(t *testing.T) {
			entry, err := ballastcatalog.LoadFile(path)
			if err != nil {
				t.Fatalf("bundled entry failed schema validation: %v", err)
			}
			base := strings.TrimSuffix(filepath.Base(path), ".yaml")
			if entry.Service != base {
				t.Errorf("service = %q, want it to match the file name %q", entry.Service, base)
			}
		})
	}
}

// TestBundledEntriesTemplatesRoundTrip resolves every bundled entry
// with dummy values and asserts two things: templates only reference
// declared parameters, and substitution succeeds end-to-end (spec
// §13: parameter templating round-trips).
func TestBundledEntriesTemplatesRoundTrip(t *testing.T) {
	for _, path := range bundledEntries(t) {
		t.Run(path, func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading entry: %v", err)
			}
			entry, err := ballastcatalog.Load(raw)
			if err != nil {
				t.Fatalf("loading entry: %v", err)
			}

			declared := map[string]bool{}
			for _, p := range entry.Parameters {
				declared[p.Name] = true
			}
			ctx := ballastcatalog.TemplateContext{
				Params:   map[string]string{},
				Services: map[string]string{},
			}
			for _, m := range refRE.FindAllStringSubmatch(string(raw), -1) {
				table, name := m[1], m[2]
				if table == "Params" {
					if !declared[name] {
						t.Errorf("template references parameter %q, which is not declared in parameters:", name)
					}
					ctx.Params[name] = "test-" + name
				} else {
					ctx.Services[name] = "test-" + name
				}
			}

			resolved := entry.DeepCopy()
			if err := resolved.Resolve(ctx); err != nil {
				t.Fatalf("Resolve with dummy values: %v", err)
			}
			// No placeholder may survive resolution.
			for _, d := range resolved.Backup.Dumps {
				if strings.Contains(d.Container, "{{") {
					t.Errorf("dump %q container still contains a placeholder: %q", d.Name, d.Container)
				}
				for _, part := range d.Command {
					if strings.Contains(part, "{{") {
						t.Errorf("dump %q command still contains a placeholder: %q", d.Name, part)
					}
				}
			}
			for _, f := range resolved.Backup.Files {
				if strings.Contains(f.Path, "{{") {
					t.Errorf("file path still contains a placeholder: %q", f.Path)
				}
			}
			if strings.Contains(resolved.Verify.Image, "{{") {
				t.Errorf("verify image still contains a placeholder: %q", resolved.Verify.Image)
			}
		})
	}
}

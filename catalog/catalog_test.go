package catalog

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	ballastcatalog "github.com/springledev/ballast/internal/catalog"
	"github.com/springledev/ballast/internal/strategy"
	"github.com/springledev/ballast/internal/verify"
	"github.com/springledev/keel/runner"
)

// resolveWithDummyValues loads and resolves one bundled entry with
// dummy parameter/service values, exactly as
// TestBundledEntriesTemplatesRoundTrip does: every {{ .Params.* }} /
// {{ .Services.* }} reference found in the raw YAML gets a
// "test-<name>" value (a parameter's catalog default, when it has
// one, is filled in automatically by Entry.Resolve for anything not
// referenced directly here — but every bundled entry's templates
// happen to reference every parameter, so this covers them all).
func resolveWithDummyValues(t *testing.T, path string) *ballastcatalog.Entry {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading entry: %v", err)
	}
	entry, err := ballastcatalog.Load(raw)
	if err != nil {
		t.Fatalf("loading entry: %v", err)
	}
	ctx := ballastcatalog.TemplateContext{Params: map[string]string{}, Services: map[string]string{}, Discovery: map[string]string{}}
	for _, m := range refRE.FindAllStringSubmatch(string(raw), -1) {
		table, name := m[1], m[2]
		switch table {
		case "Params":
			ctx.Params[name] = "test-" + name
		case "Discovery":
			ctx.Discovery[name] = "test-" + name
		default:
			ctx.Services[name] = "test-" + name
		}
	}
	if err := entry.Resolve(ctx); err != nil {
		t.Fatalf("Resolve with dummy values: %v", err)
	}
	return entry
}

// refRE finds template references ({{ .Params.NAME }} /
// {{ .Services.NAME }} / {{ .Discovery.NAME }}) in raw catalog YAML.
var refRE = regexp.MustCompile(`\{\{\s*\.(Params|Services|Discovery)\.([A-Za-z0-9_]+)\s*\}\}`)

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
				Params:    map[string]string{},
				Services:  map[string]string{},
				Discovery: map[string]string{},
			}
			for _, m := range refRE.FindAllStringSubmatch(string(raw), -1) {
				table, name := m[1], m[2]
				switch table {
				case "Params":
					if !declared[name] {
						t.Errorf("template references parameter %q, which is not declared in parameters:", name)
					}
					ctx.Params[name] = "test-" + name
				case "Discovery":
					ctx.Discovery[name] = "test-" + name
				default:
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

// TestBundledEntriesAreRunnable asserts that every bundled entry is
// not merely schema-valid but actually runnable by the implemented
// strategy and verification code: once resolved, strategy.For and
// verify.For (the same constructors the real pipeline calls, see
// internal/pipeline.Pipeline.Run) must both succeed against it. A
// catalog entry that fails here would fail every real backup run.
func TestBundledEntriesAreRunnable(t *testing.T) {
	for _, path := range bundledEntries(t) {
		t.Run(path, func(t *testing.T) {
			entry := resolveWithDummyValues(t, path)
			opts := strategy.Options{Runner: &runner.Runner{}}
			if _, err := strategy.For(entry, opts); err != nil {
				t.Errorf("strategy.For(%s) = error %v, want nil", entry.Service, err)
			}
			vopts := verify.Options{Runner: &runner.Runner{}}
			if _, err := verify.For(entry, vopts); err != nil {
				t.Errorf("verify.For(%s) = error %v, want nil", entry.Service, err)
			}
		})
	}
}

// TestImmichCatalogRequirements pins the Immich-specific shape the
// container-restore verification and the dump+files backup strategy
// depend on, so a future edit to catalog/immich.yaml cannot silently
// drop what makes the vector-search extension restore work (see
// internal/verify.ContainerRestore's package doc: it must start the
// throwaway container with the image's own default command, and the
// "role already exists" stderr from a --clean-less pg_dumpall restore
// must be filtered as benign).
func TestImmichCatalogRequirements(t *testing.T) {
	entry, err := ballastcatalog.LoadFile("immich.yaml")
	if err != nil {
		t.Fatalf("LoadFile(immich.yaml): %v", err)
	}
	if entry.Verify.Method != "db-restore-drill" {
		t.Errorf("Verify.Method = %q, want db-restore-drill", entry.Verify.Method)
	}
	found := false
	for _, s := range entry.Verify.FilterStderr {
		if strings.Contains(s, "already exists") {
			found = true
		}
	}
	if !found {
		t.Errorf("Verify.FilterStderr = %v, want an entry containing %q", entry.Verify.FilterStderr, "already exists")
	}
	if entry.Backup.Strategy != "dump" {
		t.Errorf("Backup.Strategy = %q, want dump", entry.Backup.Strategy)
	}
	if len(entry.Backup.Dumps) == 0 {
		t.Error("Backup.Dumps is empty, want a database dump")
	}
	if len(entry.Backup.Files) == 0 {
		t.Error("Backup.Files is empty, want the photo/video library path")
	}
}

// TestStopStartCatalogRequirements pins that vaultwarden and couchdb
// use the stop-start strategy verified by archive-integrity and
// restart-smoke — the combination that guarantees the container is
// restarted, answers its health endpoint again, and the data-folder
// archive is read back, per catalog/vaultwarden.yaml and
// catalog/couchdb.yaml's own doc comments.
func TestStopStartCatalogRequirements(t *testing.T) {
	for _, svc := range []string{"vaultwarden", "couchdb"} {
		t.Run(svc, func(t *testing.T) {
			entry, err := ballastcatalog.LoadFile(svc + ".yaml")
			if err != nil {
				t.Fatalf("LoadFile(%s.yaml): %v", svc, err)
			}
			if entry.Backup.Strategy != "stop-start" {
				t.Errorf("Backup.Strategy = %q, want stop-start", entry.Backup.Strategy)
			}
			if got := entry.Verify.MethodList(); !slices.Equal(got, []string{"archive-integrity", "restart-smoke"}) {
				t.Errorf("Verify.MethodList() = %v, want [archive-integrity restart-smoke]", got)
			}
			if sm := entry.Verify.Smoke; sm == nil || sm.HTTP == nil || sm.Container != "{{ .Params.container }}" {
				t.Errorf("Verify.Smoke = %+v, want an HTTP probe of the service's container", sm)
			}
			if entry.Backup.StopStart == nil || len(entry.Backup.StopStart.Containers) == 0 {
				t.Error("Backup.StopStart.Containers is empty")
			}
			if len(entry.Backup.Files) == 0 {
				t.Error("Backup.Files is empty, want the service's data directory")
			}
		})
	}
}

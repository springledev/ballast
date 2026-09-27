package catalog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// validEntry is the spec §6.1 Immich example, slightly trimmed to the fields
// the v0.1 schema covers.
const validEntry = `
apiVersion: ballast.dev/v1
service: immich
version: 1
display:
  name: Immich
  description: Photos & videos (Postgres + library)
detect:
  docker:
    imagePattern: "ghcr.io/immich-app/immich-server"
parameters:
  - name: library_dir
    required: true
    prompt: "Path to your Immich upload/library directory"
backup:
  strategy: dump
  dumps:
    - name: database
      container: "{{ .Services.database }}"
      command: ["pg_dumpall", "-U", "postgres"]
      format: sql
  files:
    - path: "{{ .Params.library_dir }}"
verify:
  method: container-restore
  image: "ghcr.io/immich-app/postgres:16"
  filterStderr:
    - "already exists"
  restartPolicy: never
notifications:
  failureVerbosity: full
`

func TestLoadValidEntry(t *testing.T) {
	entry, err := Load([]byte(validEntry))
	if err != nil {
		t.Fatalf("Load(valid entry) = error %v, want nil", err)
	}
	if entry.Service != "immich" {
		t.Errorf("Service = %q, want %q", entry.Service, "immich")
	}
	if entry.APIVersion != "ballast.dev/v1" {
		t.Errorf("APIVersion = %q, want ballast.dev/v1", entry.APIVersion)
	}
	if entry.Version != 1 {
		t.Errorf("Version = %d, want 1", entry.Version)
	}
	if len(entry.Backup.Dumps) != 1 || entry.Backup.Dumps[0].Command[0] != "pg_dumpall" {
		t.Errorf("Backup.Dumps = %+v, want one pg_dumpall dump", entry.Backup.Dumps)
	}
	if entry.Backup.Strategy != "dump" {
		t.Errorf("Strategy = %q, want dump", entry.Backup.Strategy)
	}
	if entry.Verify.Method != "container-restore" {
		t.Errorf("Verify.Method = %q, want container-restore", entry.Verify.Method)
	}
	if got := entry.Verify.FilterStderr; len(got) != 1 || got[0] != "already exists" {
		t.Errorf("FilterStderr = %v, want [already exists]", got)
	}
	if len(entry.Parameters) != 1 || entry.Parameters[0].Name != "library_dir" || !entry.Parameters[0].Required {
		t.Errorf("Parameters = %+v, want one required library_dir", entry.Parameters)
	}
}

func TestLoadRejections(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name:    "unknown top-level key rejected",
			yaml:    validEntry + "\nsuspicious_key: yes\n",
			wantErr: "not allowed",
		},
		{
			name:    "wrong apiVersion",
			yaml:    strings.Replace(validEntry, "ballast.dev/v1", "ballast.dev/v2", 1),
			wantErr: "catalog validation",
		},
		{
			name:    "missing verify block",
			yaml:    strings.Replace(validEntry, "\nverify:\n  method: container-restore\n  image: \"ghcr.io/immich-app/postgres:16\"\n  filterStderr:\n    - \"already exists\"\n  restartPolicy: never\n", "\n", 1),
			wantErr: "missing property",
		},
		{
			name: "unknown key in nested block",
			yaml: strings.Replace(validEntry,
				"  method: container-restore",
				"  method: container-restore\n  evil: true", 1),
			wantErr: "not allowed",
		},
		{
			name:    "bad strategy value",
			yaml:    strings.Replace(validEntry, "strategy: dump", "strategy: teleport", 1),
			wantErr: "catalog validation",
		},
		{
			name:    "invalid yaml",
			yaml:    "not: [valid",
			wantErr: "parsing YAML",
		},
		{
			name: "dump strategy without dumps",
			yaml: strings.Replace(validEntry,
				"strategy: dump\n  dumps:\n    - name: database\n      container: \"{{ .Services.database }}\"\n      command: [\"pg_dumpall\", \"-U\", \"postgres\"]\n      format: sql\n",
				"strategy: dump\n", 1),
			wantErr: "catalog validation",
		},
		{
			name:    "imagePattern is not a valid regular expression",
			yaml:    strings.Replace(validEntry, `imagePattern: "ghcr.io/immich-app/immich-server"`, `imagePattern: "immich-server("`, 1),
			wantErr: "not a valid regular expression",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Load([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("Load() = no error, want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Load() error = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestLoadFileWrapsPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(path, []byte("apiVersion: ballast.dev/v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadFile(path)
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("LoadFile(bad file) error = %v, want error mentioning %q", err, path)
	}
	if _, err := LoadFile(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Fatal("LoadFile(missing) = nil error, want error")
	}
}

func TestResolveTemplates(t *testing.T) {
	entry, err := Load([]byte(validEntry))
	if err != nil {
		t.Fatal(err)
	}
	deep := entry.DeepCopy()
	ctx := TemplateContext{
		Params:   map[string]string{"library_dir": "/srv/immich/library"},
		Services: map[string]string{"database": "immich-db-1"},
	}
	if err := deep.Resolve(ctx); err != nil {
		t.Fatalf("Resolve() error = %v, want nil", err)
	}
	if got := deep.Backup.Files[0].Path; got != "/srv/immich/library" {
		t.Errorf("file path = %q, want /srv/immich/library", got)
	}
	if got := deep.Backup.Dumps[0].Container; got != "immich-db-1" {
		t.Errorf("dump container = %q, want immich-db-1", got)
	}
	if got := deep.Backup.Dumps[0].Command[0]; got != "pg_dumpall" {
		t.Errorf("command = %q, want pg_dumpall", got)
	}
	// The original entry must be unchanged (round-trip check).
	if got := entry.Backup.Files[0].Path; got != "{{ .Params.library_dir }}" {
		t.Errorf("original entry mutated; file path = %q", got)
	}
}

func TestResolveRejections(t *testing.T) {
	base, err := Load([]byte(validEntry))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		ctx     TemplateContext
		wantErr string
	}{
		{
			name:    "missing required parameter",
			ctx:     TemplateContext{Services: map[string]string{"database": "db"}},
			wantErr: "missing required parameter(s): library_dir",
		},
		{
			name:    "unknown service reference",
			ctx:     TemplateContext{Params: map[string]string{"library_dir": "/mnt"}, Services: map[string]string{}},
			wantErr: "unknown detected service \"database\"",
		},
	}
	for _, tc := range cases {
		entry := base.DeepCopy()
		err := entry.Resolve(tc.ctx)
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("Resolve(%s) error = %v, want containing %q", tc.name, err, tc.wantErr)
		}
	}
}

// TestResolveParameterDefaults: an omitted parameter takes its catalog
// default (so `ballast service add` need not repeat it), a supplied
// value — even an empty one — wins, and the caller's map is untouched.
func TestResolveParameterDefaults(t *testing.T) {
	base, err := Load([]byte(validEntry))
	if err != nil {
		t.Fatal(err)
	}
	base.Parameters[0].Default = "/srv/default-library"
	cases := []struct {
		name   string
		params map[string]string
		want   string
	}{
		{name: "omitted takes default", params: map[string]string{}, want: "/srv/default-library"},
		{name: "nil params takes default", params: nil, want: "/srv/default-library"},
		{name: "supplied value wins", params: map[string]string{"library_dir": "/mnt/photos"}, want: "/mnt/photos"},
		{name: "supplied empty value wins", params: map[string]string{"library_dir": ""}, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entry := base.DeepCopy()
			before := len(tc.params)
			if err := entry.Resolve(TemplateContext{Params: tc.params, Services: map[string]string{"database": "db"}}); err != nil {
				t.Fatalf("Resolve() error = %v", err)
			}
			if got := entry.Backup.Files[0].Path; got != tc.want {
				t.Errorf("file path = %q, want %q", got, tc.want)
			}
			if len(tc.params) != before {
				t.Errorf("Resolve mutated the caller's params map: %v", tc.params)
			}
		})
	}
}

func TestResolveUnknownParamReference(t *testing.T) {
	base, err := Load([]byte(validEntry))
	if err != nil {
		t.Fatal(err)
	}
	// Strip the required-parameters declaration so the substitution error
	// (not the required check) is what surfaces.
	entry := base.DeepCopy()
	entry.Parameters = nil
	err = entry.Resolve(TemplateContext{
		Params:   map[string]string{"wrong": "x"},
		Services: map[string]string{"database": "db"},
	})
	if err == nil || !strings.Contains(err.Error(), `unknown parameter "library_dir"`) {
		t.Fatalf("Resolve() error = %v, want unknown-parameter error", err)
	}
}

// TestResolveSecrets checks that {{ .Secrets.NAME }} placeholders in
// backup.dumps[].secretEnv resolve to the given plaintext values,
// leave the rest of the entry (and dumps with no secretEnv) alone,
// and that Resolve itself never needs or accepts a Secrets value —
// only ResolveSecrets does.
func TestResolveSecrets(t *testing.T) {
	yaml := strings.Replace(validEntry,
		"      format: sql\n",
		"      format: sql\n      secretEnv:\n        MYSQL_PWD: \"{{ .Secrets.db_password }}\"\n", 1)
	entry, err := Load([]byte(yaml))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	deep := entry.DeepCopy()
	if err := deep.Resolve(TemplateContext{
		Params:   map[string]string{"library_dir": "/srv/immich/library"},
		Services: map[string]string{"database": "immich-db-1"},
	}); err != nil {
		t.Fatalf("Resolve() error = %v, want nil (Resolve must not need secrets)", err)
	}
	if err := deep.ResolveSecrets(map[string]string{"db_password": "hunter2"}); err != nil {
		t.Fatalf("ResolveSecrets() error = %v, want nil", err)
	}
	if got := deep.Backup.Dumps[0].SecretEnv["MYSQL_PWD"]; got != "hunter2" {
		t.Errorf("secretEnv[MYSQL_PWD] = %q, want hunter2", got)
	}
	// The original entry's secretEnv template must be untouched.
	if got := entry.Backup.Dumps[0].SecretEnv["MYSQL_PWD"]; got != "{{ .Secrets.db_password }}" {
		t.Errorf("original entry mutated; secretEnv = %q", got)
	}
}

// TestResolveSecretsUnknownSecret checks that an unresolvable
// {{ .Secrets.* }} reference is a loud error, not a silent pass-through.
func TestResolveSecretsUnknownSecret(t *testing.T) {
	yaml := strings.Replace(validEntry,
		"      format: sql\n",
		"      format: sql\n      secretEnv:\n        MYSQL_PWD: \"{{ .Secrets.db_password }}\"\n", 1)
	entry, err := Load([]byte(yaml))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	err = entry.ResolveSecrets(map[string]string{"other": "x"})
	if err == nil || !strings.Contains(err.Error(), `unknown secret "db_password"`) {
		t.Fatalf("ResolveSecrets(missing secret) error = %v, want unknown-secret error", err)
	}
}

// TestResolveRejectsSecretsOutsideSecretEnv checks that {{ .Secrets.* }}
// used anywhere other than secretEnv (command, container, path, verify
// image) is rejected by Resolve itself — the same guard that already
// rejects any other unsupported table name — so a secret value can
// never reach argv, a container name, or a file path.
func TestResolveRejectsSecretsOutsideSecretEnv(t *testing.T) {
	yaml := strings.Replace(validEntry,
		`container: "{{ .Services.database }}"`,
		`container: "{{ .Secrets.db_password }}"`, 1)
	entry, err := Load([]byte(yaml))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	err = entry.Resolve(TemplateContext{
		Params:   map[string]string{"library_dir": "/x"},
		Services: map[string]string{"database": "db"},
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported template expression") {
		t.Fatalf("Resolve(secret outside secretEnv) error = %v, want unsupported-template-expression refusal", err)
	}
}

func TestResolveRejectsUnsafeExpressions(t *testing.T) {
	yaml := strings.Replace(validEntry,
		"path: \"{{ .Params.library_dir }}\"",
		"path: \"{{ .Env.PASSWORD }}\"", 1)
	entry, err := Load([]byte(yaml))
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	err = entry.Resolve(TemplateContext{
		Params:   map[string]string{"library_dir": "/x", ".Params": "", "|": ""},
		Services: map[string]string{"database": "db"},
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported template expression") {
		t.Fatalf("Resolve(unsafe expr) error = %v, want unsupported-template-expression refusal", err)
	}
}

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
			name:    "volume strategy without volumes",
			yaml:    volumeEntryYAML(""),
			wantErr: "catalog validation",
		},
		{
			name:    "unknown parameter picker",
			yaml:    strings.Replace(volumeEntryYAML("  volumes:\n    - name: \"{{ .Params.volume }}\"\n"), "picker: docker-volume", "picker: colour-wheel", 1),
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

// volumeEntryYAML is a minimal volume-strategy entry whose backup
// block ends with volumesBlock (indented under backup:).
func volumeEntryYAML(volumesBlock string) string {
	return `apiVersion: ballast.dev/v1
service: vol
version: 1
parameters:
  - name: volume
    required: true
    picker: docker-volume
backup:
  strategy: volume
` + volumesBlock + `verify:
  method: archive-integrity
`
}

// TestResolveVolumeTemplates checks that volume names are templated,
// the picker hint survives loading, and DeepCopy isolates volumes.
func TestResolveVolumeTemplates(t *testing.T) {
	entry, err := Load([]byte(volumeEntryYAML("  volumes:\n    - name: \"{{ .Params.volume }}\"\n")))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := entry.Parameters[0].Picker; got != "docker-volume" {
		t.Errorf("picker = %q, want docker-volume", got)
	}
	deep := entry.DeepCopy()
	if err := deep.Resolve(TemplateContext{Params: map[string]string{"volume": "app_data"}}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := deep.Backup.Volumes[0].Name; got != "app_data" {
		t.Errorf("volume name = %q, want app_data", got)
	}
	if got := entry.Backup.Volumes[0].Name; got != "{{ .Params.volume }}" {
		t.Errorf("original entry mutated; volume name = %q", got)
	}
}

// composeEntryYAML is a minimal compose-strategy entry.
func composeEntryYAML() string {
	return `apiVersion: ballast.dev/v1
service: stack
version: 1
parameters:
  - name: project
    required: true
    picker: compose-project
backup:
  strategy: compose
  compose:
    project: "{{ .Params.project }}"
verify:
  method: archive-integrity
`
}

// TestResolveComposeTemplates checks that the compose project name is
// templated, the picker hint survives loading, and DeepCopy isolates
// the compose spec.
func TestResolveComposeTemplates(t *testing.T) {
	entry, err := Load([]byte(composeEntryYAML()))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := entry.Parameters[0].Picker; got != "compose-project" {
		t.Errorf("picker = %q, want compose-project", got)
	}
	deep := entry.DeepCopy()
	if err := deep.Resolve(TemplateContext{Params: map[string]string{"project": "immich"}}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := deep.Backup.Compose.Project; got != "immich" {
		t.Errorf("compose project = %q, want immich", got)
	}
	if got := entry.Backup.Compose.Project; got != "{{ .Params.project }}" {
		t.Errorf("original entry mutated; compose project = %q", got)
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

// TestDiscoveryFieldLoading checks that a catalog entry with a discovery
// block round-trips through YAML parsing.
func TestDiscoveryFieldLoading(t *testing.T) {
	yaml := strings.Replace(validEntry,
		"parameters:",
		"discovery:\n  database:\n    type: docker-label\n    match: \"com.docker.compose.service=db\"\n    fallback: \"{{ .Params.db_container }}\"\nparameters:", 1)
	entry, err := Load([]byte(yaml))
	if err != nil {
		t.Fatalf("Load(with discovery) error = %v, want nil", err)
	}
	if len(entry.Discovery) != 1 {
		t.Fatalf("Discovery = %+v, want one entry", entry.Discovery)
	}
	if rule, ok := entry.Discovery["database"]; !ok {
		t.Fatal("Discovery[database] not found")
	} else {
		if rule.Type != "docker-label" {
			t.Errorf("Type = %q, want docker-label", rule.Type)
		}
		if rule.Match != "com.docker.compose.service=db" {
			t.Errorf("Match = %q, want com.docker.compose.service=db", rule.Match)
		}
		if rule.Fallback != "{{ .Params.db_container }}" {
			t.Errorf("Fallback = %q, want {{ .Params.db_container }}", rule.Fallback)
		}
	}
}

// TestResolveTemplateWithDiscovery checks that ResolveTemplate substitutes
// {{ .Discovery.* }} placeholders.
func TestResolveTemplateWithDiscovery(t *testing.T) {
	tests := []struct {
		name    string
		tmpl    string
		ctx     TemplateContext
		want    string
		wantErr string
	}{
		{
			name: "resolves discovery value",
			tmpl: "prefix-{{ .Discovery.db }}-suffix",
			ctx: TemplateContext{
				Discovery: map[string]string{"db": "mydb-container"},
			},
			want: "prefix-mydb-container-suffix",
		},
		{
			name: "empty string discovery value",
			tmpl: "{{ .Discovery.empty }}",
			ctx: TemplateContext{
				Discovery: map[string]string{"empty": ""},
			},
			want: "",
		},
		{
			name:    "unknown discovery name errors",
			tmpl:    "{{ .Discovery.unknown }}",
			ctx:     TemplateContext{Discovery: map[string]string{}},
			wantErr: `unknown discovery value "unknown"`,
		},
		{
			name: "mixed params and discovery",
			tmpl: "{{ .Params.x }}-{{ .Discovery.y }}",
			ctx: TemplateContext{
				Params:    map[string]string{"x": "param-val"},
				Discovery: map[string]string{"y": "disc-val"},
			},
			want: "param-val-disc-val",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveTemplate(tc.tmpl, tc.ctx)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("ResolveTemplate() error = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveTemplate() error = %v, want nil", err)
			}
			if got != tc.want {
				t.Errorf("ResolveTemplate() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestResolveWithDiscovery checks that Entry.Resolve substitutes
// {{ .Discovery.* }} in dump containers and other template fields.
func TestResolveWithDiscovery(t *testing.T) {
	yaml := strings.Replace(validEntry,
		"parameters:",
		"discovery:\n  database:\n    type: docker-label\n    match: \"com.docker.compose.service=db\"\nparameters:", 1)
	yaml = strings.Replace(yaml,
		`container: "{{ .Services.database }}"`,
		`container: "{{ .Discovery.database }}"`, 1)
	entry, err := Load([]byte(yaml))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	deep := entry.DeepCopy()
	ctx := TemplateContext{
		Params:    map[string]string{"library_dir": "/srv/immich/library"},
		Services:  map[string]string{},
		Discovery: map[string]string{"database": "immich-db-resolved"},
	}
	if err := deep.Resolve(ctx); err != nil {
		t.Fatalf("Resolve() error = %v, want nil", err)
	}
	if got := deep.Backup.Dumps[0].Container; got != "immich-db-resolved" {
		t.Errorf("dump container = %q, want immich-db-resolved", got)
	}
}

// TestResolveWithMissingDiscovery checks that resolving a missing discovery
// value is a clear error.
func TestResolveWithMissingDiscovery(t *testing.T) {
	yaml := strings.Replace(validEntry,
		`container: "{{ .Services.database }}"`,
		`container: "{{ .Discovery.database }}"`, 1)
	entry, err := Load([]byte(yaml))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	err = entry.Resolve(TemplateContext{
		Params:    map[string]string{"library_dir": "/x"},
		Services:  map[string]string{},
		Discovery: map[string]string{}, // database discovery not provided
	})
	if err == nil || !strings.Contains(err.Error(), `unknown discovery value "database"`) {
		t.Fatalf("Resolve(missing discovery) error = %v, want unknown discovery error", err)
	}
}

// TestParamsWithDefaults checks that ParamsWithDefaults returns params
// merged with catalog parameter defaults.
func TestParamsWithDefaults(t *testing.T) {
	entry, err := Load([]byte(validEntry))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	entry.Parameters[0].Default = "/srv/default-library"
	tests := []struct {
		name   string
		params map[string]string
		want   string
	}{
		{name: "omitted takes default", params: map[string]string{}, want: "/srv/default-library"},
		{name: "nil params takes default", params: nil, want: "/srv/default-library"},
		{name: "supplied value wins", params: map[string]string{"library_dir": "/mnt/photos"}, want: "/mnt/photos"},
		{name: "empty value wins", params: map[string]string{"library_dir": ""}, want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := entry.ParamsWithDefaults(tc.params)
			if got := result["library_dir"]; got != tc.want {
				t.Errorf("ParamsWithDefaults() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDeepCopyDiscovery checks that DeepCopy deep-copies the Discovery map
// so mutating the copy does not affect the original.
func TestDeepCopyDiscovery(t *testing.T) {
	yaml := strings.Replace(validEntry,
		"parameters:",
		"discovery:\n  database:\n    type: docker-label\n    match: \"com.docker.compose.service=db\"\n  cache:\n    type: docker-label\n    match: \"service=redis\"\nparameters:", 1)
	entry, err := Load([]byte(yaml))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	deep := entry.DeepCopy()

	// Mutate the copy's discovery map.
	deep.Discovery["database"] = DiscoveryRule{Type: "modified"}
	deep.Discovery["new-key"] = DiscoveryRule{Type: "new"}

	// Original must be unchanged.
	if orig, ok := entry.Discovery["database"]; !ok || orig.Type != "docker-label" {
		t.Errorf("original Discovery[database] was mutated: %+v", orig)
	}
	if _, ok := entry.Discovery["new-key"]; ok {
		t.Errorf("original Discovery gained new-key: %+v", entry.Discovery)
	}
	// And the deep copy's modifications must be present.
	if deep.Discovery["database"].Type != "modified" {
		t.Errorf("deep copy not mutated correctly: %+v", deep.Discovery["database"])
	}
}

// verifyEntryYAML returns a minimal files-strategy entry whose verify
// block is verifyBlock (indented two spaces under "verify:").
func verifyEntryYAML(verifyBlock string) string {
	return `
apiVersion: ballast.dev/v1
service: demo
version: 1
parameters:
  - name: container
    required: true
backup:
  strategy: files
  files:
    - path: /srv/demo
verify:
` + verifyBlock
}

func TestLoadVerifyMethods(t *testing.T) {
	t.Parallel()
	smoke := "  smoke:\n    container: web\n"
	cases := []struct {
		name    string
		verify  string
		wantErr string // "" = accepted
	}{
		{name: "single method", verify: "  method: archive-integrity\n"},
		{name: "methods list", verify: "  methods: [archive-integrity, restart-smoke]\n" + smoke},
		{name: "legacy container-restore with image", verify: "  method: container-restore\n  image: postgres:16\n"},
		{name: "db-restore-drill with image", verify: "  method: db-restore-drill\n  image: postgres:16\n"},
		{name: "smoke with http probe", verify: "  method: restart-smoke\n  smoke:\n    container: web\n    timeout: 90s\n    http:\n      port: 80\n      path: /alive\n      expectStatus: 204\n"},
		{name: "smoke with tcp probe", verify: "  method: restart-smoke\n  smoke:\n    container: web\n    tcp:\n      port: 5432\n"},
		{name: "method and methods both", verify: "  method: archive-integrity\n  methods: [archive-integrity]\n", wantErr: "catalog validation"},
		{name: "neither method nor methods", verify: "  restartPolicy: never\n", wantErr: "catalog validation"},
		{name: "empty methods list", verify: "  methods: []\n", wantErr: "catalog validation"},
		{name: "duplicate methods", verify: "  methods: [archive-integrity, archive-integrity]\n", wantErr: "catalog validation"},
		{name: "unknown method", verify: "  method: vibes-check\n", wantErr: "catalog validation"},
		{name: "unknown method in list", verify: "  methods: [archive-integrity, vibes-check]\n", wantErr: "catalog validation"},
		{name: "db-restore-drill without image", verify: "  method: db-restore-drill\n", wantErr: "image"},
		{name: "db-restore-drill in list without image", verify: "  methods: [archive-integrity, db-restore-drill]\n", wantErr: "image"},
		{name: "container-restore without image", verify: "  method: container-restore\n", wantErr: "image"},
		{name: "restart-smoke without smoke", verify: "  method: restart-smoke\n", wantErr: "smoke"},
		{name: "restart-smoke in list without smoke", verify: "  methods: [archive-integrity, restart-smoke]\n", wantErr: "smoke"},
		{name: "smoke without container", verify: "  method: restart-smoke\n  smoke:\n    timeout: 30s\n", wantErr: "container"},
		{name: "smoke with http and tcp", verify: "  method: restart-smoke\n  smoke:\n    container: web\n    http: {port: 80}\n    tcp: {port: 80}\n", wantErr: "catalog validation"},
		{name: "smoke port out of range", verify: "  method: restart-smoke\n  smoke:\n    container: web\n    tcp: {port: 70000}\n", wantErr: "catalog validation"},
		{name: "smoke bad timeout", verify: "  method: restart-smoke\n  smoke:\n    container: web\n    timeout: soon\n", wantErr: "catalog validation"},
		{name: "smoke path without slash", verify: "  method: restart-smoke\n  smoke:\n    container: web\n    http: {port: 80, path: alive}\n", wantErr: "catalog validation"},
		{name: "smoke unknown key", verify: "  method: restart-smoke\n  smoke:\n    container: web\n    udp: {port: 53}\n", wantErr: "not allowed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Load([]byte(verifyEntryYAML(tc.verify)))
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("Load: unexpected error %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("Load: accepted, want error containing %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("Load error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestVerifyMethodList(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		v    Verify
		want []string
	}{
		{name: "empty", v: Verify{}, want: []string{}},
		{name: "single", v: Verify{Method: "archive-integrity"}, want: []string{"archive-integrity"}},
		{name: "list order kept", v: Verify{Methods: []string{"restart-smoke", "archive-integrity"}}, want: []string{"restart-smoke", "archive-integrity"}},
		{name: "legacy alias", v: Verify{Method: "container-restore"}, want: []string{"db-restore-drill"}},
		{name: "alias and new name dedupe", v: Verify{Methods: []string{"container-restore", "db-restore-drill", "archive-integrity"}}, want: []string{"db-restore-drill", "archive-integrity"}},
		{name: "method first then methods", v: Verify{Method: "archive-integrity", Methods: []string{"restart-smoke", "archive-integrity"}}, want: []string{"archive-integrity", "restart-smoke"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tc.v.MethodList()
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("MethodList() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDeepCopyVerifySmoke(t *testing.T) {
	t.Parallel()
	orig := &Entry{Verify: Verify{
		Methods: []string{"archive-integrity", "restart-smoke"},
		Smoke: &Smoke{
			Container: "web",
			HTTP:      &SmokeHTTP{Port: 80, Path: "/alive"},
			TCP:       &SmokeTCP{Port: 81},
		},
	}}
	c := orig.DeepCopy()
	c.Verify.Methods[0] = "changed"
	c.Verify.Smoke.Container = "changed"
	c.Verify.Smoke.HTTP.Path = "/changed"
	c.Verify.Smoke.TCP.Port = 1
	if orig.Verify.Methods[0] != "archive-integrity" || orig.Verify.Smoke.Container != "web" ||
		orig.Verify.Smoke.HTTP.Path != "/alive" || orig.Verify.Smoke.TCP.Port != 81 {
		t.Errorf("mutating the copy changed the original: %+v / %+v / %+v", orig.Verify, *orig.Verify.Smoke.HTTP, *orig.Verify.Smoke.TCP)
	}
}

func TestResolveSmokeTemplates(t *testing.T) {
	t.Parallel()
	e, err := Load([]byte(verifyEntryYAML("  method: restart-smoke\n  smoke:\n    container: \"{{ .Params.container }}\"\n    http:\n      port: 80\n      path: \"/{{ .Params.container }}/alive\"\n")))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := e.Resolve(TemplateContext{Params: map[string]string{"container": "vw"}}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if e.Verify.Smoke.Container != "vw" {
		t.Errorf("Smoke.Container = %q, want vw", e.Verify.Smoke.Container)
	}
	if e.Verify.Smoke.HTTP.Path != "/vw/alive" {
		t.Errorf("Smoke.HTTP.Path = %q, want /vw/alive", e.Verify.Smoke.HTTP.Path)
	}

	bad, err := Load([]byte(verifyEntryYAML("  method: restart-smoke\n  smoke:\n    container: \"{{ .Params.nope }}\"\n")))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := bad.Resolve(TemplateContext{Params: map[string]string{"container": "vw"}}); err == nil || !strings.Contains(err.Error(), "smoke container") {
		t.Errorf("Resolve with unknown param = %v, want a smoke container error", err)
	}
}

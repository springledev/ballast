package detect

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	bundled "github.com/springledev/ballast/catalog"
	"github.com/springledev/ballast/internal/catalog"
	"github.com/springledev/ballast/internal/runner"
)

// dockerPsStub is a fake `docker` CLI answering only `docker ps
// --format ...` with a fixed set of running containers, so detection
// tests are hermetic (no Docker daemon needed).
const dockerPsStub = `#!/bin/sh
if [ "$1" = "ps" ]; then
  printf 'immich_postgres\tghcr.io/immich-app/postgres:14\n'
  printf 'my-app\tsome/other-image:latest\n'
  printf 'mysql-db\tmysql:8\n'
fi
exit 0
`

func withStubDocker(t *testing.T) *runner.Runner {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "docker")
	if err := os.WriteFile(path, []byte(dockerPsStub), 0o755); err != nil {
		t.Fatalf("writing docker stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return &runner.Runner{}
}

func TestListRunning(t *testing.T) {
	r := withStubDocker(t)
	containers, err := ListRunning(context.Background(), r)
	if err != nil {
		t.Fatalf("ListRunning error: %v", err)
	}
	want := []Container{
		{Name: "immich_postgres", Image: "ghcr.io/immich-app/postgres:14"},
		{Name: "my-app", Image: "some/other-image:latest"},
		{Name: "mysql-db", Image: "mysql:8"},
	}
	if len(containers) != len(want) {
		t.Fatalf("ListRunning() = %+v, want %+v", containers, want)
	}
	for i, c := range containers {
		if c != want[i] {
			t.Errorf("container[%d] = %+v, want %+v", i, c, want[i])
		}
	}
}

func TestListRunning_DockerMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no docker binary anywhere on PATH
	_, err := ListRunning(context.Background(), &runner.Runner{})
	if err == nil {
		t.Fatal("ListRunning() with no docker binary: want an error")
	}
	if !strings.Contains(err.Error(), "docker is not installed") {
		t.Errorf("error = %q, want it to say docker is not installed", err)
	}
}

func TestSuggest(t *testing.T) {
	entries := []*catalog.Entry{
		{Service: "postgres", Detect: catalog.Detect{Docker: &catalog.DockerDetect{ImagePattern: "postgres"}}},
		{Service: "mysql", Detect: catalog.Detect{Docker: &catalog.DockerDetect{ImagePattern: "mysql|mariadb"}}},
		{Service: "folder"}, // no detect rule: never suggested
	}
	containers := []Container{
		{Name: "immich_postgres", Image: "ghcr.io/immich-app/postgres:14"},
		{Name: "my-app", Image: "some/other-image:latest"},
		{Name: "mysql-db", Image: "mysql:8"},
	}

	got := Suggest(entries, containers)
	if len(got) != 2 {
		t.Fatalf("Suggest() = %+v, want 2 suggestions", got)
	}
	// mysql|mariadb pattern (13 chars) is longer than postgres (8 chars), so mysql comes first
	if got[0].Service != "mysql" || len(got[0].Containers) != 1 || got[0].Containers[0].Name != "mysql-db" {
		t.Errorf("mysql suggestion = %+v, want mysql-db matched", got[0])
	}
	if got[1].Service != "postgres" || len(got[1].Containers) != 1 || got[1].Containers[0].Name != "immich_postgres" {
		t.Errorf("postgres suggestion = %+v, want immich_postgres matched", got[1])
	}
}

// TestSuggestBundledCatalogPrefersApps runs detection against the
// real bundled catalog: on a host running a typical app stack, the
// app's own entry is suggested first, never the generic database
// entry that also matches one of its containers.
func TestSuggestBundledCatalogPrefersApps(t *testing.T) {
	reg, err := catalog.LoadRegistry(bundled.Bundled, nil)
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	tests := []struct {
		name       string
		containers []Container
		want       []string
	}{
		{
			name: "immich compose stack",
			containers: []Container{
				{Name: "immich_server", Image: "ghcr.io/immich-app/immich-server:release"},
				{Name: "immich_postgres", Image: "ghcr.io/immich-app/postgres:14-vectorchord0.4.3-pgvectors0.2.0"},
				{Name: "immich_redis", Image: "docker.io/valkey/valkey:8-bookworm"},
			},
			want: []string{"immich", "postgres"},
		},
		{
			name: "several apps",
			containers: []Container{
				{Name: "couchdb", Image: "couchdb:3"},
				{Name: "vaultwarden", Image: "vaultwarden/server:latest"},
				{Name: "immich_server", Image: "ghcr.io/immich-app/immich-server:release"},
			},
			want: []string{"immich", "vaultwarden", "couchdb"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Suggest(reg.List(), tt.containers)
			var names []string
			for _, s := range got {
				names = append(names, s.Service)
			}
			if strings.Join(names, ",") != strings.Join(tt.want, ",") {
				t.Errorf("Suggest() order = %v, want %v", names, tt.want)
			}
		})
	}
}

func TestSuggest_NoMatches(t *testing.T) {
	entries := []*catalog.Entry{
		{Service: "postgres", Detect: catalog.Detect{Docker: &catalog.DockerDetect{ImagePattern: "postgres"}}},
	}
	got := Suggest(entries, []Container{{Name: "unrelated", Image: "nginx:latest"}})
	if len(got) != 0 {
		t.Errorf("Suggest() = %+v, want no suggestions", got)
	}
}

func TestSuggest_MariaDBDetection(t *testing.T) {
	// Test that regex patterns work: mysql|mariadb should match both MySQL and MariaDB images
	entries := []*catalog.Entry{
		{Service: "mysql", Detect: catalog.Detect{Docker: &catalog.DockerDetect{ImagePattern: "mysql|mariadb"}}},
		{Service: "postgres", Detect: catalog.Detect{Docker: &catalog.DockerDetect{ImagePattern: "postgres"}}},
	}
	tests := []struct {
		name       string
		containers []Container
		want       []string // service names
	}{
		{
			name: "MySQL image matches mysql|mariadb pattern",
			containers: []Container{
				{Name: "mysql-db", Image: "mysql:8"},
			},
			want: []string{"mysql"},
		},
		{
			name: "MariaDB image matches mysql|mariadb pattern",
			containers: []Container{
				{Name: "mariadb-db", Image: "mariadb:11"},
			},
			want: []string{"mysql"},
		},
		{
			name: "Both MySQL and MariaDB images matched by same pattern",
			containers: []Container{
				{Name: "mysql-db", Image: "mysql:8"},
				{Name: "mariadb-db", Image: "mariadb:11"},
			},
			want: []string{"mysql"},
		},
		{
			name: "mariadb image matched by mysql|mariadb, postgres image by postgres pattern",
			containers: []Container{
				{Name: "mariadb-db", Image: "mariadb:11"},
				{Name: "postgres-db", Image: "postgres:16"},
			},
			want: []string{"mysql", "postgres"},
		},
		{
			name: "single-word patterns still work like before",
			containers: []Container{
				{Name: "postgres-db", Image: "ghcr.io/immich-app/postgres:14"},
			},
			want: []string{"postgres"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Suggest(entries, tt.containers)
			var names []string
			for _, s := range got {
				names = append(names, s.Service)
			}
			if strings.Join(names, ",") != strings.Join(tt.want, ",") {
				t.Errorf("Suggest() order = %v, want %v", names, tt.want)
			}
		})
	}
}

// TestResolveDiscovery tests the discovery resolution mechanism with
// docker-label rules for error conditions and edge cases.
func TestResolveDiscovery(t *testing.T) {
	dir := t.TempDir()
	// Stub docker that returns empty (no containers match the filter)
	emptyStub := `#!/bin/sh
if [ "$1" = "ps" ]; then
  exit 0
fi
exit 1
`
	path := filepath.Join(dir, "docker")
	if err := os.WriteFile(path, []byte(emptyStub), 0o755); err != nil {
		t.Fatalf("writing docker stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	r := &runner.Runner{}

	tests := []struct {
		name      string
		discovery map[string]catalog.DiscoveryRule
		params    map[string]string
		want      map[string]string
		wantErr   string
	}{
		{
			name:      "empty discovery returns nil",
			discovery: map[string]catalog.DiscoveryRule{},
			params:    map[string]string{},
			want:      nil,
		},
		{
			name:      "nil discovery returns nil",
			discovery: nil,
			params:    map[string]string{},
			want:      nil,
		},
		{
			name: "no match with fallback resolves to fallback",
			discovery: map[string]catalog.DiscoveryRule{
				"database": {
					Type:     "docker-label",
					Match:    "nomatch=ever",
					Fallback: "{{ .Params.db_container }}",
				},
			},
			params: map[string]string{"db_container": "fallback-db"},
			want:   map[string]string{"database": "fallback-db"},
		},
		{
			name: "no match without fallback errors",
			discovery: map[string]catalog.DiscoveryRule{
				"database": {
					Type:  "docker-label",
					Match: "nomatch=ever",
				},
			},
			params:  map[string]string{},
			wantErr: "no running container matched",
		},
		{
			name: "fallback that resolves to empty errors",
			discovery: map[string]catalog.DiscoveryRule{
				"database": {
					Type:     "docker-label",
					Match:    "nomatch=ever",
					Fallback: "{{ .Params.empty }}",
				},
			},
			params:  map[string]string{"empty": ""},
			wantErr: "resolved to an empty value",
		},
		{
			name: "unsupported discovery type errors",
			discovery: map[string]catalog.DiscoveryRule{
				"database": {
					Type:  "unsupported-type",
					Match: "anything",
				},
			},
			params:  map[string]string{},
			wantErr: "unsupported type",
		},
		{
			name: "empty match field errors",
			discovery: map[string]catalog.DiscoveryRule{
				"database": {
					Type:  "docker-label",
					Match: "",
				},
			},
			params:  map[string]string{},
			wantErr: "match must not be empty",
		},
		{
			name: "fallback with unresolvable param errors",
			discovery: map[string]catalog.DiscoveryRule{
				"database": {
					Type:     "docker-label",
					Match:    "nomatch=ever",
					Fallback: "{{ .Params.missing }}",
				},
			},
			params:  map[string]string{},
			wantErr: `unknown parameter "missing"`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveDiscovery(context.Background(), r, "", tc.discovery, tc.params)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("ResolveDiscovery() error = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveDiscovery() error = %v, want nil", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("ResolveDiscovery() = %+v, want %+v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("ResolveDiscovery()[%s] = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

// TestResolveDiscovery_DockerLabelMatch tests a successful docker-label
// match by stubbing the docker ps output with a specific label match.
func TestResolveDiscovery_DockerLabelMatch(t *testing.T) {
	dir := t.TempDir()
	// Stub docker that echoes back the filter argument and returns a container name
	labelStub := `#!/bin/sh
if [ "$1" = "ps" ] && [ "$3" = "label=service=database" ]; then
  printf 'resolved-database-container\n'
  exit 0
fi
exit 1
`
	path := filepath.Join(dir, "docker")
	if err := os.WriteFile(path, []byte(labelStub), 0o755); err != nil {
		t.Fatalf("writing docker stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	got, err := ResolveDiscovery(context.Background(), &runner.Runner{}, "", map[string]catalog.DiscoveryRule{
		"database": {
			Type:  "docker-label",
			Match: "service=database",
		},
	}, map[string]string{})
	if err != nil {
		t.Fatalf("ResolveDiscovery() error = %v", err)
	}
	if got["database"] != "resolved-database-container" {
		t.Errorf("ResolveDiscovery() database = %q, want resolved-database-container", got["database"])
	}
}

// TestResolveDiscovery_AmbiguousMatch tests that multiple matching containers
// is an error.
func TestResolveDiscovery_AmbiguousMatch(t *testing.T) {
	dir := t.TempDir()
	// Stub docker that returns multiple containers for one label
	labelStub := `#!/bin/sh
if [ "$1" = "ps" ]; then
  printf 'container-1\n'
  printf 'container-2\n'
  exit 0
fi
exit 1
`
	path := filepath.Join(dir, "docker")
	if err := os.WriteFile(path, []byte(labelStub), 0o755); err != nil {
		t.Fatalf("writing docker stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := ResolveDiscovery(context.Background(), &runner.Runner{}, "", map[string]catalog.DiscoveryRule{
		"database": {
			Type:  "docker-label",
			Match: "service=ambiguous",
		},
	}, map[string]string{})
	if err == nil || !strings.Contains(err.Error(), "matched 2 running containers") {
		t.Errorf("ResolveDiscovery(ambiguous) error = %v, want ambiguity error", err)
	}
}

// TestResolveDiscoveryOffline checks that when Docker cannot be asked,
// declared fallbacks stand in, and a rule without one returns the
// original Docker error.
func TestResolveDiscoveryOffline(t *testing.T) {
	dockerBin := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(dockerBin, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("writing docker stub: %v", err)
	}
	tests := []struct {
		name      string
		discovery map[string]catalog.DiscoveryRule
		want      map[string]string
		wantErr   string
	}{
		{
			name:      "fallback used when docker fails",
			discovery: map[string]catalog.DiscoveryRule{"database": {Type: "docker-label", Match: "svc=db", Fallback: "{{ .Params.db }}"}},
			want:      map[string]string{"database": "my-db"},
		},
		{
			name:      "no fallback returns docker error",
			discovery: map[string]catalog.DiscoveryRule{"database": {Type: "docker-label", Match: "svc=db"}},
			wantErr:   "discovery \"database\"",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveDiscoveryOffline(context.Background(), &runner.Runner{}, dockerBin, tt.discovery, map[string]string{"db": "my-db"})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveDiscoveryOffline: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

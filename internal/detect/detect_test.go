package detect

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
		{Service: "mysql", Detect: catalog.Detect{Docker: &catalog.DockerDetect{ImagePattern: "mysql"}}},
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
	if got[0].Service != "postgres" || len(got[0].Containers) != 1 || got[0].Containers[0].Name != "immich_postgres" {
		t.Errorf("postgres suggestion = %+v, want immich_postgres matched", got[0])
	}
	if got[1].Service != "mysql" || len(got[1].Containers) != 1 || got[1].Containers[0].Name != "mysql-db" {
		t.Errorf("mysql suggestion = %+v, want mysql-db matched", got[1])
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

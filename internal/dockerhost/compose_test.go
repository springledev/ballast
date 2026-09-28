package dockerhost

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/springledev/ballast/internal/runner"
)

func TestListComposeProjects(t *testing.T) {
	script := `if [ "$1 $2 $3 $4" = "ps -a --format {{json .}}" ]; then
  printf '%s\n' '{"Names":"immich_server","Image":"ghcr.io/immich-app/immich-server","Labels":"com.docker.compose.project=immich,com.docker.compose.service=immich-server,com.docker.compose.project.working_dir=/opt/immich,com.docker.compose.project.config_files=/opt/immich/docker-compose.yml"}'
  printf '%s\n' '{"Names":"immich_postgres","Image":"ghcr.io/immich-app/postgres","Labels":"com.docker.compose.project=immich,com.docker.compose.service=database,com.docker.compose.project.working_dir=/opt/immich,com.docker.compose.project.config_files=/opt/immich/docker-compose.yml"}'
  printf '%s\n' '{"Names":"lone_container","Image":"nginx","Labels":""}'
  printf '%s\n' '{"Names":"other_web","Image":"httpd","Labels":"com.docker.compose.project=other,com.docker.compose.service=web,com.docker.compose.project.working_dir=/opt/other,com.docker.compose.project.config_files=/opt/other/compose.yaml"}'
fi
exit 0
`
	d := stubDocker(t, script)
	got, err := ListComposeProjects(context.Background(), &runner.Runner{}, d)
	if err != nil {
		t.Fatalf("ListComposeProjects error: %v", err)
	}
	want := []ComposeProject{
		{
			Name:        "immich",
			WorkingDir:  "/opt/immich",
			ConfigFiles: []string{"/opt/immich/docker-compose.yml"},
			Services:    []string{"database", "immich-server"},
			Containers: []ComposeContainer{
				{Name: "immich_postgres", Service: "database", Image: "ghcr.io/immich-app/postgres", Labels: map[string]string{
					"com.docker.compose.project":              "immich",
					"com.docker.compose.service":              "database",
					"com.docker.compose.project.working_dir":  "/opt/immich",
					"com.docker.compose.project.config_files": "/opt/immich/docker-compose.yml",
				}},
				{Name: "immich_server", Service: "immich-server", Image: "ghcr.io/immich-app/immich-server", Labels: map[string]string{
					"com.docker.compose.project":              "immich",
					"com.docker.compose.service":              "immich-server",
					"com.docker.compose.project.working_dir":  "/opt/immich",
					"com.docker.compose.project.config_files": "/opt/immich/docker-compose.yml",
				}},
			},
		},
		{
			Name:        "other",
			WorkingDir:  "/opt/other",
			ConfigFiles: []string{"/opt/other/compose.yaml"},
			Services:    []string{"web"},
			Containers: []ComposeContainer{
				{Name: "other_web", Service: "web", Image: "httpd", Labels: map[string]string{
					"com.docker.compose.project":              "other",
					"com.docker.compose.service":              "web",
					"com.docker.compose.project.working_dir":  "/opt/other",
					"com.docker.compose.project.config_files": "/opt/other/compose.yaml",
				}},
			},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ListComposeProjects() = %+v, want %+v", got, want)
	}
}

func TestListComposeProjectsEmpty(t *testing.T) {
	d := stubDocker(t, "exit 0\n")
	got, err := ListComposeProjects(context.Background(), &runner.Runner{}, d)
	if err != nil {
		t.Fatalf("ListComposeProjects error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ListComposeProjects() = %+v, want empty", got)
	}
}

func TestListComposeProjectsError(t *testing.T) {
	d := stubDocker(t, `echo "permission denied while trying to connect to the Docker daemon socket at unix:///var/run/docker.sock" >&2; exit 1`+"\n")
	_, err := ListComposeProjects(context.Background(), &runner.Runner{}, d)
	if err == nil || !strings.Contains(err.Error(), "docker group") {
		t.Errorf("ListComposeProjects() error = %v, want a docker-group hint", err)
	}
}

func TestListComposeProjectsBadOutput(t *testing.T) {
	d := stubDocker(t, `printf 'not json\n'; exit 0`+"\n")
	_, err := ListComposeProjects(context.Background(), &runner.Runner{}, d)
	if err == nil || !strings.Contains(err.Error(), "unexpected output") {
		t.Errorf("ListComposeProjects() error = %v, want unexpected output", err)
	}
}

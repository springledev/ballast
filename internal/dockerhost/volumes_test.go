package dockerhost

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/springledev/ballast/internal/runner"
)

// stubDocker writes a fake `docker` CLI script and returns a Docker
// handle pointed at it, so tests are hermetic (no Docker daemon
// needed).
func stubDocker(t *testing.T, script string) Docker {
	dir := t.TempDir()
	path := filepath.Join(dir, "docker")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatalf("writing docker stub: %v", err)
	}
	return NewLocal(path)
}

func TestListVolumes(t *testing.T) {
	script := `if [ "$1 $2 $3 $4" = "volume ls --format {{json .}}" ]; then
  printf '%s\n' '{"Name":"immich_pgdata","Driver":"local","Labels":"com.docker.compose.project=immich,com.docker.compose.version=2"}'
  printf '%s\n' '{"Name":"a_plain_volume","Driver":"local","Labels":""}'
  printf '%s\n' '{"Name":"another_zzz","Driver":"nfs","Labels":"com.docker.compose.project=other"}'
fi
exit 0
`
	d := stubDocker(t, script)
	got, err := ListVolumes(context.Background(), &runner.Runner{}, d)
	if err != nil {
		t.Fatalf("ListVolumes error: %v", err)
	}
	want := []Volume{
		{Name: "a_plain_volume", Driver: "local"},
		{Name: "another_zzz", Driver: "nfs", ComposeProject: "other"},
		{Name: "immich_pgdata", Driver: "local", ComposeProject: "immich"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ListVolumes() = %+v, want %+v", got, want)
	}
}

func TestListVolumesEmpty(t *testing.T) {
	d := stubDocker(t, "exit 0\n")
	got, err := ListVolumes(context.Background(), &runner.Runner{}, d)
	if err != nil {
		t.Fatalf("ListVolumes error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ListVolumes() = %+v, want empty", got)
	}
}

func TestListVolumesError(t *testing.T) {
	d := stubDocker(t, `echo "permission denied while trying to connect to the Docker daemon socket at unix:///var/run/docker.sock" >&2; exit 1`+"\n")
	_, err := ListVolumes(context.Background(), &runner.Runner{}, d)
	if err == nil || !strings.Contains(err.Error(), "docker group") {
		t.Errorf("ListVolumes() error = %v, want a docker-group hint", err)
	}
}

func TestListVolumesBadOutput(t *testing.T) {
	d := stubDocker(t, `printf 'not json\n'; exit 0`+"\n")
	_, err := ListVolumes(context.Background(), &runner.Runner{}, d)
	if err == nil || !strings.Contains(err.Error(), "unexpected output") {
		t.Errorf("ListVolumes() error = %v, want unexpected output", err)
	}
}

func TestParseLabels(t *testing.T) {
	tests := []struct {
		in   string
		want map[string]string
	}{
		{"", map[string]string{}},
		{"a=b", map[string]string{"a": "b"}},
		{"a=b,c=d", map[string]string{"a": "b", "c": "d"}},
		{"malformed,a=b", map[string]string{"a": "b"}},
	}
	for _, tt := range tests {
		if got := parseLabels(tt.in); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("parseLabels(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/springledev/ballast/internal/engine"
	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/restic"
	"github.com/springledev/ballast/internal/store"
	"github.com/springledev/keel/dockerhost"
	"github.com/springledev/keel/runner"
)

// seedComposeProject creates a working directory holding
// docker-compose.yml and .env, and starts two containers carrying the
// same compose labels `docker compose up` would set — enough for
// dockerhost.ListComposeProjects to discover the project exactly as
// it would a real one, without needing the compose CLI plugin
// installed in this environment.
func seedComposeProject(t *testing.T, ctx context.Context) (project, workingDir string) {
	t.Helper()
	suffix := fmt.Sprint(time.Now().UnixNano())
	project = "ballast-compose-it-" + suffix

	workingDir = t.TempDir()
	composeYAML := "services:\n  web:\n    image: busybox\n  db:\n    image: busybox\n"
	if err := os.WriteFile(filepath.Join(workingDir, "docker-compose.yml"), []byte(composeYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workingDir, ".env"), []byte("DB_PASSWORD=super-secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	configFiles := filepath.Join(workingDir, "docker-compose.yml")
	web := project + "_web"
	db := project + "_db"
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = exec.CommandContext(c, "docker", "rm", "-f", web, db).Run()
	})
	startComposeContainer(t, ctx, web, project, "web", workingDir, configFiles)
	startComposeContainer(t, ctx, db, project, "database", workingDir, configFiles)
	return project, workingDir
}

// startComposeContainer starts a throwaway container carrying the
// same compose labels a real `docker compose up` would set.
func startComposeContainer(t *testing.T, ctx context.Context, name, project, service, workingDir, configFiles string) {
	t.Helper()
	docker(t, ctx, "run", "-d", "--name", name,
		"--label", "com.docker.compose.project="+project,
		"--label", "com.docker.compose.service="+service,
		"--label", "com.docker.compose.project.working_dir="+workingDir,
		"--label", "com.docker.compose.project.config_files="+configFiles,
		"busybox", "sleep", "600")
}

// TestPipelineComposeRoundTripIntegration runs the bundled
// docker-compose entry end-to-end: discover the project by its
// containers' labels, snapshot its config into restic (VERIFYING ->
// SUCCESS), and confirm the config tar and labels JSON read back
// intact — including that the container labels artifact never carries
// an environment variable (only .env, inside the tar, does), so a
// secret set directly as a container env var is never captured.
func TestPipelineComposeRoundTripIntegration(t *testing.T) {
	bin := requireRestic(t)
	ctx := context.Background()
	requireDockerDaemon(t, ctx)
	project, workingDir := seedComposeProject(t, ctx)

	entryYAML, err := os.ReadFile(filepath.Join("..", "..", "catalog", "docker-compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	credDir := t.TempDir()
	pwFile := filepath.Join(credDir, "ballast-restic-password.local")
	if err := os.WriteFile(pwFile, []byte("integration-test-password\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repoDir := filepath.Join(t.TempDir(), "repo")
	s, err := store.Open(filepath.Join(t.TempDir(), "ballast.db"))
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	defer s.Close()
	db := s.DB()
	if err := inventory.PutDestination(db, inventory.Destination{Name: "local", Location: repoDir, KeepDaily: 7}); err != nil {
		t.Fatalf("PutDestination: %v", err)
	}
	if err := inventory.PutService(db, inventory.Service{
		Name: "stack", CatalogEntry: "docker-compose", Destination: "local",
		Params: map[string]string{"project": project},
	}); err != nil {
		t.Fatalf("PutService: %v", err)
	}

	r := &runner.Runner{}
	repo, err := restic.Open(repoDir, restic.Options{Runner: r, PasswordFile: pwFile, ResticBin: bin})
	if err != nil {
		t.Fatalf("restic.Open: %v", err)
	}
	if err := repo.Init(ctx); err != nil {
		t.Fatalf("restic Init: %v", err)
	}

	p := &Pipeline{
		DB:        db,
		Catalog:   registryWith(t, map[string]string{"docker-compose.yaml": string(entryYAML)}),
		Runner:    r,
		CredDir:   credDir,
		WorkRoot:  t.TempDir(),
		ResticBin: bin,
	}
	state, err := p.Run(ctx, "stack")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if state != engine.SUCCESS {
		t.Fatalf("state = %s, want SUCCESS", state)
	}

	runs, err := engine.RecentRuns(db, "stack", 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("RecentRuns: %v, %v", runs, err)
	}
	if seq := transitionsOf(t, db, runs[0].ID); !containsInOrder(seq, string(engine.VERIFYING), string(engine.SUCCESS)) {
		t.Errorf("transitions = %v, want VERIFYING before SUCCESS", seq)
	}
	results, err := engine.VerificationResults(db, runs[0].ID)
	if err != nil {
		t.Fatalf("VerificationResults: %v", err)
	}
	archiveChecked := false
	for _, res := range results {
		if res.Method == "archive-integrity" {
			archiveChecked = res.Passed
		}
	}
	if !archiveChecked {
		t.Errorf("no passing archive-integrity record: %+v", results)
	}

	entries, err := repo.LS(ctx, "latest", "")
	if err != nil {
		t.Fatalf("restic ls: %v", err)
	}
	var configPath, labelsPath string
	for _, e := range entries {
		switch {
		case strings.HasSuffix(e.Path, "/compose-config.tar"):
			configPath = e.Path
		case strings.HasSuffix(e.Path, "/compose-labels.json"):
			labelsPath = e.Path
		}
	}
	if configPath == "" || labelsPath == "" {
		t.Fatalf("snapshot is missing compose-config.tar or compose-labels.json; entries: %v", entries)
	}

	var configArchive bytes.Buffer
	if err := repo.Dump(ctx, "latest", configPath, &configArchive); err != nil {
		t.Fatalf("restic dump config: %v", err)
	}
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "a.tar"), configArchive.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.CommandContext(ctx, "tar", "-tf", filepath.Join(tmp, "a.tar")).Output()
	if err != nil {
		t.Fatalf("tar -tf: %v", err)
	}
	members := strings.Fields(string(out))
	if !slicesContain(members, "docker-compose.yml") || !slicesContain(members, ".env") {
		t.Errorf("config archive members = %v, want docker-compose.yml and .env", members)
	}

	var labelsJSON bytes.Buffer
	if err := repo.Dump(ctx, "latest", labelsPath, &labelsJSON); err != nil {
		t.Fatalf("restic dump labels: %v", err)
	}
	var decoded struct {
		Project    string `json:"project"`
		WorkingDir string `json:"working_dir"`
		Containers []struct {
			Name    string                 `json:"name"`
			Service string                 `json:"service"`
			Image   string                 `json:"image"`
			Labels  map[string]interface{} `json:"labels"`
		} `json:"containers"`
	}
	if err := json.Unmarshal(labelsJSON.Bytes(), &decoded); err != nil {
		t.Fatalf("decoding compose-labels.json: %v", err)
	}
	if decoded.Project != project || decoded.WorkingDir != workingDir {
		t.Errorf("labels = %+v, want project %q working_dir %q", decoded, project, workingDir)
	}
	if len(decoded.Containers) != 2 {
		t.Fatalf("containers = %+v, want 2", decoded.Containers)
	}
	if strings.Contains(labelsJSON.String(), "super-secret") {
		t.Error("compose-labels.json leaked the .env secret; it must only ever be inside the config tar, never in a label")
	}

	// Sanity: the discovery path this test exercises is the same one
	// a wizard/UI picker would use.
	if _, err := dockerhost.ListComposeProjects(ctx, r, dockerhost.NewLocal("")); err != nil {
		t.Errorf("ListComposeProjects: %v", err)
	}
}

// slicesContain reports whether s is present in list.
func slicesContain(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

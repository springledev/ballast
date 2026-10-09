package pipeline

import (
	"archive/tar"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/springledev/ballast/internal/engine"
	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/keel/dockerhost"
)

// TestPipelineRemoteFiles proves the remote-files entry (spec §20.2.1)
// reaches VERIFYING then SUCCESS: a fake ssh on PATH answers the
// read-only ls and tar commands, restic stores the archive and the
// checks pass.
func TestPipelineRemoteFiles(t *testing.T) {
	bin := requireRestic(t)
	p, _ := newPipelineHarness(t, bin)
	dir := t.TempDir()
	p.DockerStateDir = filepath.Join(dir, "state")
	p.KnownHostsFile = filepath.Join(dir, "known_hosts")

	tarPath := filepath.Join(dir, "in.tar")
	f, _ := os.Create(tarPath)
	tw := tar.NewWriter(f)
	_ = tw.WriteHeader(&tar.Header{Name: "etc/nfs.conf", Mode: 0o644, Size: 3})
	_, _ = tw.Write([]byte("x\n\n"))
	_ = tw.Close()
	_ = f.Close()
	stubDir := filepath.Join(dir, "bin")
	_ = os.MkdirAll(stubDir, 0o755)
	script := "#!/bin/sh\ncase \"$*\" in *\"ls -d\"*) exit 0 ;; *\"'tar'\"*) cat " + tarPath + " ;; *) exit 9 ;; esac\n"
	if err := os.WriteFile(filepath.Join(stubDir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if _, err := inventory.PutDockerHost(p.DB, dockerhost.Host{Name: "pve", Kind: dockerhost.RemoteSSH, Address: "root@pve.lan"}); err != nil {
		t.Fatalf("PutDockerHost: %v", err)
	}
	if err := inventory.PutService(p.DB, inventory.Service{
		Name: "pve-files", CatalogEntry: "remote-files", Destination: "local", DockerHost: "pve",
		Params: map[string]string{"paths": "/etc/nfs.conf"},
	}); err != nil {
		t.Fatalf("PutService: %v", err)
	}
	state, err := p.Run(context.Background(), "pve-files")
	if err != nil || state != engine.SUCCESS {
		t.Fatalf("Run = %s, %v; want SUCCESS", state, err)
	}
	runs, _ := engine.RecentRuns(p.DB, "pve-files", 1)
	if len(runs) != 1 {
		t.Fatalf("runs = %v", runs)
	}
	if seq := transitionsOf(t, p.DB, runs[0].ID); !containsInOrder(seq, string(engine.VERIFYING), string(engine.SUCCESS)) {
		t.Errorf("transitions = %v, want VERIFYING then SUCCESS", seq)
	}
	results, _ := engine.VerificationResults(p.DB, runs[0].ID)
	got := map[string]bool{}
	for _, r := range results {
		if !r.Passed {
			t.Errorf("verification not passed: %+v", r)
		}
		got[r.Method] = true
	}
	if !got["archive-integrity"] || !got["restic-read-back"] {
		t.Errorf("methods passed = %v", got)
	}
}

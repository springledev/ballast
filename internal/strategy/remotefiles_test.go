package strategy

import (
	"archive/tar"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/springledev/ballast/internal/catalog"
	"github.com/springledev/keel/dockerhost"
	"github.com/springledev/keel/runner"
)

// TestPlanRemoteFiles walks the allow-list and secret opt-in rules.
func TestPlanRemoteFiles(t *testing.T) {
	tests := []struct {
		name     string
		paths    []string
		secrets  []string
		wantErr  string
		wantExcl string
	}{
		{name: "plain config files", paths: []string{"/etc/fstab, /etc/exports", "/etc/nfs.conf"}},
		{name: "relative", paths: []string{"etc/fstab"}, wantErr: "absolute"},
		{name: "dotdot", paths: []string{"/etc/../root/.ssh"}, wantErr: "absolute"},
		{name: "outside roots", paths: []string{"/root/.ssh/id_ed25519"}, wantErr: "outside the folders"},
		{name: "pve forbidden", paths: []string{"/etc/pve/priv"}, wantErr: "Proxmox settings snapshot"},
		{name: "wildcard", paths: []string{"/etc/*.conf"}, wantErr: "wildcards"},
		{name: "empty", paths: []string{" , "}, wantErr: "at least one path"},
		{name: "tailscale without opt-in", paths: []string{"/var/lib/tailscale/tailscaled.state"}, wantErr: "outside the folders"},
		{name: "tailscale opted in", paths: []string{"/var/lib/tailscale/tailscaled.state"}, secrets: []string{"tailscale-node-key"}},
		{name: "unknown secret class", paths: []string{"/etc/fstab"}, secrets: []string{"nope"}, wantErr: "unknown secrets choice"},
		{name: "shadow asked for directly", paths: []string{"/etc/shadow"}, wantErr: "password hashes"},
		{name: "shadow opted in", paths: []string{"/etc/shadow"}, secrets: []string{"password-hashes"}},
		{name: "etc folder skips ssh host keys", paths: []string{"/etc"}, wantExcl: "etc/ssh/ssh_host_*"},
		{name: "etc folder with host keys opted in", paths: []string{"/etc"}, secrets: []string{"ssh-host-keys"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := planRemoteFiles("pve-host", &catalog.RemoteFilesSpec{Paths: tt.paths, Secrets: tt.secrets})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) || !strings.Contains(err.Error(), "pve-host") {
					t.Fatalf("err = %v, want it to name the service and contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("planRemoteFiles: %v", err)
			}
			joined := strings.Join(plan.excludes, " ")
			if tt.wantExcl != "" && !strings.Contains(joined, tt.wantExcl) {
				t.Errorf("excludes = %q, want %q", joined, tt.wantExcl)
			}
			if tt.wantExcl == "" && tt.secrets != nil && strings.Contains(joined, "ssh_host") {
				t.Errorf("opted-in secret still excluded: %q", joined)
			}
		})
	}
}

// remoteStub is a fake ssh that records its arguments and, for a tar
// command, emits a tiny tar archive.
const remoteStub = `#!/bin/sh
echo "$@" >> "$STUB_LOG"
case "$*" in
  *"ls -d"*) [ -n "$STUB_MISSING" ] && exit 2; exit 0 ;;
  *"'tar'"*) cat "$STUB_TAR" ;;
esac
`

func remoteOptions(t *testing.T, kind dockerhost.Kind, addr string) (Options, string) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "ssh.log")
	ssh := writeStub(t, dir, "ssh-stub", remoteStub)
	tarPath := filepath.Join(dir, "in.tar")
	f, _ := os.Create(tarPath)
	tw := tar.NewWriter(f)
	_ = tw.WriteHeader(&tar.Header{Name: "etc/fstab", Mode: 0o644, Size: 2})
	_, _ = tw.Write([]byte("x\n"))
	_ = tw.Close()
	_ = f.Close()
	res := dockerhost.Resolver{SSHBin: ssh, StateDir: filepath.Join(dir, "state"), KnownHostsFile: filepath.Join(dir, "kh")}
	h := dockerhost.Host{Name: "pve", Kind: kind, Address: addr}
	if err := h.Normalize(); err != nil {
		t.Fatal(err)
	}
	d, err := res.Docker(h)
	if err != nil {
		t.Fatal(err)
	}
	r := &runner.Runner{BaseEnv: append(os.Environ(), "STUB_LOG="+logPath, "STUB_TAR="+tarPath)}
	return Options{Runner: r, Docker: d}, logPath
}

func remoteEntry() *catalog.Entry {
	return entryFor("remote-files", func(e *catalog.Entry) {
		e.Service = "pve-files"
		e.Backup.RemoteFiles = &catalog.RemoteFilesSpec{Paths: []string{"/etc/fstab", "/etc/nfs.conf"}}
	})
}

// TestRemoteFilesBackup checks the ssh commands are read-only, the
// SSH secrets-free, and the artifact is the streamed tar.
func TestRemoteFilesBackup(t *testing.T) {
	opts, logPath := remoteOptions(t, dockerhost.RemoteSSH, "root@pve.lan:2222")
	s, err := For(remoteEntry(), opts)
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	res, err := s.Backup(context.Background(), Request{WorkDir: filepath.Join(t.TempDir(), "w")})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if len(res.Artifacts) != 1 || res.Artifacts[0].Format != "tar" {
		t.Fatalf("artifacts = %+v", res.Artifacts)
	}
	if fi, err := os.Stat(res.Artifacts[0].Path); err != nil || fi.Mode().Perm() != 0o600 || fi.Size() == 0 {
		t.Fatalf("artifact stat = %v, %v", fi, err)
	}
	log, _ := os.ReadFile(logPath)
	lines := strings.Split(strings.TrimSpace(string(log)), "\n")
	if len(lines) != 2 {
		t.Fatalf("ssh calls = %v, want ls then tar", lines)
	}
	for _, want := range []string{"-p 2222", "-l root", "pve.lan"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("ssh call %q missing %q", lines[0], want)
		}
	}
	if !strings.Contains(lines[0], "ls -d -- '/etc/fstab' '/etc/nfs.conf'") || !strings.Contains(lines[1], "'tar' '--numeric-owner' '-cf' '-'") {
		t.Errorf("unexpected remote commands: %v", lines)
	}
	for _, l := range lines {
		for _, bad := range []string{" rm ", " mv ", " cp ", ">"} {
			if strings.Contains(l, bad) {
				t.Errorf("remote command %q is not read-only (%q)", l, bad)
			}
		}
	}
}

// TestRemoteFilesFailures covers a local host and a missing remote path.
func TestRemoteFilesFailures(t *testing.T) {
	opts, _ := remoteOptions(t, dockerhost.RemoteSSH, "root@pve.lan")
	local := Options{Runner: opts.Runner, Docker: dockerhost.NewLocal("")}
	s, err := For(remoteEntry(), local)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Backup(context.Background(), Request{WorkDir: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "SSH") {
		t.Errorf("local host err = %v, want SSH guidance", err)
	}

	opts.Runner.BaseEnv = append(opts.Runner.BaseEnv, "STUB_MISSING=1")
	s, _ = For(remoteEntry(), opts)
	dir := t.TempDir()
	_, err = s.Backup(context.Background(), Request{WorkDir: dir})
	if err == nil || !strings.Contains(err.Error(), "could not find") || !strings.Contains(err.Error(), "pve-files") {
		t.Errorf("missing path err = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "files.tar")); statErr == nil {
		t.Error("an archive was created although the path check failed")
	}
}

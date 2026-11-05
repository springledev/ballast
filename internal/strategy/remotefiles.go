package strategy

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/springledev/ballast/internal/catalog"
	"github.com/springledev/keel/runner"
)

// remoteFilesArtifact is the name of the tar a remote-files backup
// produces.
const remoteFilesArtifact = "files"

// remoteAllowedRoots are the folders a remote-files service may read
// (spec §20.2.1). Anything else is refused before a connection is made.
var remoteAllowedRoots = []string{"/etc", "/usr/local", "/opt", "/srv", "/var/spool/cron"}

// remoteSecretClass is one kind of secret file that is left out of a
// remote-files backup unless the service opts in by name.
type remoteSecretClass struct {
	// Name is what the service lists under backup.remoteFiles.secrets.
	Name string
	// Paths are absolute paths or tar-style globs (a trailing "*" only).
	Paths []string
	// What is a plain-English description for messages.
	What string
}

// remoteSecretClasses is the opt-in list of spec §20.2.1.
var remoteSecretClasses = []remoteSecretClass{
	{Name: "tailscale-node-key", Paths: []string{"/var/lib/tailscale"}, What: "Tailscale's node key"},
	{Name: "ssh-host-keys", Paths: []string{"/etc/ssh/ssh_host_*"}, What: "the machine's SSH host keys"},
	{Name: "password-hashes", Paths: []string{"/etc/shadow", "/etc/shadow-", "/etc/gshadow", "/etc/gshadow-"}, What: "the machine's password hashes"},
}

// remoteForbidden are paths never read: /etc/pve is a live cluster
// file system that the Proxmox settings snapshot already covers.
var remoteForbidden = []string{"/etc/pve"}

// RemoteFiles implements the remote-files strategy (spec §20.2.1):
// chosen files of another machine are streamed over SSH, read-only,
// into one tar artifact. Nothing is installed or written on the target.
type RemoteFiles struct {
	entry *catalog.Entry
	opts  Options
}

// remotePlan is the validated, expanded form of a remote-files entry.
type remotePlan struct {
	paths    []string
	excludes []string // tar --exclude patterns for secrets not opted in
	skipped  []string // plain-English names of what is left out
}

// splitRemotePaths flattens catalog entries that may hold several
// comma- or newline-separated paths (a single catalog parameter).
func splitRemotePaths(in []string) []string {
	var out []string
	for _, s := range in {
		for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' }) {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

func underOrEqual(p, root string) bool {
	return p == root || strings.HasPrefix(p, root+"/")
}

// matchSecretPath reports whether p is covered by secret pattern pat.
func matchSecretPath(p, pat string) bool {
	ok, _ := path.Match(pat, p)
	if ok {
		return true
	}
	return underOrEqual(p, pat) && !strings.ContainsAny(pat, "*?[")
}

// planRemoteFiles validates the entry against the allow-list.
func planRemoteFiles(service string, rf *catalog.RemoteFilesSpec) (*remotePlan, error) {
	if rf == nil {
		return nil, fmt.Errorf("service %s: remote-files strategy needs backup.remoteFiles", service)
	}
	opted := map[string]bool{}
	for _, name := range splitRemotePaths(rf.Secrets) {
		if name == "none" {
			continue
		}
		known := false
		for _, c := range remoteSecretClasses {
			known = known || c.Name == name
		}
		if !known {
			var names []string
			for _, c := range remoteSecretClasses {
				names = append(names, c.Name)
			}
			return nil, fmt.Errorf("service %s: unknown secrets choice %q (known: %s)", service, name, strings.Join(names, ", "))
		}
		opted[name] = true
	}
	raw := splitRemotePaths(rf.Paths)
	if len(raw) == 0 {
		return nil, fmt.Errorf("service %s: remote-files needs at least one path to read from the remote machine", service)
	}
	plan := &remotePlan{}
	seen := map[string]bool{}
	for _, p := range raw {
		if !path.IsAbs(p) || strings.Contains(p, "..") {
			return nil, fmt.Errorf("service %s: remote path %q must be absolute and must not contain \"..\"", service, p)
		}
		if strings.ContainsAny(p, "\x00\r\n*?[") {
			return nil, fmt.Errorf("service %s: remote path %q must name a file or folder, without wildcards or line breaks", service, p)
		}
		p = path.Clean(p)
		for _, f := range remoteForbidden {
			if underOrEqual(p, f) {
				return nil, fmt.Errorf("service %s: remote path %s is not allowed: it is already covered by the Proxmox settings snapshot", service, p)
			}
		}
		secretOK := false
		for _, c := range remoteSecretClasses {
			if !opted[c.Name] {
				continue
			}
			for _, pat := range c.Paths {
				secretOK = secretOK || matchSecretPath(p, pat)
			}
		}
		inRoot := false
		for _, r := range remoteAllowedRoots {
			inRoot = inRoot || underOrEqual(p, r)
		}
		if !inRoot && !secretOK {
			return nil, fmt.Errorf("service %s: remote path %s is outside the folders Ballast may read from another machine (%s); secret locations need an explicit secrets choice", service, p, strings.Join(remoteAllowedRoots, ", "))
		}
		if seen[p] {
			continue
		}
		seen[p] = true
		plan.paths = append(plan.paths, p)
	}
	// Anything secret that a requested path would sweep in, or that was
	// asked for directly without opting in, is excluded.
	for _, c := range remoteSecretClasses {
		if opted[c.Name] {
			continue
		}
		for _, pat := range c.Paths {
			for _, p := range plan.paths {
				if matchSecretPath(p, pat) {
					return nil, fmt.Errorf("service %s: remote path %s holds %s; add %q to secrets to include it on purpose", service, p, c.What, c.Name)
				}
				if underOrEqual(strings.TrimRight(pat, "*"), p) || underOrEqual(path.Dir(pat), p) {
					plan.excludes = append(plan.excludes, strings.TrimPrefix(pat, "/"))
					plan.skipped = append(plan.skipped, c.What+" ("+pat+")")
					break
				}
			}
		}
	}
	return plan, nil
}

// validateRemoteFilesEntry checks the static shape of the entry.
func validateRemoteFilesEntry(entry *catalog.Entry) error {
	_, err := planRemoteFiles(entry.Service, entry.Backup.RemoteFiles)
	return err
}

// sshTarget is how to run ssh against the service's remote machine.
type sshTarget struct {
	bin  string
	args []string // connection args, before the remote command
	name string   // host name for messages
}

// sshTarget derives the ssh program (the per-host wrapper, which adds
// the key, BatchMode and known-hosts rules) and the user@host from the
// service's remote-ssh Docker host.
func (o Options) sshTarget() (*sshTarget, error) {
	cmd := o.Docker.Command(nil)
	addr := ""
	for i := 0; i+1 < len(cmd.Args); i++ {
		if cmd.Args[i] == "--host" {
			addr = cmd.Args[i+1]
		}
	}
	if !strings.HasPrefix(addr, "ssh://") {
		return nil, fmt.Errorf("remote-files needs a remote machine reached over SSH, but Docker host %s is not one; add the machine under Docker hosts as an SSH host first", o.Docker.Name())
	}
	u, err := url.Parse(addr)
	if err != nil || u.Hostname() == "" {
		return nil, fmt.Errorf("the Docker host %s has an unusable SSH address", o.Docker.Name())
	}
	t := &sshTarget{bin: "ssh", name: o.Docker.Name()}
	for _, e := range cmd.Env {
		if rest, ok := strings.CutPrefix(e, "PATH="); ok {
			dir, _, _ := strings.Cut(rest, string(os.PathListSeparator))
			if p := filepath.Join(dir, "ssh"); dir != "" {
				t.bin = p
			}
		}
	}
	if port := u.Port(); port != "" {
		t.args = append(t.args, "-p", port)
	}
	if u.User != nil {
		t.args = append(t.args, "-l", u.User.Username())
	}
	t.args = append(t.args, "--", u.Hostname())
	return t, nil
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// command builds the ssh command running remote (already quoted).
func (t *sshTarget) command(remote string) runner.Command {
	return runner.Command{Name: t.bin, Args: append(append([]string(nil), t.args...), remote)}
}

// Preflight proves every path exists on the remote machine with a
// read-only `ls -d`, before anything is uploaded.
func (s *RemoteFiles) Preflight(ctx context.Context) error {
	plan, err := planRemoteFiles(s.entry.Service, s.entry.Backup.RemoteFiles)
	if err != nil {
		return err
	}
	t, err := s.opts.sshTarget()
	if err != nil {
		return fmt.Errorf("service %s: %w", s.entry.Service, err)
	}
	q := make([]string, len(plan.paths))
	for i, p := range plan.paths {
		q[i] = shQuote(p)
	}
	if _, err := s.opts.Runner.Run(ctx, nil, nil, t.command("ls -d -- "+strings.Join(q, " "))); err != nil {
		return fmt.Errorf("service %s: could not find all of %s on %s (check the paths and the SSH login; nothing was uploaded): %w", s.entry.Service, strings.Join(plan.paths, ", "), t.name, err)
	}
	return nil
}

// Backup streams the files into <WorkDir>/files.tar (mode 0600).
func (s *RemoteFiles) Backup(ctx context.Context, req Request) (*Result, error) {
	plan, err := planRemoteFiles(s.entry.Service, s.entry.Backup.RemoteFiles)
	if err != nil {
		return nil, err
	}
	t, err := s.opts.sshTarget()
	if err != nil {
		return nil, fmt.Errorf("service %s: %w", s.entry.Service, err)
	}
	if err := ensureWorkDir(req.WorkDir); err != nil {
		return nil, fmt.Errorf("service %s: %w", s.entry.Service, err)
	}
	if err := s.Preflight(ctx); err != nil {
		return nil, err
	}
	dest := filepath.Join(req.WorkDir, remoteFilesArtifact+".tar")
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("service %s: creating archive file: %w", s.entry.Service, err)
	}
	remote := []string{"tar", "--numeric-owner", "-cf", "-"}
	sort.Strings(plan.excludes)
	for _, e := range plan.excludes {
		remote = append(remote, "--exclude="+e)
	}
	remote = append(remote, "--")
	remote = append(remote, plan.paths...)
	for i, a := range remote {
		remote[i] = shQuote(a)
	}
	_, runErr := s.opts.Runner.Run(ctx, nil, f, t.command(strings.Join(remote, " ")))
	closeErr := f.Close()
	if runErr != nil {
		return nil, fmt.Errorf("service %s: reading files from %s: %w", s.entry.Service, t.name, runErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("service %s: writing archive file: %w", s.entry.Service, closeErr)
	}
	return &Result{Artifacts: []Artifact{{Name: remoteFilesArtifact, Path: dest, Format: "tar"}}, Skipped: plan.skipped}, nil
}

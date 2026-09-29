package backend

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/springledev/ballast/internal/restic"
	"github.com/springledev/keel/runner"
	"github.com/springledev/keel/secret"
)

// Target is everything Resolver.Open needs about one destination. It
// mirrors inventory.Destination without importing it (inventory
// imports this package to validate and generate locations).
type Target struct {
	Name     string
	Type     Type
	Settings Settings
	// Location is the restic repository string Location generated.
	Location string
	// PasswordCredential and BackendCredential are credential file
	// NAMES (never paths or values); BackendCredential is empty for a
	// type that needs none.
	PasswordCredential string
	BackendCredential  string
}

// Resolver opens restic repositories for destinations, resolving
// every credential file against the credential directories. The
// pipeline, the API and the CLI all open repositories through one
// Resolver so a destination behaves the same everywhere.
type Resolver struct {
	// CredDirs are searched in order for each credential file
	// (config.Config.CredentialDirs: $CREDENTIALS_DIRECTORY first).
	CredDirs []string
	// Runner executes restic. Required.
	Runner *runner.Runner
	// ResticBin overrides the restic binary (tests).
	ResticBin string
	// Redact receives every secret read from a credential file, fresh
	// on each Open, so a file replaced while Ballast runs is covered.
	Redact *secret.Set
	// KnownHostsFile is where SFTP server host keys are remembered
	// (trust on first use). Required for SFTP destinations.
	KnownHostsFile string
}

// Open returns a restic.Repository for t with its password file,
// backend credential file and backend options attached.
func (r Resolver) Open(t Target) (*restic.Repository, error) {
	if t.PasswordCredential == "" {
		return nil, fmt.Errorf("destination %s: no repository password credential is configured", t.Name)
	}
	pwFile := r.path(t.PasswordCredential)
	if _, err := os.Stat(pwFile); errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("destination %s has not been set up yet: set it up with `ballast destination init %s` (or \"Set up repository\" on the Destinations page); if its repository already exists, put that repository's password in %s first: %w", t.Name, t.Name, pwFile, err)
	}
	r.Redact.AddFile(pwFile)
	opts := restic.Options{Runner: r.Runner, PasswordFile: pwFile, ResticBin: r.ResticBin, RetryLock: restic.DefaultRetryLock}

	kind := t.Type.CredentialKind()
	if kind != NoCredential && t.BackendCredential == "" {
		return nil, fmt.Errorf("destination %s: no backend credential file is configured for this %s destination", t.Name, t.Type)
	}
	switch kind {
	case AWSCredential:
		dir := runner.FindCredentialDir(t.BackendCredential, r.CredDirs...)
		env, err := runner.FileEnv(dir, map[string]string{"AWS_SHARED_CREDENTIALS_FILE": t.BackendCredential})
		if err != nil {
			return nil, fmt.Errorf("destination %s: access keys: %w", t.Name, err)
		}
		r.redactAWSFile(filepath.Join(dir, t.BackendCredential))
		opts.Env = append(env, awsEnv(Region(t.Type, t.Settings))...)
	case SSHCredential:
		key := r.path(t.BackendCredential)
		if err := runner.CheckCredentialFile(key); err != nil {
			return nil, fmt.Errorf("destination %s: SSH key: %w", t.Name, err)
		}
		if r.KnownHostsFile == "" {
			return nil, fmt.Errorf("destination %s: no known_hosts file is configured for SFTP destinations (internal wiring error)", t.Name)
		}
		if err := os.MkdirAll(filepath.Dir(r.KnownHostsFile), 0o700); err != nil {
			return nil, fmt.Errorf("destination %s: creating %s: %w", t.Name, filepath.Dir(r.KnownHostsFile), err)
		}
		args, err := sshArgs(key, r.KnownHostsFile)
		if err != nil {
			return nil, fmt.Errorf("destination %s: %w", t.Name, err)
		}
		opts.ExtendedOptions = []string{"sftp.args=" + args}
	}
	repo, err := restic.Open(t.Location, opts)
	if err != nil {
		return nil, fmt.Errorf("destination %s: %w", t.Name, err)
	}
	return repo, nil
}

func (r Resolver) path(name string) string {
	return filepath.Join(runner.FindCredentialDir(name, r.CredDirs...), name)
}

// awsEnv pins restic's S3 credential lookup to the shared-credentials
// file: every credential source that restic (minio-go) consults ahead
// of it is blanked, so keys that happen to be in Ballast's own
// environment can never be used instead of the destination's own.
func awsEnv(region string) []string {
	return []string{
		"AWS_ACCESS_KEY_ID=",
		"AWS_SECRET_ACCESS_KEY=",
		"AWS_SESSION_TOKEN=",
		"AWS_PROFILE=default",
		"MINIO_ACCESS_KEY=",
		"MINIO_SECRET_KEY=",
		"MINIO_ROOT_USER=",
		"MINIO_ROOT_PASSWORD=",
		"AWS_DEFAULT_REGION=" + region,
	}
}

// redactAWSFile registers every value in an AWS shared-credentials
// file with Redact. The file's contents never leave this function.
func (r Resolver) redactAWSFile(path string) {
	if r.Redact == nil {
		return
	}
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if _, v, ok := strings.Cut(sc.Text(), "="); ok {
			if v = strings.TrimSpace(v); len(v) >= 8 {
				r.Redact.Add(v)
			}
		}
	}
}

// sshArgs builds restic's sftp.args: the destination's own key only,
// never prompting (BatchMode), and host keys remembered in Ballast's
// own known_hosts file on first connection.
func sshArgs(key, knownHosts string) (string, error) {
	q := func(p string) (string, error) {
		if strings.ContainsAny(p, "\"\n\r") {
			return "", fmt.Errorf("path %q cannot be passed to ssh (contains a quote or newline)", p)
		}
		if strings.ContainsAny(p, " \t'\\") {
			return `"` + p + `"`, nil
		}
		return p, nil
	}
	k, err := q(key)
	if err != nil {
		return "", err
	}
	kh, err := q(knownHosts)
	if err != nil {
		return "", err
	}
	return strings.Join([]string{
		"-i", k,
		"-o", "IdentitiesOnly=yes",
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile=" + kh,
		"-o", "ConnectTimeout=20",
	}, " "), nil
}

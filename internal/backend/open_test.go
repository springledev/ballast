package backend

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/springledev/ballast/internal/runner"
	"github.com/springledev/ballast/internal/secret"
)

// writeCred writes a credential file with exactly the given
// permissions (WriteFile's perm is umask-masked, so chmod after).
func writeCred(t *testing.T, dir, name, content string, perm os.FileMode) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing credential fixture: %v", err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatalf("chmod credential fixture: %v", err)
	}
	return path
}

func findEnv(env []string, key string) (string, bool) {
	for _, e := range env {
		if k, v, ok := strings.Cut(e, "="); ok && k == key {
			return v, true
		}
	}
	return "", false
}

func TestResolverOpenS3(t *testing.T) {
	dir := t.TempDir()
	writeCred(t, dir, "ballast-restic-password.dest", "reppw\n", 0o600)
	writeCred(t, dir, "ballast-dest-keys.dest", "[default]\naws_access_key_id = AKIAEXAMPLE\naws_secret_access_key = supersecretkey1234\n", 0o600)

	redact := secret.NewSet()
	r := Resolver{
		CredDirs: []string{dir},
		Runner:   &runner.Runner{},
		Redact:   redact,
	}
	target := Target{
		Name:               "dest",
		Type:               S3,
		Settings:           Settings{Endpoint: "s3.amazonaws.com", Bucket: "bucket"},
		Location:           "s3:https://s3.amazonaws.com/bucket",
		PasswordCredential: "ballast-restic-password.dest",
		BackendCredential:  "ballast-dest-keys.dest",
	}
	repo, err := r.Open(target)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	credPath := filepath.Join(dir, "ballast-dest-keys.dest")
	if v, ok := findEnv(repo.Env, "AWS_SHARED_CREDENTIALS_FILE"); !ok || v != credPath {
		t.Errorf("AWS_SHARED_CREDENTIALS_FILE = %q, %v, want %q", v, ok, credPath)
	}
	if v, ok := findEnv(repo.Env, "AWS_ACCESS_KEY_ID"); !ok || v != "" {
		t.Errorf("AWS_ACCESS_KEY_ID = %q, %v, want blanked", v, ok)
	}
	if v, ok := findEnv(repo.Env, "AWS_PROFILE"); !ok || v != "default" {
		t.Errorf("AWS_PROFILE = %q, %v, want default", v, ok)
	}

	if got := redact.Redact("secret is supersecretkey1234 here"); strings.Contains(got, "supersecretkey1234") {
		t.Errorf("Redact() = %q, want the secret access key redacted", got)
	}
}

func TestResolverOpenR2SetsRegionAuto(t *testing.T) {
	dir := t.TempDir()
	writeCred(t, dir, "ballast-restic-password.dest", "reppw\n", 0o600)
	writeCred(t, dir, "ballast-dest-keys.dest", "[default]\naws_access_key_id = AKIAEXAMPLE\naws_secret_access_key = supersecretkey1234\n", 0o600)

	r := Resolver{CredDirs: []string{dir}, Runner: &runner.Runner{}, Redact: secret.NewSet()}
	target := Target{
		Name:               "dest",
		Type:               R2,
		Settings:           Settings{AccountID: "abc123def456", Bucket: "bucket"},
		Location:           "s3:https://abc123def456.r2.cloudflarestorage.com/bucket",
		PasswordCredential: "ballast-restic-password.dest",
		BackendCredential:  "ballast-dest-keys.dest",
	}
	repo, err := r.Open(target)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if v, ok := findEnv(repo.Env, "AWS_DEFAULT_REGION"); !ok || v != "auto" {
		t.Errorf("AWS_DEFAULT_REGION = %q, %v, want auto", v, ok)
	}
}

func TestResolverOpenRejects0644CredentialFile(t *testing.T) {
	dir := t.TempDir()
	writeCred(t, dir, "ballast-restic-password.dest", "reppw\n", 0o600)
	writeCred(t, dir, "ballast-dest-keys.dest", "[default]\naws_access_key_id = A\naws_secret_access_key = B\n", 0o644)

	r := Resolver{CredDirs: []string{dir}, Runner: &runner.Runner{}, Redact: secret.NewSet()}
	target := Target{
		Name:               "dest",
		Type:               S3,
		Settings:           Settings{Endpoint: "s3.amazonaws.com", Bucket: "bucket"},
		Location:           "s3:https://s3.amazonaws.com/bucket",
		PasswordCredential: "ballast-restic-password.dest",
		BackendCredential:  "ballast-dest-keys.dest",
	}
	if _, err := r.Open(target); err == nil {
		t.Fatal("Open with a 0644 credential file: nil error, want refusal")
	}
}

func TestResolverOpenMissingBackendCredential(t *testing.T) {
	dir := t.TempDir()
	writeCred(t, dir, "ballast-restic-password.dest", "reppw\n", 0o600)

	r := Resolver{CredDirs: []string{dir}, Runner: &runner.Runner{}, Redact: secret.NewSet()}
	target := Target{
		Name:               "dest",
		Type:               S3,
		Settings:           Settings{Endpoint: "s3.amazonaws.com", Bucket: "bucket"},
		Location:           "s3:https://s3.amazonaws.com/bucket",
		PasswordCredential: "ballast-restic-password.dest",
		// BackendCredential left empty on purpose.
	}
	if _, err := r.Open(target); err == nil || !strings.Contains(err.Error(), "no backend credential file is configured") {
		t.Errorf("error = %v, want the missing-backend-credential message", err)
	}
}

func TestResolverOpenMissingPasswordCredential(t *testing.T) {
	r := Resolver{Runner: &runner.Runner{}, Redact: secret.NewSet()}
	target := Target{Name: "dest", Type: Local, Settings: Settings{Path: "/srv"}, Location: "/srv"}
	if _, err := r.Open(target); err == nil || !strings.Contains(err.Error(), "no repository password credential is configured") {
		t.Errorf("error = %v, want the missing-password-credential message", err)
	}
}

// TestResolverOpenPasswordFileNotYetCreated: a destination added
// without setting up its repository has no password file yet; Open
// names the fix instead of surfacing a raw stat error.
func TestResolverOpenPasswordFileNotYetCreated(t *testing.T) {
	r := Resolver{CredDirs: []string{t.TempDir()}, Runner: &runner.Runner{}, Redact: secret.NewSet()}
	target := Target{Name: "dest", Type: Local, Settings: Settings{Path: "/srv"}, Location: "/srv", PasswordCredential: "ballast-restic-password.dest"}
	_, err := r.Open(target)
	if err == nil {
		t.Fatal("Open = nil error, want not-set-up error")
	}
	for _, want := range []string{"has not been set up yet", "ballast destination init dest", "ballast-restic-password.dest"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want %q", err, want)
		}
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("error = %v, want it to wrap fs.ErrNotExist", err)
	}
}

func TestResolverOpenSFTP(t *testing.T) {
	dir := t.TempDir()
	writeCred(t, dir, "ballast-restic-password.dest", "reppw\n", 0o600)
	keyPath := writeCred(t, dir, "ballast-dest-ssh-key.dest", "-----BEGIN OPENSSH PRIVATE KEY-----\nfake\n-----END OPENSSH PRIVATE KEY-----\n", 0o600)
	kh := filepath.Join(t.TempDir(), "nested", "known_hosts")

	r := Resolver{
		CredDirs:       []string{dir},
		Runner:         &runner.Runner{},
		Redact:         secret.NewSet(),
		KnownHostsFile: kh,
	}
	target := Target{
		Name:               "dest",
		Type:               SFTP,
		Settings:           Settings{Host: "h", User: "u", Path: "/srv/x"},
		Location:           "sftp://u@h:22//srv/x",
		PasswordCredential: "ballast-restic-password.dest",
		BackendCredential:  "ballast-dest-ssh-key.dest",
	}
	repo, err := r.Open(target)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if len(repo.ExtendedOptions) != 1 {
		t.Fatalf("ExtendedOptions = %v, want exactly one entry", repo.ExtendedOptions)
	}
	opt := repo.ExtendedOptions[0]
	if !strings.HasPrefix(opt, "sftp.args=") {
		t.Fatalf("ExtendedOptions[0] = %q, want the sftp.args= entry", opt)
	}
	for _, want := range []string{"-i " + keyPath, "BatchMode=yes", "StrictHostKeyChecking=accept-new", "UserKnownHostsFile=" + kh} {
		if !strings.Contains(opt, want) {
			t.Errorf("sftp.args = %q, want it to contain %q", opt, want)
		}
	}
	if info, err := os.Stat(filepath.Dir(kh)); err != nil || !info.IsDir() {
		t.Errorf("known_hosts parent dir not created: %v", err)
	}
}

// TestResolverOpenSFTPQuotesSpacedPath: a key or known_hosts path
// containing a space must be double-quoted for ssh's -o parser.
func TestResolverOpenSFTPQuotesSpacedPath(t *testing.T) {
	dir := t.TempDir()
	writeCred(t, dir, "ballast-restic-password.dest", "reppw\n", 0o600)
	// Credential file names must be plain (no separators), but the
	// credential DIRECTORY itself can contain a space.
	spacedDir := filepath.Join(t.TempDir(), "dir with space")
	if err := os.MkdirAll(spacedDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeCred(t, spacedDir, "ballast-dest-ssh-key.dest", "-----BEGIN OPENSSH PRIVATE KEY-----\nfake\n-----END OPENSSH PRIVATE KEY-----\n", 0o600)
	kh := filepath.Join(t.TempDir(), "known_hosts")

	r := Resolver{
		CredDirs:       []string{dir, spacedDir},
		Runner:         &runner.Runner{},
		Redact:         secret.NewSet(),
		KnownHostsFile: kh,
	}
	target := Target{
		Name:               "dest",
		Type:               SFTP,
		Settings:           Settings{Host: "h", User: "u", Path: "/srv/x"},
		Location:           "sftp://u@h:22//srv/x",
		PasswordCredential: "ballast-restic-password.dest",
		BackendCredential:  "ballast-dest-ssh-key.dest",
	}
	repo, err := r.Open(target)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	opt := repo.ExtendedOptions[0]
	quoted := `"` + filepath.Join(spacedDir, "ballast-dest-ssh-key.dest") + `"`
	if !strings.Contains(opt, quoted) {
		t.Errorf("sftp.args = %q, want the spaced key path double-quoted as %q", opt, quoted)
	}
}

func TestResolverOpenSFTPMissingKnownHostsFile(t *testing.T) {
	dir := t.TempDir()
	writeCred(t, dir, "ballast-restic-password.dest", "reppw\n", 0o600)
	writeCred(t, dir, "ballast-dest-ssh-key.dest", "-----BEGIN OPENSSH PRIVATE KEY-----\nfake\n-----END OPENSSH PRIVATE KEY-----\n", 0o600)

	r := Resolver{CredDirs: []string{dir}, Runner: &runner.Runner{}, Redact: secret.NewSet()} // KnownHostsFile left empty
	target := Target{
		Name:               "dest",
		Type:               SFTP,
		Settings:           Settings{Host: "h", User: "u", Path: "/srv/x"},
		Location:           "sftp://u@h:22//srv/x",
		PasswordCredential: "ballast-restic-password.dest",
		BackendCredential:  "ballast-dest-ssh-key.dest",
	}
	if _, err := r.Open(target); err == nil || !strings.Contains(err.Error(), "no known_hosts file is configured") {
		t.Errorf("error = %v, want the missing-known_hosts message", err)
	}
}

func TestResolverOpenLocalHasNoExtras(t *testing.T) {
	dir := t.TempDir()
	writeCred(t, dir, "ballast-restic-password.dest", "reppw\n", 0o600)

	r := Resolver{CredDirs: []string{dir}, Runner: &runner.Runner{}, Redact: secret.NewSet()}
	target := Target{
		Name:               "dest",
		Type:               Local,
		Settings:           Settings{Path: "/srv"},
		Location:           "/srv",
		PasswordCredential: "ballast-restic-password.dest",
	}
	repo, err := r.Open(target)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if len(repo.Env) != 0 {
		t.Errorf("Env = %v, want none for a local destination", repo.Env)
	}
	if len(repo.ExtendedOptions) != 0 {
		t.Errorf("ExtendedOptions = %v, want none for a local destination", repo.ExtendedOptions)
	}
}

// TestResolverCredDirSearchOrder: a credential file present only in the
// second CredDirs entry is still found there (runner.FindCredentialDir
// semantics), so an operator-provided $CREDENTIALS_DIRECTORY entry and
// Ballast's own saved location can coexist.
func TestResolverCredDirSearchOrder(t *testing.T) {
	firstDir := t.TempDir()
	secondDir := t.TempDir()
	writeCred(t, secondDir, "ballast-restic-password.dest", "reppw\n", 0o600)
	writeCred(t, secondDir, "ballast-dest-keys.dest", "[default]\naws_access_key_id = A\naws_secret_access_key = supersecretkey1234\n", 0o600)

	r := Resolver{CredDirs: []string{firstDir, secondDir}, Runner: &runner.Runner{}, Redact: secret.NewSet()}
	target := Target{
		Name:               "dest",
		Type:               S3,
		Settings:           Settings{Endpoint: "s3.amazonaws.com", Bucket: "bucket"},
		Location:           "s3:https://s3.amazonaws.com/bucket",
		PasswordCredential: "ballast-restic-password.dest",
		BackendCredential:  "ballast-dest-keys.dest",
	}
	repo, err := r.Open(target)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	want := filepath.Join(secondDir, "ballast-dest-keys.dest")
	if v, ok := findEnv(repo.Env, "AWS_SHARED_CREDENTIALS_FILE"); !ok || v != want {
		t.Errorf("AWS_SHARED_CREDENTIALS_FILE = %q, %v, want %q (found via CredDirs search order)", v, ok, want)
	}
}

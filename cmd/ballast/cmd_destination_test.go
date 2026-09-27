package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDestinationAddListRemove(t *testing.T) {
	withStubRestic(t)
	cliEnv(t, &spyRunner{})

	out, err := execCLI(t, "destination", "add", "local", "--location", "/srv/backups")
	if err != nil {
		t.Fatalf("destination add: %v (output %q)", err, out)
	}
	if !strings.Contains(out, "local") || !strings.Contains(out, "/srv/backups") {
		t.Errorf("output = %q, want it to name the destination and location", out)
	}

	out, err = execCLI(t, "destination", "list")
	if err != nil {
		t.Fatalf("destination list: %v", err)
	}
	if !strings.Contains(out, "local") {
		t.Errorf("list output = %q, want the destination listed", out)
	}

	if _, err := execCLI(t, "destination", "remove", "local"); err != nil {
		t.Fatalf("destination remove: %v", err)
	}
	out, err = execCLI(t, "destination", "list")
	if err != nil {
		t.Fatalf("destination list after remove: %v", err)
	}
	if strings.Contains(out, "local") {
		t.Errorf("list output after remove = %q, want it gone", out)
	}
}

func TestDestinationAddInit(t *testing.T) {
	withStubRestic(t)
	cliEnv(t, &spyRunner{})
	credDir := filepath.Join(os.Getenv("BALLAST_CONFIG_DIR"), "credentials")
	if err := os.MkdirAll(credDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(credDir, "ballast-restic-password.local"), []byte("pw\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := execCLI(t, "destination", "add", "local", "--location", "/srv/backups", "--init")
	if err != nil {
		t.Fatalf("destination add --init: %v (output %q)", err, out)
	}
	if !strings.Contains(out, "initialized") {
		t.Errorf("output = %q, want it to confirm initialization", out)
	}

	if out, err := execCLI(t, "destination", "test", "local"); err != nil {
		t.Fatalf("destination test: %v (output %q)", err, out)
	}
}

func TestDestinationAddRequiresLocation(t *testing.T) {
	cliEnv(t, &spyRunner{})
	_, err := execCLI(t, "destination", "add", "local")
	if err == nil {
		t.Fatal("destination add without --location: want an error")
	}
}

func TestDestinationRemoveInUse(t *testing.T) {
	withStubRestic(t)
	cliEnv(t, &spyRunner{})
	if _, err := execCLI(t, "destination", "add", "local", "--location", "/srv/backups"); err != nil {
		t.Fatalf("destination add: %v", err)
	}
	if _, err := execCLI(t, "service", "add", "svc", "--catalog", "folder", "--destination", "local", "--param", "path=/tmp"); err != nil {
		t.Fatalf("service add: %v", err)
	}
	_, err := execCLI(t, "destination", "remove", "local")
	if err == nil || !strings.Contains(err.Error(), "in use") {
		t.Errorf("destination remove in use = %v, want an 'in use' error", err)
	}
}

// fakeRSAPrivateKey is a syntactically valid, unencrypted private key
// (the shape RenderCredentials checks: BEGIN/END markers and a
// "PRIVATE KEY" body), used to exercise --ssh-key-file without
// depending on ssh-keygen being installed.
const fakeRSAPrivateKey = "-----BEGIN RSA PRIVATE KEY-----\n" +
	"ZmFrZS1rZXktY29udGVudHMtZm9yLXRlc3Rpbmc=\n" +
	"-----END RSA PRIVATE KEY-----"

// TestDestinationAddTyped checks that --type plus settings flags
// generate the expected restic location and a default backend
// credential file name, without any credentials given.
func TestDestinationAddTyped(t *testing.T) {
	withStubRestic(t)
	cliEnv(t, &spyRunner{})

	out, err := execCLI(t, "destination", "add", "aws",
		"--type", "s3", "--endpoint", "s3.amazonaws.com", "--bucket", "my-bucket", "--region", "us-east-1")
	if err != nil {
		t.Fatalf("destination add: %v (output %q)", err, out)
	}
	wantLoc := "s3:https://s3.amazonaws.com/my-bucket"
	if !strings.Contains(out, wantLoc) {
		t.Errorf("output = %q, want it to contain the generated location %q", out, wantLoc)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	a, err := openApp(cfg, &discardWriter{})
	if err != nil {
		t.Fatalf("openApp: %v", err)
	}
	defer a.Close()
	saved, err := a.ops.GetDestination(t.Context(), "aws")
	if err != nil {
		t.Fatalf("GetDestination: %v", err)
	}
	if string(saved.Type) != "s3" {
		t.Errorf("Type = %q, want s3", saved.Type)
	}
	if saved.Location != wantLoc {
		t.Errorf("Location = %q, want %q", saved.Location, wantLoc)
	}
	if want := "ballast-dest-keys.aws"; saved.BackendCredential != want {
		t.Errorf("BackendCredential = %q, want %q", saved.BackendCredential, want)
	}

	listOut, err := execCLI(t, "destination", "list")
	if err != nil {
		t.Fatalf("destination list: %v", err)
	}
	if !strings.Contains(listOut, "s3") {
		t.Errorf("list output = %q, want it to show the destination type", listOut)
	}
}

// TestDestinationAddAccessKeyIDStdin checks that --access-key-id reads
// the matching secret access key from stdin, writes it to a 0600
// credential file with the expected INI contents, and that the secret
// never appears in command output or leaks into the sqlite database
// file's raw bytes.
func TestDestinationAddAccessKeyIDStdin(t *testing.T) {
	withStubRestic(t)
	cliEnv(t, &spyRunner{})

	const secretKey = "SuperSecretKey1234567890"
	rootCmd.SetIn(strings.NewReader(secretKey + "\n"))
	t.Cleanup(func() { rootCmd.SetIn(nil) })

	out, err := execCLI(t, "destination", "add", "aws",
		"--type", "s3", "--endpoint", "s3.amazonaws.com", "--bucket", "my-bucket",
		"--access-key-id", "AKIAEXAMPLE")
	if err != nil {
		t.Fatalf("destination add: %v (output %q)", err, out)
	}
	if strings.Contains(out, secretKey) {
		t.Errorf("output leaked the secret access key: %q", out)
	}

	credDir := filepath.Join(os.Getenv("BALLAST_CONFIG_DIR"), "credentials")
	path := filepath.Join(credDir, "ballast-dest-keys.aws")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat credential file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("credential file permissions = %04o, want 0600", perm)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading credential file: %v", err)
	}
	want := "[default]\naws_access_key_id = AKIAEXAMPLE\naws_secret_access_key = " + secretKey + "\n"
	if string(data) != want {
		t.Errorf("credential file contents = %q, want %q", data, want)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	dbBytes, err := os.ReadFile(filepath.Join(cfg.DataDir, "ballast.db"))
	if err != nil {
		t.Fatalf("reading sqlite db file: %v", err)
	}
	if strings.Contains(string(dbBytes), secretKey) {
		t.Error("the secret access key leaked into the sqlite database file")
	}
}

// TestDestinationAddSSHKeyFile checks that --ssh-key-file reads the
// key file's contents and writes them to the backend credential file.
func TestDestinationAddSSHKeyFile(t *testing.T) {
	withStubRestic(t)
	cliEnv(t, &spyRunner{})

	keyPath := filepath.Join(t.TempDir(), "id_rsa")
	if err := os.WriteFile(keyPath, []byte(fakeRSAPrivateKey), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := execCLI(t, "destination", "add", "nas",
		"--type", "sftp", "--host", "backup.example.com", "--user", "ballast", "--path", "/srv/backups/ballast",
		"--ssh-key-file", keyPath)
	if err != nil {
		t.Fatalf("destination add: %v (output %q)", err, out)
	}

	credDir := filepath.Join(os.Getenv("BALLAST_CONFIG_DIR"), "credentials")
	path := filepath.Join(credDir, "ballast-dest-ssh-key.nas")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading credential file: %v", err)
	}
	if !strings.Contains(string(data), fakeRSAPrivateKey) {
		t.Errorf("credential file contents = %q, want it to contain the key file's contents", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("credential file permissions = %04o, want 0600", perm)
	}
}

// TestDestinationAddInvalidType checks that an unknown --type is a
// loud, immediate error.
func TestDestinationAddInvalidType(t *testing.T) {
	cliEnv(t, &spyRunner{})
	_, err := execCLI(t, "destination", "add", "bogus", "--type", "carrier-pigeon", "--path", "/tmp/x")
	if err == nil || !strings.Contains(err.Error(), "unknown destination type") {
		t.Errorf("destination add --type carrier-pigeon = %v, want an 'unknown destination type' error", err)
	}
}

// TestDestinationAddInvalidSetting checks that a setting which does
// not apply to the chosen type is a loud, immediate error rather than
// being silently ignored.
func TestDestinationAddInvalidSetting(t *testing.T) {
	cliEnv(t, &spyRunner{})
	_, err := execCLI(t, "destination", "add", "local", "--type", "local", "--path", "/srv/backups", "--bucket", "not-applicable")
	if err == nil || !strings.Contains(err.Error(), "do not apply") {
		t.Errorf("destination add with an inapplicable setting = %v, want a 'do not apply' error", err)
	}
}

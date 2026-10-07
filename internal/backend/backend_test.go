package backend

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestLocation covers Location for every Type: the exact restic
// repository string on the happy path, and every documented failure
// (unknown type, fields that don't apply, missing required fields,
// bad values).
func TestLocation(t *testing.T) {
	tests := []struct {
		name    string
		typ     Type
		s       Settings
		want    string
		wantErr string
	}{
		// --- unknown type ---
		{name: "unknown type", typ: Type("ftp"), s: Settings{}, wantErr: `unknown destination type "ftp"`},

		// --- local ---
		{name: "local happy path", typ: Local, s: Settings{Path: "/mnt/backups/ballast"}, want: "/mnt/backups/ballast"},
		{name: "local relative path rejected", typ: Local, s: Settings{Path: "backups"}, wantErr: "full folder path starting with /"},
		{name: "local field not applying", typ: Local, s: Settings{Path: "/mnt/backups", Bucket: "b"}, wantErr: "do not apply to a local destination"},
		{name: "local path with double quote", typ: Local, s: Settings{Path: `/mnt/back"ups`}, wantErr: "double quotes"},

		// --- s3 ---
		{name: "s3 happy path bare host", typ: S3, s: Settings{Endpoint: "s3.amazonaws.com", Bucket: "my-bucket"}, want: "s3:https://s3.amazonaws.com/my-bucket"},
		{name: "s3 http endpoint with path trimmed", typ: S3, s: Settings{Endpoint: "http://minio:9000", Bucket: "bucket", Path: "/sub/"}, want: "s3:http://minio:9000/bucket/sub"},
		{name: "s3 with region", typ: S3, s: Settings{Endpoint: "s3.amazonaws.com", Bucket: "bucket", Region: "us-east-1"}, want: "s3:https://s3.amazonaws.com/bucket"},
		{name: "s3 missing endpoint", typ: S3, s: Settings{Bucket: "bucket"}, wantErr: "needs the provider's endpoint"},
		{name: "s3 missing bucket", typ: S3, s: Settings{Endpoint: "s3.amazonaws.com"}, wantErr: "bucket name"},
		{name: "s3 bad region", typ: S3, s: Settings{Endpoint: "s3.amazonaws.com", Bucket: "bucket", Region: "not a region!"}, wantErr: "not a valid region name"},
		{name: "s3 endpoint with path", typ: S3, s: Settings{Endpoint: "minio.lan/extra", Bucket: "bucket"}, wantErr: "must be just a host name"},
		{name: "s3 endpoint with userinfo", typ: S3, s: Settings{Endpoint: "user:pass@minio.lan", Bucket: "bucket"}, wantErr: "must be just a host name"},
		{name: "s3 field not applying (account_id)", typ: S3, s: Settings{Endpoint: "s3.amazonaws.com", Bucket: "bucket", AccountID: "abc12345"}, wantErr: "do not apply to a s3 destination"},

		// --- b2 ---
		{name: "b2 happy path", typ: B2, s: Settings{Endpoint: "s3.us-west-004.backblazeb2.com", Bucket: "photos"}, want: "s3:https://s3.us-west-004.backblazeb2.com/photos"},
		{name: "b2 missing endpoint", typ: B2, s: Settings{Bucket: "photos"}, wantErr: "needs the bucket's S3 endpoint"},
		{name: "b2 non-b2 endpoint rejected", typ: B2, s: Settings{Endpoint: "s3.amazonaws.com", Bucket: "photos"}, wantErr: "does not look like a Backblaze S3 endpoint"},
		{name: "b2 field not applying (region)", typ: B2, s: Settings{Endpoint: "s3.us-west-004.backblazeb2.com", Bucket: "photos", Region: "us-east-1"}, wantErr: "do not apply to a b2 destination"},

		// --- r2 ---
		{name: "r2 happy path", typ: R2, s: Settings{AccountID: "abc123def456", Bucket: "bucket"}, want: "s3:https://abc123def456.r2.cloudflarestorage.com/bucket"},
		{name: "r2 missing account id", typ: R2, s: Settings{Bucket: "bucket"}, wantErr: "needs your Cloudflare account ID"},
		{name: "r2 field not applying (endpoint)", typ: R2, s: Settings{AccountID: "abc123def456", Bucket: "bucket", Endpoint: "x"}, wantErr: "do not apply to a r2 destination"},

		// --- proton drive ---
		{name: "proton default folder", typ: ProtonDrive, s: Settings{}, want: "proton-drive:/my-files/ballast"},
		{name: "proton chosen folder trimmed", typ: ProtonDrive, s: Settings{Path: "/a/b/", MaxArtifactMB: 50}, want: "proton-drive:/a/b"},
		{name: "proton relative folder", typ: ProtonDrive, s: Settings{Path: "backups"}, wantErr: "starting with /"},
		{name: "proton negative limit", typ: ProtonDrive, s: Settings{MaxArtifactMB: -1}, wantErr: "must not be negative"},
		{name: "proton field not applying", typ: ProtonDrive, s: Settings{Bucket: "b"}, wantErr: "do not apply to a proton-drive destination"},
		{name: "local rejects the size limit", typ: Local, s: Settings{Path: "/x", MaxArtifactMB: 5}, wantErr: "max_artifact_mb"},

		// --- sftp ---
		{name: "sftp default port absolute path", typ: SFTP, s: Settings{Host: "h", User: "u", Path: "/srv/x"}, want: "sftp://u@h:22//srv/x"},
		{name: "sftp custom port relative path", typ: SFTP, s: Settings{Host: "h", User: "u", Port: 2222, Path: "backups"}, want: "sftp://u@h:2222/backups"},
		{name: "sftp missing host", typ: SFTP, s: Settings{User: "u", Path: "/x"}, wantErr: "needs the server's host name"},
		{name: "sftp missing user", typ: SFTP, s: Settings{Host: "h", Path: "/x"}, wantErr: "needs the user name"},
		{name: "sftp missing path", typ: SFTP, s: Settings{Host: "h", User: "u"}, wantErr: "needs the folder on the server"},
		{name: "sftp bad port", typ: SFTP, s: Settings{Host: "h", User: "u", Path: "/x", Port: 70000}, wantErr: "not a valid port number"},
		{name: "sftp field not applying (bucket)", typ: SFTP, s: Settings{Host: "h", User: "u", Path: "/x", Bucket: "b"}, wantErr: "do not apply to a sftp destination"},

		// --- custom ---
		{name: "custom happy path", typ: Custom, s: Settings{URL: "rest:https://backup.example.com/ballast"}, want: "rest:https://backup.example.com/ballast"},
		{name: "custom missing url", typ: Custom, s: Settings{}, wantErr: "needs a restic repository string"},
		{name: "custom url with user@", typ: Custom, s: Settings{URL: "rest:https://user@backup.example.com/ballast"}, wantErr: "must not embed a user name or password"},
		{name: "custom field not applying (path)", typ: Custom, s: Settings{URL: "rest:https://x", Path: "/a"}, wantErr: "do not apply to a custom destination"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Location(tt.typ, tt.s)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("Location() = %q, nil, want error containing %q", got, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %q, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Location() error = %v, want success", err)
			}
			if got != tt.want {
				t.Errorf("Location() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRegion(t *testing.T) {
	tests := []struct {
		name string
		typ  Type
		s    Settings
		want string
	}{
		{"s3 with region", S3, Settings{Region: "eu-west-1"}, "eu-west-1"},
		{"s3 no region", S3, Settings{}, ""},
		{"b2 region derived from endpoint", B2, Settings{Endpoint: "s3.us-west-004.backblazeb2.com"}, "us-west-004"},
		{"b2 with https prefix", B2, Settings{Endpoint: "https://s3.us-west-004.backblazeb2.com"}, "us-west-004"},
		{"b2 bad endpoint", B2, Settings{Endpoint: "not-b2"}, ""},
		{"r2 always auto", R2, Settings{}, "auto"},
		{"local none", Local, Settings{}, ""},
		{"sftp none", SFTP, Settings{}, ""},
		{"custom none", Custom, Settings{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Region(tt.typ, tt.s); got != tt.want {
				t.Errorf("Region() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTypeValid(t *testing.T) {
	tests := []struct {
		typ  Type
		want bool
	}{
		{Local, true}, {S3, true}, {B2, true}, {R2, true}, {SFTP, true}, {Custom, true},
		{Type(""), false}, {Type("ftp"), false},
	}
	for _, tt := range tests {
		if got := tt.typ.Valid(); got != tt.want {
			t.Errorf("Type(%q).Valid() = %v, want %v", tt.typ, got, tt.want)
		}
	}
}

func TestCredentialKind(t *testing.T) {
	tests := []struct {
		typ  Type
		want CredentialKind
	}{
		{Local, NoCredential},
		{S3, AWSCredential},
		{B2, AWSCredential},
		{R2, AWSCredential},
		{SFTP, SSHCredential},
		{Custom, NoCredential},
	}
	for _, tt := range tests {
		if got := tt.typ.CredentialKind(); got != tt.want {
			t.Errorf("Type(%q).CredentialKind() = %q, want %q", tt.typ, got, tt.want)
		}
	}
}

func TestDefaultCredentialName(t *testing.T) {
	tests := []struct {
		typ  Type
		dest string
		want string
	}{
		{S3, "myb2", "ballast-dest-keys.myb2"},
		{B2, "myb2", "ballast-dest-keys.myb2"},
		{R2, "myr2", "ballast-dest-keys.myr2"},
		{SFTP, "myssh", "ballast-dest-ssh-key.myssh"},
		{Local, "local", ""},
		{Custom, "custom", ""},
	}
	for _, tt := range tests {
		if got := tt.typ.DefaultCredentialName(tt.dest); got != tt.want {
			t.Errorf("Type(%q).DefaultCredentialName(%q) = %q, want %q", tt.typ, tt.dest, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// RenderCredentials
// ---------------------------------------------------------------------------

func TestRenderCredentialsAWS(t *testing.T) {
	data, secrets, err := RenderCredentials(AWSCredential, Credentials{AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "supersecretkey1234"})
	if err != nil {
		t.Fatalf("RenderCredentials: %v", err)
	}
	want := "[default]\naws_access_key_id = AKIAEXAMPLE\naws_secret_access_key = supersecretkey1234\n"
	if string(data) != want {
		t.Errorf("data = %q, want %q", data, want)
	}
	if len(secrets) != 1 || secrets[0] != "supersecretkey1234" {
		t.Errorf("secrets = %v, want [supersecretkey1234]", secrets)
	}
}

func TestRenderCredentialsAWSMissingHalf(t *testing.T) {
	if _, _, err := RenderCredentials(AWSCredential, Credentials{AccessKeyID: "AKIAEXAMPLE"}); err == nil {
		t.Error("missing secret key: nil error, want failure")
	}
	if _, _, err := RenderCredentials(AWSCredential, Credentials{SecretAccessKey: "supersecretkey1234"}); err == nil {
		t.Error("missing access key id: nil error, want failure")
	}
}

func TestRenderCredentialsAWSRejectsSSHKey(t *testing.T) {
	_, _, err := RenderCredentials(AWSCredential, Credentials{SSHPrivateKey: "-----BEGIN OPENSSH PRIVATE KEY-----\nx\n-----END OPENSSH PRIVATE KEY-----"})
	if err == nil || !strings.Contains(err.Error(), "SSH key does not apply") {
		t.Errorf("error = %v, want rejection of SSH key for AWS kind", err)
	}
}

func TestRenderCredentialsAWSUnexpectedChars(t *testing.T) {
	if _, _, err := RenderCredentials(AWSCredential, Credentials{AccessKeyID: "AKIA EXAMPLE", SecretAccessKey: "key"}); err == nil {
		t.Error("space in access key id: nil error, want failure")
	}
}

func TestRenderCredentialsSSHRejectsAWSKeys(t *testing.T) {
	_, _, err := RenderCredentials(SSHCredential, Credentials{AccessKeyID: "AKIA", SecretAccessKey: "key"})
	if err == nil || !strings.Contains(err.Error(), "access key pair does not apply") {
		t.Errorf("error = %v, want rejection of an access key pair for SSH kind", err)
	}
}

func TestRenderCredentialsEmpty(t *testing.T) {
	if _, _, err := RenderCredentials(AWSCredential, Credentials{}); err != ErrNoCredentials {
		t.Errorf("RenderCredentials(empty) error = %v, want ErrNoCredentials", err)
	}
	if _, _, err := RenderCredentials(SSHCredential, Credentials{}); err != ErrNoCredentials {
		t.Errorf("RenderCredentials(empty) error = %v, want ErrNoCredentials", err)
	}
}

func TestRenderCredentialsUnknownKind(t *testing.T) {
	if _, _, err := RenderCredentials(NoCredential, Credentials{AccessKeyID: "x", SecretAccessKey: "y"}); err == nil {
		t.Error("NoCredential kind with values: nil error, want failure")
	}
}

func TestRenderCredentialsSSHPEMEncryptedRejected(t *testing.T) {
	key := "-----BEGIN RSA PRIVATE KEY-----\n" +
		"Proc-Type: 4,ENCRYPTED\n" +
		"DEK-Info: AES-128-CBC,ABCDEF\n\n" +
		"garbagebase64data\n" +
		"-----END RSA PRIVATE KEY-----"
	_, _, err := RenderCredentials(SSHCredential, Credentials{SSHPrivateKey: key})
	if err == nil || !strings.Contains(err.Error(), "protected by a passphrase") {
		t.Errorf("error = %v, want passphrase rejection", err)
	}
}

func TestRenderCredentialsSSHMalformed(t *testing.T) {
	_, _, err := RenderCredentials(SSHCredential, Credentials{SSHPrivateKey: "not a key at all"})
	if err == nil || !strings.Contains(err.Error(), "full contents") {
		t.Errorf("error = %v, want malformed-key rejection", err)
	}
}

// requireSSHKeygen skips the test when ssh-keygen is not installed.
func requireSSHKeygen(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not installed")
	}
}

// genKey generates a real ed25519 key (OpenSSH format) with the given
// passphrase ("" for none) in t.TempDir() and returns its PEM/OpenSSH
// text.
func genKey(t *testing.T, passphrase string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "id_ed25519")
	cmd := exec.Command("ssh-keygen", "-t", "ed25519", "-N", passphrase, "-f", path, "-C", "ballast-test")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, out)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading generated key: %v", err)
	}
	return string(data)
}

func TestRenderCredentialsSSHRealKeys(t *testing.T) {
	requireSSHKeygen(t)

	t.Run("unencrypted accepted", func(t *testing.T) {
		key := genKey(t, "")
		data, secrets, err := RenderCredentials(SSHCredential, Credentials{SSHPrivateKey: key})
		if err != nil {
			t.Fatalf("RenderCredentials: %v", err)
		}
		if !strings.HasSuffix(string(data), "\n") {
			t.Error("rendered key must end with a trailing newline")
		}
		if len(secrets) != 1 {
			t.Fatalf("secrets = %v, want one value", secrets)
		}
	})

	t.Run("openssh passphrase-protected rejected", func(t *testing.T) {
		key := genKey(t, "hunter2pass")
		_, _, err := RenderCredentials(SSHCredential, Credentials{SSHPrivateKey: key})
		if err == nil || !strings.Contains(err.Error(), "protected by a passphrase") {
			t.Errorf("error = %v, want passphrase rejection", err)
		}
	})

	t.Run("CRLF normalized and trailing newline added", func(t *testing.T) {
		key := genKey(t, "")
		crlf := strings.ReplaceAll(strings.TrimRight(key, "\n"), "\n", "\r\n")
		data, _, err := RenderCredentials(SSHCredential, Credentials{SSHPrivateKey: crlf})
		if err != nil {
			t.Fatalf("RenderCredentials: %v", err)
		}
		if strings.Contains(string(data), "\r") {
			t.Error("rendered key must have CRLF normalized to LF")
		}
		if !strings.HasSuffix(string(data), "-----\n") {
			t.Errorf("rendered key = %q, want it to end with the END line plus one newline", data)
		}
	})
}

func TestCredentialsEmpty(t *testing.T) {
	if !(Credentials{}).Empty() {
		t.Error("zero Credentials.Empty() = false, want true")
	}
	if (Credentials{AccessKeyID: "x"}).Empty() {
		t.Error("Credentials with AccessKeyID set: Empty() = true, want false")
	}
}

func TestProtonDriveIsNotAResticRepository(t *testing.T) {
	if ProtonDrive.IsResticRepository() || !Local.IsResticRepository() || !SFTP.IsResticRepository() {
		t.Error("only the Proton Drive copy is not a restic repository")
	}
	if ProtonDrive.CredentialKind() != NoCredential {
		t.Error("a Proton Drive copy keeps its login in the tool's own session folder, not a credential file")
	}
	_, err := Resolver{}.Open(Target{Name: "p", Type: ProtonDrive})
	if err == nil || !strings.Contains(err.Error(), "not a restic repository") {
		t.Errorf("Open on a Proton Drive destination = %v, want a clear refusal", err)
	}
}

// Package backend turns a destination's backend type and non-secret
// settings into the restic repository string, environment and backend
// options restic needs (spec §17.1: "remote destinations = restic
// backend strings, generated from the credential store").
//
// Secrets never pass through here as stored values: a destination's
// backend credentials live in one root-only credential file (an AWS
// shared-credentials file for S3/B2/R2, an SSH private key for SFTP),
// and restic is only ever given that file's *path*. RenderCredentials
// turns what the user typed into a credential form into that file's
// contents once, at save time; the caller writes it straight to disk.
//
// B2 is reached through Backblaze's S3-compatible API rather than
// restic's native b2: backend. restic's own documentation recommends
// this, and it is the only way to keep the B2 application key in a
// file: the native backend reads it from a plain environment variable
// only. Cloudflare R2 is S3-compatible too (spec §17.1).
package backend

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Type is a destination's storage backend.
type Type string

// Supported backend types.
const (
	// Local is a folder on this machine (or a mounted NAS share).
	Local Type = "local"
	// S3 is Amazon S3 or any S3-compatible service (MinIO, Wasabi, ...).
	S3 Type = "s3"
	// B2 is Backblaze B2, via its S3-compatible API.
	B2 Type = "b2"
	// R2 is Cloudflare R2, via its S3-compatible API.
	R2 Type = "r2"
	// SFTP is any server reachable over SSH.
	SFTP Type = "sftp"
	// Custom is a raw restic repository string for advanced setups
	// (rest-server, rclone:, ...). Ballast manages no backend
	// credentials for it.
	Custom Type = "custom"
)

// Types lists every supported Type, in the order a form offers them.
var Types = []Type{Local, B2, S3, R2, SFTP, Custom}

// Valid reports whether t is a supported backend type.
func (t Type) Valid() bool {
	for _, k := range Types {
		if t == k {
			return true
		}
	}
	return false
}

// CredentialKind is the shape of a backend's credential file.
type CredentialKind string

// Credential kinds.
const (
	// NoCredential means the backend needs no credential file.
	NoCredential CredentialKind = ""
	// AWSCredential is an AWS shared-credentials file
	// (AWS_SHARED_CREDENTIALS_FILE) holding an access key pair.
	AWSCredential CredentialKind = "aws"
	// SSHCredential is an SSH private key file.
	SSHCredential CredentialKind = "ssh"
)

// CredentialKind returns the credential file shape t needs.
func (t Type) CredentialKind() CredentialKind {
	switch t {
	case S3, B2, R2:
		return AWSCredential
	case SFTP:
		return SSHCredential
	}
	return NoCredential
}

// DefaultCredentialName is the credential file name used for a
// destination's backend credentials when none is configured, or ""
// when t needs none.
func (t Type) DefaultCredentialName(destination string) string {
	switch t.CredentialKind() {
	case AWSCredential:
		return "ballast-dest-keys." + destination
	case SSHCredential:
		return "ballast-dest-ssh-key." + destination
	}
	return ""
}

// Settings are a destination's non-secret backend settings. Which
// fields apply depends on the Type; the rest must be empty.
type Settings struct {
	// Path is the folder for Local, the repository path on the server
	// for SFTP (absolute, or relative to the SSH user's home), and an
	// optional folder inside the bucket for S3/B2/R2.
	Path string `json:"path,omitempty"`
	// Endpoint is the S3 API endpoint for S3 (host or URL, default
	// scheme https) and B2 (e.g. "s3.us-west-004.backblazeb2.com").
	Endpoint string `json:"endpoint,omitempty"`
	// Bucket is the bucket name for S3/B2/R2.
	Bucket string `json:"bucket,omitempty"`
	// Region is an optional S3 region; B2's is derived from Endpoint
	// and R2's is always "auto".
	Region string `json:"region,omitempty"`
	// AccountID is the Cloudflare account ID for R2.
	AccountID string `json:"account_id,omitempty"`
	// Host, Port and User locate the SFTP server (Port defaults to 22).
	Host string `json:"host,omitempty"`
	Port int    `json:"port,omitempty"`
	User string `json:"user,omitempty"`
	// URL is the raw restic repository string for Custom.
	URL string `json:"url,omitempty"`
}

var (
	bucketRE    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{1,62}$`)
	hostRE      = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$|^\[[0-9A-Fa-f:.]+\]$`)
	userRE      = regexp.MustCompile(`^[A-Za-z0-9._][A-Za-z0-9._-]{0,63}$`)
	accountRE   = regexp.MustCompile(`^[A-Za-z0-9]{8,64}$`)
	regionRE    = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
	b2RegionRE  = regexp.MustCompile(`^s3\.([a-z0-9-]+)\.backblazeb2\.com$`)
	userinfoRE  = regexp.MustCompile(`://[^/@]*@`)
	unsafePathR = regexp.MustCompile(`[\x00-\x1f"]`)
)

// Location validates s for t and returns the restic repository string.
// Every error names the offending setting in plain English.
func Location(t Type, s Settings) (string, error) {
	if !t.Valid() {
		return "", fmt.Errorf("unknown destination type %q (choose one of: %s)", t, typeList())
	}
	if err := onlyFields(t, s); err != nil {
		return "", err
	}
	if s.Path != "" && unsafePathR.MatchString(s.Path) {
		return "", fmt.Errorf("path must not contain control characters or double quotes")
	}
	switch t {
	case Local:
		if !strings.HasPrefix(s.Path, "/") {
			return "", fmt.Errorf("a local destination needs a full folder path starting with / (for example /mnt/backups/ballast)")
		}
		return s.Path, nil
	case S3:
		base, err := endpointURL(s.Endpoint)
		if err != nil {
			return "", err
		}
		if s.Region != "" && !regionRE.MatchString(s.Region) {
			return "", fmt.Errorf("region %q is not a valid region name (for example us-east-1)", s.Region)
		}
		return bucketLocation(base, s)
	case B2:
		if s.Endpoint == "" {
			return "", fmt.Errorf("a B2 destination needs the bucket's S3 endpoint, shown on the bucket's page in the Backblaze web console (for example s3.us-west-004.backblazeb2.com)")
		}
		host := strings.TrimPrefix(s.Endpoint, "https://")
		if !b2RegionRE.MatchString(host) {
			return "", fmt.Errorf("B2 endpoint %q does not look like a Backblaze S3 endpoint (for example s3.us-west-004.backblazeb2.com)", s.Endpoint)
		}
		return bucketLocation("https://"+host, s)
	case R2:
		if !accountRE.MatchString(s.AccountID) {
			return "", fmt.Errorf("an R2 destination needs your Cloudflare account ID (letters and numbers, shown on the R2 overview page)")
		}
		return bucketLocation("https://"+s.AccountID+".r2.cloudflarestorage.com", s)
	case SFTP:
		if !hostRE.MatchString(s.Host) {
			return "", fmt.Errorf("an SFTP destination needs the server's host name or IP address (got %q)", s.Host)
		}
		if !userRE.MatchString(s.User) {
			return "", fmt.Errorf("an SFTP destination needs the user name to log in as (got %q)", s.User)
		}
		port := s.Port
		if port == 0 {
			port = 22
		}
		if port < 1 || port > 65535 {
			return "", fmt.Errorf("port %d is not a valid port number (1-65535)", s.Port)
		}
		if s.Path == "" || strings.Trim(s.Path, "/") == "" {
			return "", fmt.Errorf("an SFTP destination needs the folder on the server to store backups in (for example /srv/backups/ballast)")
		}
		// restic's sftp:// form: one slash after host:port, so an
		// absolute path shows up as "//srv/...", a relative one is
		// relative to the user's home directory.
		return "sftp://" + s.User + "@" + s.Host + ":" + strconv.Itoa(port) + "/" + s.Path, nil
	case Custom:
		if s.URL == "" {
			return "", fmt.Errorf("a custom destination needs a restic repository string (for example rest:https://backup.example.com/ballast)")
		}
		if userinfoRE.MatchString(s.URL) {
			return "", fmt.Errorf("the repository string must not embed a user name or password (found user@ in the URL); put credentials in a credential file instead")
		}
		if strings.ContainsAny(s.URL, "\x00\n\r") {
			return "", fmt.Errorf("the repository string must be a single line")
		}
		return s.URL, nil
	}
	return "", fmt.Errorf("unknown destination type %q", t)
}

// Region returns the AWS region restic should sign S3 requests for, or
// "" to let restic discover it.
func Region(t Type, s Settings) string {
	switch t {
	case S3:
		return s.Region
	case B2:
		if m := b2RegionRE.FindStringSubmatch(strings.TrimPrefix(s.Endpoint, "https://")); m != nil {
			return m[1]
		}
	case R2:
		return "auto"
	}
	return ""
}

// onlyFields rejects settings that don't apply to t, so a typo or a
// half-changed form never silently stores a field Ballast ignores.
func onlyFields(t Type, s Settings) error {
	allowed := map[Type][]string{
		Local:  {"path"},
		S3:     {"path", "endpoint", "bucket", "region"},
		B2:     {"path", "endpoint", "bucket"},
		R2:     {"path", "bucket", "account_id"},
		SFTP:   {"path", "host", "port", "user"},
		Custom: {"url"},
	}[t]
	set := map[string]bool{
		"path": s.Path != "", "endpoint": s.Endpoint != "", "bucket": s.Bucket != "",
		"region": s.Region != "", "account_id": s.AccountID != "", "host": s.Host != "",
		"port": s.Port != 0, "user": s.User != "", "url": s.URL != "",
	}
	for _, a := range allowed {
		delete(set, a)
	}
	var extra []string
	for _, f := range []string{"path", "endpoint", "bucket", "region", "account_id", "host", "port", "user", "url"} {
		if set[f] {
			extra = append(extra, f)
		}
	}
	if len(extra) > 0 {
		return fmt.Errorf("setting(s) %s do not apply to a %s destination", strings.Join(extra, ", "), t)
	}
	return nil
}

// endpointURL normalizes an S3 endpoint to scheme://host[:port].
func endpointURL(endpoint string) (string, error) {
	if endpoint == "" {
		return "", fmt.Errorf("an S3 destination needs the provider's endpoint (for example s3.amazonaws.com or https://minio.lan:9000)")
	}
	raw := endpoint
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil ||
		strings.Trim(u.Path, "/") != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("endpoint %q must be just a host name, optionally with http:// or https:// and a port (for example https://minio.lan:9000)", endpoint)
	}
	return u.Scheme + "://" + u.Host, nil
}

// bucketLocation builds "s3:<base>/<bucket>[/<path>]".
func bucketLocation(base string, s Settings) (string, error) {
	if !bucketRE.MatchString(s.Bucket) {
		return "", fmt.Errorf("bucket name %q is not valid (3-63 letters, numbers, dots, dashes or underscores)", s.Bucket)
	}
	loc := "s3:" + base + "/" + s.Bucket
	if p := strings.Trim(s.Path, "/"); p != "" {
		loc += "/" + p
	}
	return loc, nil
}

func typeList() string {
	names := make([]string, len(Types))
	for i, t := range Types {
		names[i] = string(t)
	}
	return strings.Join(names, ", ")
}

// Credentials is what a user types into a destination's credential
// form. It is write-only: never stored anywhere but the credential
// file RenderCredentials produces, and never returned by any API.
type Credentials struct {
	// AccessKeyID and SecretAccessKey are the S3/B2/R2 key pair (for
	// B2: the application key ID and the application key).
	AccessKeyID     string `json:"access_key_id,omitempty"`
	SecretAccessKey string `json:"secret_access_key,omitempty"`
	// SSHPrivateKey is an SFTP destination's private key (OpenSSH
	// format, unencrypted: Ballast runs unattended).
	SSHPrivateKey string `json:"ssh_private_key,omitempty"`
}

// Empty reports whether no credential field was filled in.
func (c Credentials) Empty() bool {
	return c.AccessKeyID == "" && c.SecretAccessKey == "" && c.SSHPrivateKey == ""
}

// ErrNoCredentials is returned by RenderCredentials when c is empty.
var ErrNoCredentials = errors.New("no credentials given")

// RenderCredentials returns the credential file contents for kind
// built from c, plus the secret values the caller must register for
// redaction before anything else can print them.
func RenderCredentials(kind CredentialKind, c Credentials) (data []byte, secrets []string, err error) {
	if c.Empty() {
		return nil, nil, ErrNoCredentials
	}
	switch kind {
	case AWSCredential:
		if c.SSHPrivateKey != "" {
			return nil, nil, fmt.Errorf("an SSH key does not apply to this destination type; it uses an access key pair")
		}
		id, key := strings.TrimSpace(c.AccessKeyID), strings.TrimSpace(c.SecretAccessKey)
		if id == "" || key == "" {
			return nil, nil, fmt.Errorf("both the access key ID and the secret access key are needed")
		}
		if strings.ContainsAny(id+key, " \t\r\n=[]") {
			return nil, nil, fmt.Errorf("the access key ID or secret contains spaces or unexpected characters — check it was copied exactly")
		}
		data = []byte("[default]\naws_access_key_id = " + id + "\naws_secret_access_key = " + key + "\n")
		return data, []string{key}, nil
	case SSHCredential:
		if c.AccessKeyID != "" || c.SecretAccessKey != "" {
			return nil, nil, fmt.Errorf("an access key pair does not apply to an SFTP destination; it uses an SSH private key")
		}
		k := strings.TrimSpace(strings.ReplaceAll(c.SSHPrivateKey, "\r\n", "\n"))
		if !strings.HasPrefix(k, "-----BEGIN ") || !strings.Contains(k, "PRIVATE KEY-----") || !strings.HasSuffix(k, "-----") {
			return nil, nil, fmt.Errorf("the SSH key must be a private key file's full contents, from -----BEGIN ... PRIVATE KEY----- to the matching END line")
		}
		if strings.Contains(k, "ENCRYPTED") || opensshEncrypted(k) {
			return nil, nil, fmt.Errorf("the SSH key is protected by a passphrase; Ballast runs unattended and needs a key without one (create a dedicated key with: ssh-keygen -t ed25519 -N '' -f ballast_key)")
		}
		// ssh refuses a key file without a trailing newline.
		return []byte(k + "\n"), []string{k}, nil
	}
	return nil, nil, fmt.Errorf("this destination type does not take credentials")
}

// opensshEncrypted reports whether an "OPENSSH PRIVATE KEY" block is
// passphrase-protected. That format hides the cipher inside its
// base64 body (unlike PEM keys' "ENCRYPTED" header): the body starts
// "openssh-key-v1\x00" followed by a length-prefixed cipher name,
// which is "none" for an unprotected key.
func opensshEncrypted(key string) bool {
	const begin, end = "-----BEGIN OPENSSH PRIVATE KEY-----", "-----END OPENSSH PRIVATE KEY-----"
	body, ok := strings.CutPrefix(key, begin)
	if !ok {
		return false
	}
	body, _ = strings.CutSuffix(body, end)
	raw, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(body), ""))
	if err != nil {
		return false
	}
	const magic = "openssh-key-v1\x00"
	rest, ok := strings.CutPrefix(string(raw), magic)
	if !ok || len(rest) < 4 {
		return false
	}
	n := binary.BigEndian.Uint32([]byte(rest[:4]))
	if uint64(len(rest)-4) < uint64(n) {
		return false
	}
	return rest[4:4+n] != "none"
}

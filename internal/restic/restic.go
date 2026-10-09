// Package restic wraps the restic binary — Ballast's primary backup
// tier (spec §5.2). restic itself is never reimplemented (spec rule
// "Wrap, don't rebuild"): every operation shells out to the restic CLI
// through internal/runner, so pipefail, SIGPIPE and secret-redaction
// semantics are shared with every other external command.
//
// Output parsing (spec §8) is pinned to restic's --json format as
// produced by the restic release named in PinnedVersion, and fails
// safe: output that does not match the pinned shape is a loud
// ParseError, never a silently wrong answer. The human-readable table
// formats are deliberately NOT parsed — `snapshots --compact` has no
// Paths column, table columns drift between releases (Size appeared
// in 0.17), and `--path`/`--latest` group by path-set in ways a
// column parser cannot see.
//
// Secrets never travel in argv or in plaintext environment values:
// the repository password and every backend credential are referenced
// by *file* path (RESTIC_PASSWORD_FILE and friends, resolved via
// runner.FileEnv / $CREDENTIALS_DIRECTORY).
package restic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/springledev/keel/runner"
)

// PinnedVersion is the restic release whose --json output this
// wrapper is pinned to (spec §8). PinnedSeries is its major.minor
// series: within the series the JSON schema is expected to be stable;
// anything outside it must warn loudly at startup (spec §4) so an
// output-format drift is never mistaken for an empty repository.
const (
	PinnedVersion = "0.17.3"
	PinnedSeries  = "0.17"
)

// TestedSeries lists every restic major.minor series Ballast's parsers
// are tested against (spec §19.1): the pinned 0.17 series plus 0.18.
// A binary outside these series gets the startup warning.
var TestedSeries = []string{PinnedSeries, "0.18"}

// MinimumVersion is the oldest restic release Ballast can set up a
// new destination with: restic 0.17.0 is the first to report "no
// repository here yet" with its own exit status (10). Older releases
// exit 1 for that and for every other failure alike, so Ballast could
// not tell "needs creating" from "something is wrong" and would
// either never create the repository or risk hiding a real problem.
const MinimumVersion = "0.17.0"

// restic exit statuses beyond 0/1 (from `restic help`, EXIT STATUS).
// Encoded here so failures can be explained in plain English to
// homelab beginners (spec §10 beginner requirement).
const (
	exitRepoMissing   = 10
	exitRepoLocked    = 11
	exitWrongPassword = 12
)

// Repository is one restic repository (one backup destination: local
// disk, B2, S3/R2, SFTP — anything restic supports). All operations
// are safe for use by one engine run at a time; the underlying restic
// commands take the repository lock themselves where they need it.
type Repository struct {
	// Location is the repository URL or path (e.g. "/srv/backups",
	// "s3:https://s3.example.com/bucket", "b2:bucket:name"). It is
	// passed to restic via the RESTIC_REPOSITORY environment
	// variable, never as an argv fragment, so it cannot leak through
	// a process listing. Must not contain credentials.
	Location string

	// PasswordFile is an absolute path to a root-only (0600) file
	// holding the repository password. It becomes
	// RESTIC_PASSWORD_FILE. The password itself never appears in
	// argv, the environment, the database, or logs. Open enforces
	// the 0600 permissions, like runner.FileEnv does for backend
	// credential files.
	PasswordFile string

	// Env is extra KEY=VALUE environment for every restic invocation:
	// backend credential *file* paths (AWS_SHARED_CREDENTIALS_FILE,
	// B2_ACCOUNT_KEY_FILE, ...) and other non-secret settings.
	// Build it with runner.FileEnv so 0600 permissions are enforced.
	// Plaintext secrets must never be placed here.
	Env []string

	// ExtendedOptions are restic `-o key=value` backend options (e.g.
	// "sftp.args=-i /path/to/key") passed ahead of every subcommand.
	// Like Env they carry file paths and settings, never secrets.
	ExtendedOptions []string

	// Runner executes restic. Required.
	Runner *runner.Runner

	// ResticBin is the restic binary to invoke; empty means "restic"
	// resolved against $PATH. Tests point it at a stub.
	ResticBin string

	// RetryLock, when positive, makes every command that locks the
	// repository wait up to this long for another process's lock
	// (restic --retry-lock) instead of failing at once. Pruning holds
	// an exclusive lock, so without it a backup started by `ballast
	// run-due` while `ballast serve` prunes the same repository would
	// fail with "repository is already locked".
	RetryLock time.Duration
}

// DefaultRetryLock is how long Ballast's own repository handles wait
// for another process's lock: long enough to ride out a routine
// prune, short enough that a stale lock still surfaces as a failure
// the same night.
const DefaultRetryLock = 10 * time.Minute

// Options configures Open. See the matching Repository fields.
type Options struct {
	// Runner executes restic. Required.
	Runner *runner.Runner

	// PasswordFile is the absolute path of the repository password
	// file. Optional but required by every real destination.
	PasswordFile string

	// Env is extra non-secret environment (credential file paths).
	Env []string

	// ExtendedOptions are restic `-o key=value` backend options.
	ExtendedOptions []string

	// ResticBin overrides the restic binary (tests).
	ResticBin string

	// RetryLock is how long locking commands wait for another
	// process's lock; zero fails at once (restic's default).
	RetryLock time.Duration
}

// Open validates the destination configuration and returns a ready
// Repository. location is the restic repository URL or path.
func Open(location string, opts Options) (*Repository, error) {
	if location == "" {
		return nil, fmt.Errorf("restic: repository location must not be empty (it is where the backups are stored)")
	}
	if opts.Runner == nil {
		return nil, fmt.Errorf("restic: a command runner is required to execute the restic binary")
	}
	if opts.PasswordFile != "" {
		if !filepath.IsAbs(opts.PasswordFile) {
			return nil, fmt.Errorf("restic: repository password file %q must be an absolute path (resolve it under the credential directory, see $CREDENTIALS_DIRECTORY)", opts.PasswordFile)
		}
		if err := checkPasswordFile(opts.PasswordFile); err != nil {
			return nil, err
		}
	}
	for _, e := range opts.Env {
		if k, _, ok := strings.Cut(e, "="); !ok || k == "" {
			return nil, fmt.Errorf("restic: environment entries must be in KEY=VALUE form (got an entry without a key)")
		}
	}
	for _, o := range opts.ExtendedOptions {
		if k, _, ok := strings.Cut(o, "="); !ok || k == "" {
			return nil, fmt.Errorf("restic: backend options must be in key=value form (got %q)", o)
		}
	}
	return &Repository{
		Location:        location,
		PasswordFile:    opts.PasswordFile,
		Env:             opts.Env,
		ExtendedOptions: opts.ExtendedOptions,
		Runner:          opts.Runner,
		ResticBin:       opts.ResticBin,
		RetryLock:       opts.RetryLock,
	}, nil
}

// checkPasswordFile enforces the same rules runner.FileEnv enforces
// for credential files (spec §5.3: password files are root-only,
// 0600): the file must exist, be regular, and be unreadable by group
// and other. Contents are never read — only the path and its
// permissions leave this function.
func checkPasswordFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("restic: repository password file %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("restic: repository password file %s is not a regular file", path)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("restic: repository password file %s has permissions %04o: group and other access is not allowed; run: chmod 600 %s", path, perm, path)
	}
	return nil
}

// check guards the exported-field Repository struct against literal
// construction that bypassed Open: without this guard a zero Runner
// would panic with a nil-pointer dereference mid-operation, which is
// the wrong kind of loud for a backup tool.
func (r *Repository) check() error {
	if r.Runner == nil {
		return fmt.Errorf("restic: the repository has no command runner — build it with restic.Open, not a struct literal")
	}
	if r.Location == "" {
		return fmt.Errorf("restic: the repository has no location — build it with restic.Open, not a struct literal")
	}
	return nil
}

// bin returns the configured restic binary name.
func (r *Repository) bin() string {
	if r.ResticBin != "" {
		return r.ResticBin
	}
	return "restic"
}

// command builds one restic invocation with the repository
// environment attached. Every command goes through here so
// RESTIC_REPOSITORY and RESTIC_PASSWORD_FILE are set in exactly one
// place and can never be forgotten.
func (r *Repository) command(args ...string) runner.Command {
	env := append([]string{"RESTIC_REPOSITORY=" + r.Location}, r.Env...)
	if r.PasswordFile != "" {
		env = append(env, "RESTIC_PASSWORD_FILE="+r.PasswordFile)
	}
	full := make([]string, 0, 2*len(r.ExtendedOptions)+len(args)+2)
	for _, o := range r.ExtendedOptions {
		full = append(full, "-o", o)
	}
	if r.RetryLock > 0 && takesLock(args) {
		// Right after the subcommand: restic's global flags are
		// accepted there too, and argv[0] stays the subcommand.
		full = append(full, args[0], "--retry-lock", r.RetryLock.String())
		full = append(full, args[1:]...)
	} else {
		full = append(full, args...)
	}
	return runner.Command{Name: r.bin(), Args: full, Env: env, AbortOn: permanentBackendError}
}

// takesLock reports whether the restic invocation args locks the
// repository, so --retry-lock applies: restic rejects --retry-lock
// combined with --no-lock, and `version`/`init` never lock.
func takesLock(args []string) bool {
	if len(args) == 0 || args[0] == "version" || args[0] == "init" || args[0] == "unlock" {
		return false
	}
	return !slices.Contains(args, "--no-lock")
}

// permanentBackendRetry are storage-provider errors restic keeps
// retrying (for up to 15 minutes) although no retry can fix them: a
// rejected key or a missing bucket. restic prints each retry to
// stderr as "<op> returned error, retrying after <d>: <error>".
var permanentBackendRetry = []string{
	"signature we calculated does not match",
	"SignatureDoesNotMatch",
	"InvalidAccessKeyId",
	"Access Key Id you provided does not exist",
	"AccessDenied",
	"Access Denied",
	"NoSuchBucket",
	"bucket does not exist",
}

// permanentBackendError reports whether a restic stderr line is a
// retry of a failure that retrying cannot fix, so the command is
// stopped at once instead of making a beginner with a mistyped key
// wait 15 minutes for the same answer. It only ever shortens a
// failure: it matches retry lines only, and the command fails either
// way (spec §8: never decide success from parsed text).
func permanentBackendError(line string) bool {
	if !strings.Contains(line, "returned error, retrying after") {
		return false
	}
	for _, p := range permanentBackendRetry {
		if strings.Contains(line, p) {
			return true
		}
	}
	return false
}

// run executes one restic command to completion, capturing stdout.
// Failures are translated into plain-English errors (wrong password,
// missing repository, ...) while preserving the runner's error chain
// for programmatic inspection.
func (r *Repository) run(ctx context.Context, args ...string) (runner.Result, error) {
	if err := r.check(); err != nil {
		return runner.Result{}, err
	}
	res, err := r.Runner.Run(ctx, nil, nil, r.command(args...))
	if err != nil {
		return res, r.explain("restic "+args[0], err)
	}
	return res, nil
}

// explain maps restic's documented exit statuses onto plain-English
// causes (spec §10: beginners must not need to know what "exit 12"
// means) while keeping the original error in the chain.
func (r *Repository) explain(op string, err error) error {
	if errors.Is(err, exec.ErrNotFound) {
		return fmt.Errorf("%s: restic is not installed, or not on $PATH — install it from https://restic.readthedocs.io/en/stable/020_installation.html and try again: %w", op, err)
	}
	var ab *runner.AbortError
	if errors.As(err, &ab) {
		if hint := backendHint(ab.Stderr); hint != "" {
			return fmt.Errorf("%s for repository %s: %s: %w", op, r.Location, hint, err)
		}
		return fmt.Errorf("%s for repository %s: %w", op, r.Location, err)
	}
	var ee *runner.ExitError
	if !errors.As(err, &ee) {
		return fmt.Errorf("%s for repository %s: %w", op, r.Location, err)
	}
	switch ee.Code {
	case exitRepoMissing:
		return fmt.Errorf("%s: no backup repository exists at %s — if this destination is new, set it up with `ballast destination init <name>` (or \"Set up repository\" on the Destinations page); if it held backups before, check that its disk or bucket is still there first: %w", op, r.Location, err)
	case exitRepoLocked:
		return fmt.Errorf("%s: another restic process is using the repository at %s right now (this is safe to retry): %w", op, r.Location, err)
	case exitWrongPassword:
		return fmt.Errorf("%s: the repository password for %s is wrong (check the password file's contents; the file path is logged, its contents never are): %w", op, r.Location, err)
	default:
		if hint := backendHint(ee.Stderr); hint != "" {
			return fmt.Errorf("%s for repository %s: %s: %w", op, r.Location, hint, err)
		}
		return fmt.Errorf("%s for repository %s: %w", op, r.Location, err)
	}
}

// backendHints maps well-known storage-provider error phrases in
// restic's stderr onto a plain-English likely cause. They only add an
// explanation in front of the original error, which is always kept in
// full — no decision is ever made from this text (spec §8).
var backendHints = []struct{ needle, hint string }{
	{"InvalidAccessKeyId", "the storage provider does not recognize the access key ID — check it was copied completely"},
	{"Access Key Id you provided does not exist", "the storage provider does not recognize the access key ID — check it was copied completely"},
	{"signature we calculated does not match", "the secret access key (application key) is wrong — check it was copied completely"},
	{"SignatureDoesNotMatch", "the secret access key (application key) is wrong — check it was copied completely"},
	{"NoSuchBucket", "the bucket does not exist — check its name and the endpoint/region"},
	{"specified bucket does not exist", "the bucket does not exist — check its name and the endpoint/region"},
	{"AccessDenied", "the key was accepted but is not allowed to use this bucket — check the key's permissions"},
	{"Permission denied (publickey", "the SFTP server rejected the SSH key — add its public key to the server's authorized_keys"},
	{"Host key verification failed", "the SFTP server's host key changed since Ballast first connected — if that is expected, remove its line from Ballast's known_hosts file"},
	{"Could not resolve hostname", "the server name could not be found — check the host name"},
	{"no such host", "the server name could not be found — check the host name or endpoint"},
	{"connection refused", "nothing is accepting connections at that address — check the host, port or endpoint"},
}

// backendHint returns the first matching hint for stderr, or "".
func backendHint(stderr string) string {
	for _, h := range backendHints {
		if strings.Contains(stderr, h.needle) {
			return h.hint
		}
	}
	return ""
}

// IsRepoMissing reports whether err is restic's own "repository does
// not exist yet" signal (exit code 10) — the same code every read
// operation returns until Init has been run once at this location.
// Callers use this to make Init idempotent: check with a cheap read
// like Snapshots first, and only call Init when this reports true.
// Checking restic's own signal this way is more reliable than restic
// init's own generic "config file already exists" failure text (not
// part of the documented exit-status convention, spec §8's own
// caution against parsing anything but pinned, documented output).
func IsRepoMissing(err error) bool {
	var ee *runner.ExitError
	return errors.As(err, &ee) && ee.Code == exitRepoMissing
}

// IsBucketMissing reports whether err is an S3-compatible backend
// saying the repository's bucket does not exist (restic retries this,
// so it arrives as the early-stop *runner.AbortError). restic init
// creates a missing bucket itself, so setup treats this like
// IsRepoMissing. It is only ever used to decide to *try* init, which
// fails loudly on its own if the bucket cannot be created — never to
// decide success from parsed text (spec §8).
func IsBucketMissing(err error) bool {
	var ab *runner.AbortError
	if !errors.As(err, &ab) {
		return false
	}
	return strings.Contains(ab.Stderr, "NoSuchBucket") || strings.Contains(ab.Stderr, "bucket does not exist")
}

// IsWrongPassword reports whether err is restic's "wrong password"
// signal (exit code 12): a repository exists at this location, but
// the password file does not open it.
func IsWrongPassword(err error) bool {
	var ee *runner.ExitError
	return errors.As(err, &ee) && ee.Code == exitWrongPassword
}

// Init creates a new empty repository at the configured location.
// Calling it on an existing repository fails (restic's own behavior);
// Ballast initializes each destination once, at destination-add time.
func (r *Repository) Init(ctx context.Context) error {
	_, err := r.run(ctx, "init")
	return err
}

// BackupOptions are the per-run inputs of Backup.
type BackupOptions struct {
	// Paths are the absolute host paths and/or produced artifact
	// files to include in the snapshot (strategy Result.Paths plus
	// every strategy Artifact.Path). At least one is required.
	Paths []string

	// Tags are snapshot tags (e.g. "nightly", the service name).
	Tags []string

	// Host is the snapshot's hostname label. Empty lets restic use
	// the machine's hostname (fine for single-host destinations).
	Host string

	// Excludes are patterns for files and folders restic leaves out
	// (passed as --exclude; a pattern without a leading slash matches
	// that name anywhere under the backed-up paths).
	Excludes []string

	// Progress, when set, is called with restic's progress while the
	// upload runs (roughly once a second). It must return quickly.
	Progress func(BackupProgress)
}

// BackupProgress is one progress report from a running backup.
type BackupProgress struct {
	// PercentDone is 0..1.
	PercentDone float64
	// BytesDone and TotalBytes are what has been read and what restic
	// expects to read in all (0 while it is still counting).
	BytesDone, TotalBytes uint64
	// SecondsRemaining is restic's estimate; 0 when it has none.
	SecondsRemaining uint64
}

// BackupResult is the summary restic reports for one backup
// (spec §5.3: per-run reporting).
type BackupResult struct {
	// SnapshotID is the full ID of the new snapshot.
	SnapshotID string

	FilesNew, FilesChanged, FilesUnmodified int
	DirsNew, DirsChanged, DirsUnmodified    int

	// DataAdded is the uncompressed size of newly added data;
	// DataAddedPacked is what it compressed to in the repository.
	DataAdded       uint64
	DataAddedPacked uint64

	TotalFilesProcessed int
	TotalBytesProcessed uint64

	// Duration is restic's measured run time for the backup.
	Duration time.Duration
}

// Backup uploads the given paths as one new snapshot. Paths must be
// absolute — under systemd or cron a relative path would silently
// back up the wrong directory, which is exactly the failure a backup
// tool must not have.
func (r *Repository) Backup(ctx context.Context, opts BackupOptions) (*BackupResult, error) {
	if len(opts.Paths) == 0 {
		return nil, fmt.Errorf("restic backup: at least one path is required (nothing to back up)")
	}
	args := []string{"backup", "--json"}
	if opts.Host != "" {
		args = append(args, "--host", opts.Host)
	}
	for _, tag := range opts.Tags {
		args = append(args, "--tag", tag)
	}
	for _, ex := range opts.Excludes {
		if ex == "" || strings.ContainsAny(ex, "\n\r\x00") {
			return nil, fmt.Errorf("restic backup: skip pattern %q must be a non-empty single line", ex)
		}
		args = append(args, "--exclude", ex)
	}
	for _, p := range opts.Paths {
		if !filepath.IsAbs(p) {
			return nil, fmt.Errorf("restic backup: path %q must be absolute (e.g. /srv/data), not relative", p)
		}
		args = append(args, filepath.Clean(p))
	}

	var res runner.Result
	var err error
	var tee *progressWriter
	if opts.Progress != nil {
		if err := r.check(); err != nil {
			return nil, err
		}
		tee = &progressWriter{fn: opts.Progress}
		res, err = r.Runner.Run(ctx, nil, tee, r.command(args...))
		if err != nil {
			return nil, r.explain("restic "+args[0], err)
		}
		res.Stdout = tee.all.Bytes()
	} else {
		res, err = r.run(ctx, args...)
		if err != nil {
			return nil, err
		}
	}
	summary, err := parseBackupSummary(res.Stdout)
	if err != nil {
		return nil, fmt.Errorf("restic backup for repository %s: %w", r.Location, err)
	}
	return summary, nil
}

// SnapshotFilter narrows a snapshot listing. The zero value lists
// every snapshot.
type SnapshotFilter struct {
	// Host keeps only snapshots of this hostname label.
	Host string

	// Paths keeps only snapshots that include ALL of these absolute
	// paths (restic's --path semantics).
	Paths []string

	// Tags keeps only snapshots carrying all of these tags.
	Tags []string

	// Latest keeps only the newest n snapshots PER HOST AND PATH-SET
	// (spec §8: restic groups `--latest` by path-set — with two
	// services writing to one repository, Latest returns n snapshots
	// for each service, not n overall). Zero lists all matches.
	Latest int
}

// Snapshot is one snapshot in the repository (one completed backup).
type Snapshot struct {
	// ID is the full snapshot ID; ShortID the usual 8-hex prefix.
	ID      string `json:"id"`
	ShortID string `json:"short_id"`

	// Time is when the snapshot was taken.
	Time time.Time `json:"time"`

	// Hostname is the snapshot's host label.
	Hostname string `json:"hostname"`

	// Tags are the snapshot's tags.
	Tags []string `json:"tags,omitempty"`

	// Paths is the path-set this snapshot covers. Snapshots are
	// grouped by path-set (spec §8) — two services in one repository
	// have disjoint path-sets and never mix in one snapshot.
	Paths []string `json:"paths,omitempty"`
}

// Snapshots lists the snapshots matching the filter. The listing is
// pinned to the --json format (spec §8) and NEVER uses `--compact`:
// the compact table drops the Paths column, and path-set membership
// is exactly what Ballast needs to attribute a snapshot to a service.
func (r *Repository) Snapshots(ctx context.Context, filter SnapshotFilter) ([]Snapshot, error) {
	args := []string{"snapshots", "--json", "--no-lock"}
	if filter.Host != "" {
		args = append(args, "--host", filter.Host)
	}
	for _, p := range filter.Paths {
		if !filepath.IsAbs(p) {
			return nil, fmt.Errorf("restic snapshots: path %q must be absolute", p)
		}
		args = append(args, "--path", filepath.Clean(p))
	}
	for _, tag := range filter.Tags {
		args = append(args, "--tag", tag)
	}
	if filter.Latest < 0 {
		return nil, fmt.Errorf("restic snapshots: Latest must not be negative (got %d)", filter.Latest)
	}
	if filter.Latest > 0 {
		args = append(args, "--latest", fmt.Sprint(filter.Latest))
	}

	res, err := r.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	snaps, err := parseSnapshots(res.Stdout)
	if err != nil {
		return nil, fmt.Errorf("restic snapshots for repository %s: %w", r.Location, err)
	}
	return snaps, nil
}

// FileEntry is one entry in a snapshot listing (spec §7.3 read-back).
type FileEntry struct {
	// Path is the file's absolute path within the snapshot.
	Path string

	// Type is restic's node type: "file", "dir", "symlink", ...
	Type string

	// Size is the file size in bytes (0 for directories).
	Size int64
}

// LS lists the entries in a snapshot, optionally only under prefix
// (an absolute path inside the snapshot). snapshotID may be "latest".
// The listing is read-only: the repository is not locked, so a
// verification read-back can never block a concurrent backup.
func (r *Repository) LS(ctx context.Context, snapshotID, prefix string) ([]FileEntry, error) {
	if snapshotID == "" {
		return nil, fmt.Errorf("restic ls: snapshot ID must not be empty (use \"latest\" for the newest)")
	}
	args := []string{"ls", "--json", "--no-lock", snapshotID}
	if prefix != "" {
		if !filepath.IsAbs(prefix) {
			return nil, fmt.Errorf("restic ls: path %q must be absolute", prefix)
		}
		args = append(args, filepath.Clean(prefix))
	}

	res, err := r.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	entries, err := parseLS(res.Stdout)
	if err != nil {
		return nil, fmt.Errorf("restic ls for repository %s: %w", r.Location, err)
	}
	return entries, nil
}

// Dump reads one file's content back out of a snapshot and writes it
// to w — the read-back of spec §7.3 ("restic ls and restic dump of a
// sampled file per path-set"), and the proof that a snapshot is more
// than metadata. path is the file's absolute path within the
// snapshot; snapshotID may be "latest".
func (r *Repository) Dump(ctx context.Context, snapshotID, path string, w io.Writer) error {
	if snapshotID == "" {
		return fmt.Errorf("restic dump: snapshot ID must not be empty (use \"latest\" for the newest)")
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("restic dump: file path %q must be absolute (the path inside the snapshot)", path)
	}
	if err := r.check(); err != nil {
		return err
	}
	cmd := r.command("dump", "--no-lock", snapshotID, filepath.Clean(path))
	_, err := r.Runner.Run(ctx, nil, w, cmd)
	if err != nil {
		return r.explain("restic dump", err)
	}
	return nil
}

// RestoreOptions are the per-run inputs of Restore.
type RestoreOptions struct {
	// SnapshotID is the snapshot to restore ("latest" allowed).
	SnapshotID string

	// TargetDir is the directory to restore into. Created when
	// missing (0700: restored service data may be sensitive).
	TargetDir string

	// Paths limits the restore to these absolute paths from the
	// snapshot. Empty restores everything in the snapshot.
	Paths []string
}

// Restore writes a snapshot (or selected paths) back to disk — the
// guided restore of spec §5.3 and the heavy half of throwaway
// verification. restic recreates the original directory structure
// under TargetDir.
func (r *Repository) Restore(ctx context.Context, opts RestoreOptions) error {
	if opts.SnapshotID == "" {
		return fmt.Errorf("restic restore: snapshot ID must not be empty (use \"latest\" for the newest)")
	}
	if opts.TargetDir == "" {
		return fmt.Errorf("restic restore: a target directory is required (where the files should be written)")
	}
	if !filepath.IsAbs(opts.TargetDir) {
		return fmt.Errorf("restic restore: target directory %q must be absolute", opts.TargetDir)
	}
	if err := os.MkdirAll(opts.TargetDir, 0o700); err != nil {
		return fmt.Errorf("restic restore: creating target directory %s: %w", opts.TargetDir, err)
	}

	args := []string{"restore", "--no-lock", opts.SnapshotID, "--target", filepath.Clean(opts.TargetDir)}
	for _, p := range opts.Paths {
		if !filepath.IsAbs(p) {
			return fmt.Errorf("restic restore: path %q must be absolute", p)
		}
		args = append(args, "--include", filepath.Clean(p))
	}
	_, err := r.run(ctx, args...)
	return err
}

// RetentionPolicy is a restic forget keep policy (spec §5.2:
// "Retention policy per destination (default mirroring reference:
// 7d/4w/6m)").
type RetentionPolicy struct {
	KeepLast    int // keep the newest n snapshots
	KeepHourly  int // keep the newest per hour
	KeepDaily   int // keep the newest per day
	KeepWeekly  int // keep the newest per week
	KeepMonthly int // keep the newest per month
	KeepYearly  int // keep the newest per year
}

// DefaultRetention mirrors the reference implementation: 7 daily,
// 4 weekly, 6 monthly snapshots (spec §5.2).
func DefaultRetention() RetentionPolicy {
	return RetentionPolicy{KeepDaily: 7, KeepWeekly: 4, KeepMonthly: 6}
}

// empty reports whether the policy would keep nothing at all.
func (p RetentionPolicy) empty() bool {
	return p.KeepLast <= 0 && p.KeepHourly <= 0 && p.KeepDaily <= 0 &&
		p.KeepWeekly <= 0 && p.KeepMonthly <= 0 && p.KeepYearly <= 0
}

// ForgetResult reports what a Forget run kept and removed.
type ForgetResult struct {
	// Kept are the full IDs of snapshots kept by the policy.
	Kept []string

	// Removed are the full IDs of snapshots the policy removed.
	// The data is reclaimed by a following Prune.
	Removed []string
}

// ForgetOptions scopes a Forget run.
type ForgetOptions struct {
	// Tags limits the run to snapshots carrying ALL of these tags.
	// Ballast always passes the service's own tag, so one service's
	// policy can never remove another service's snapshots from a
	// shared repository.
	Tags []string

	// GroupBy is restic's --group-by: the snapshot attributes
	// ("host", "paths", "tags", comma-separated) that split the
	// matched snapshots into series, each kept by the policy on its
	// own. Empty means restic's default, "host,paths".
	GroupBy string
}

// Forget applies the retention policy: snapshots matching no keep
// rule are removed (metadata only — call Prune to reclaim space). A
// policy that keeps nothing is rejected: that would delete every
// snapshot, which is never what a retention rule means.
func (r *Repository) Forget(ctx context.Context, policy RetentionPolicy, opts ForgetOptions) (*ForgetResult, error) {
	if policy.empty() {
		return nil, fmt.Errorf("restic forget: retention policy must keep something (e.g. KeepDaily 7, KeepWeekly 4, KeepMonthly 6); refusing a policy that would delete every snapshot")
	}
	args := []string{"forget", "--json"}
	add := func(flag string, n int) {
		if n > 0 {
			args = append(args, flag, fmt.Sprint(n))
		}
	}
	add("--keep-last", policy.KeepLast)
	add("--keep-hourly", policy.KeepHourly)
	add("--keep-daily", policy.KeepDaily)
	add("--keep-weekly", policy.KeepWeekly)
	add("--keep-monthly", policy.KeepMonthly)
	add("--keep-yearly", policy.KeepYearly)
	if len(opts.Tags) > 0 {
		for _, tag := range opts.Tags {
			if tag == "" || strings.Contains(tag, ",") {
				return nil, fmt.Errorf("restic forget: tag %q must be non-empty and must not contain a comma", tag)
			}
		}
		// One --tag with a comma list means "all of these"; repeated
		// --tag flags would mean "any of these".
		args = append(args, "--tag", strings.Join(opts.Tags, ","))
	}
	if opts.GroupBy != "" {
		for _, g := range strings.Split(opts.GroupBy, ",") {
			if g != "host" && g != "paths" && g != "tags" {
				return nil, fmt.Errorf("restic forget: group-by %q: each part must be host, paths or tags", opts.GroupBy)
			}
		}
		args = append(args, "--group-by", opts.GroupBy)
	}

	res, err := r.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	result, err := parseForget(res.Stdout)
	if err != nil {
		return nil, fmt.Errorf("restic forget for repository %s: %w", r.Location, err)
	}
	return result, nil
}

// Prune reclaims the space of snapshots previously removed by
// Forget. It is a separate step on purpose: forget is quick metadata
// work, prune rewrites pack files and can take minutes on a large
// repository — the engine runs them as distinct steps so a prune
// timeout cannot be mistaken for a retention failure.
func (r *Repository) Prune(ctx context.Context) error {
	_, err := r.run(ctx, "prune")
	return err
}

// validSnapshotIDs rejects an empty list or an ID that is not a plain
// hex snapshot ID, so a typo can never turn into a restic flag or a
// broader command than the caller meant.
func validSnapshotIDs(op string, ids []string) error {
	if len(ids) == 0 {
		return fmt.Errorf("restic %s: no snapshot IDs given", op)
	}
	for _, id := range ids {
		if len(id) < 8 || strings.Trim(id, "0123456789abcdef") != "" {
			return fmt.Errorf("restic %s: %q is not a snapshot ID", op, id)
		}
	}
	return nil
}

// ForgetSnapshots removes exactly the snapshots with the given IDs
// (metadata only, no retention policy involved); call Prune to reclaim
// their space. Used when the operator drops a part from a destination.
func (r *Repository) ForgetSnapshots(ctx context.Context, ids []string) error {
	if err := validSnapshotIDs("forget", ids); err != nil {
		return err
	}
	_, err := r.run(ctx, append([]string{"forget"}, ids...)...)
	return err
}

// RawDataSize returns the bytes of deduplicated stored data that the
// given snapshots together reference (`restic stats --mode raw-data`).
// Comparing it for a set of snapshots with and without some of them
// shows how much space removing those would free.
func (r *Repository) RawDataSize(ctx context.Context, ids []string) (uint64, error) {
	if err := validSnapshotIDs("stats", ids); err != nil {
		return 0, err
	}
	res, err := r.run(ctx, append([]string{"stats", "--json", "--no-lock", "--mode", "raw-data"}, ids...)...)
	if err != nil {
		return 0, err
	}
	var out struct {
		TotalSize uint64 `json:"total_size"`
	}
	if err := json.Unmarshal(res.Stdout, &out); err != nil {
		return 0, &ParseError{Op: "stats", Text: snippet(res.Stdout), Err: err}
	}
	return out.TotalSize, nil
}

// Version is the restic binary's self-reported version.
type Version struct {
	// Raw is the full `restic version` line.
	Raw string

	// Number is the release version (e.g. "0.17.3").
	Number string
}

// Compatible reports whether the binary is in a tested --json output
// series (TestedSeries).
func (v Version) Compatible() bool {
	for _, s := range TestedSeries {
		if strings.HasPrefix(v.Number, s+".") || v.Number == s {
			return true
		}
	}
	return false
}

// TooOld reports whether the binary predates MinimumVersion. An
// unparseable number is not reported as too old: Warning already
// flags anything outside the tested series.
func (v Version) TooOld() bool {
	have, ok := majorMinor(v.Number)
	min, _ := majorMinor(MinimumVersion)
	if !ok {
		return false
	}
	return have[0] < min[0] || (have[0] == min[0] && have[1] < min[1])
}

// majorMinor parses the leading "MAJOR.MINOR" of a version number.
func majorMinor(num string) ([2]int, bool) {
	parts := strings.SplitN(num, ".", 3)
	if len(parts) < 2 {
		return [2]int{}, false
	}
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return [2]int{}, false
	}
	return [2]int{major, minor}, true
}

// TooOldError is the plain-English explanation for a TooOld binary.
func (v Version) TooOldError() error {
	return fmt.Errorf("restic %s is too old: Ballast needs restic %s or newer (%s recommended) — install a newer version from https://restic.readthedocs.io/en/stable/020_installation.html", v.Number, MinimumVersion, PinnedVersion)
}

// Warning returns a non-empty plain-English warning when the binary
// is outside the pinned output-format series (spec §4: "Version-pin
// check at startup; warn loudly on mismatch"). Empty means the
// version is the tested one.
func (v Version) Warning() string {
	if v.Compatible() {
		return ""
	}
	return fmt.Sprintf(
		"restic %s has not been tested with this Ballast (tested with restic %s and %s.x); if backups look wrong, install restic %s",
		v.Number, PinnedVersion, TestedSeries[1], PinnedVersion,
	)
}

// Version runs `restic version` and reports the result. Call it at
// startup and surface Warning (spec §4).
func (r *Repository) Version(ctx context.Context) (Version, error) {
	res, err := r.run(ctx, "version")
	if err != nil {
		return Version{}, err
	}
	v, err := parseVersion(res.Stdout)
	if err != nil {
		return Version{}, fmt.Errorf("restic version: %w", err)
	}
	return v, nil
}

// ReadBackResult describes a completed read-back verification.
type ReadBackResult struct {
	// Snapshot is the snapshot that was read back.
	Snapshot Snapshot

	// Samples are the files dumped back, one per path-set that
	// contained at least one regular file (spec §7.3).
	Samples []ReadBackSample
}

// ReadBackSample is one file read back out of a snapshot.
type ReadBackSample struct {
	// Path is the file's absolute path within the snapshot.
	Path string

	// Bytes is how many bytes were read back.
	Bytes int64
}

// ReadBack proves a snapshot is actually readable (spec §7.3): it
// lists the snapshot and dumps one sampled regular file per path-set
// back into memory. A path-set with no regular files (an empty
// directory) contributes a listing but no sample — listing alone
// still proves the snapshot exists and its trees parse.
//
// This is the mandatory-minimum read-back; the verification framework
// (spec §7) adds archive integrity and throwaway-container restores
// on top of these primitives.
func (r *Repository) ReadBack(ctx context.Context, snapshotID string) (*ReadBackResult, error) {
	entries, err := r.LS(ctx, snapshotID, "")
	if err != nil {
		return nil, err
	}

	// The first ls line carries the snapshot; parseLS validated it.
	// Recover it via a targeted listing of the ID for the result.
	snaps, err := r.Snapshots(ctx, SnapshotFilter{})
	if err != nil {
		return nil, err
	}
	snap, err := selectSnapshot(snaps, snapshotID)
	if err != nil {
		return nil, err
	}

	samples := make([]ReadBackSample, 0, len(snap.Paths))
	seen := make(map[string]bool, len(snap.Paths))
	for _, prefix := range snap.Paths {
		if seen[filepath.Clean(prefix)] {
			continue
		}
		seen[filepath.Clean(prefix)] = true
		file := firstFileUnder(entries, prefix)
		if file == "" {
			continue
		}
		var buf countingWriter
		if err := r.Dump(ctx, snap.ID, file, &buf); err != nil {
			return nil, fmt.Errorf("read-back of %s: %w", file, err)
		}
		samples = append(samples, ReadBackSample{Path: file, Bytes: buf.n})
	}
	return &ReadBackResult{Snapshot: snap, Samples: samples}, nil
}

// countingWriter counts bytes written to it.
type countingWriter struct{ n int64 }

func (w *countingWriter) Write(p []byte) (int, error) {
	w.n += int64(len(p))
	return len(p), nil
}

// firstFileUnder returns the first regular file at or under prefix,
// or "" when the prefix holds no regular files.
func firstFileUnder(entries []FileEntry, prefix string) string {
	prefix = filepath.Clean(prefix)
	for _, e := range entries {
		if e.Type != "file" {
			continue
		}
		if e.Path == prefix || strings.HasPrefix(e.Path, prefix+"/") {
			return e.Path
		}
	}
	return ""
}

// selectSnapshot finds snapshot id ("latest" = newest by Time) in the
// listing. restic's own "latest" is accepted verbatim by Dump/LS, but
// ReadBack needs the concrete snapshot to attribute path-sets.
func selectSnapshot(snaps []Snapshot, id string) (Snapshot, error) {
	if id == "latest" {
		var best Snapshot
		found := false
		for _, s := range snaps {
			if !found || s.Time.After(best.Time) {
				best = s
				found = true
			}
		}
		if !found {
			return Snapshot{}, fmt.Errorf("restic: repository has no snapshots to read back")
		}
		return best, nil
	}
	for _, s := range snaps {
		if s.ID == id || s.ShortID == id || strings.HasPrefix(s.ID, id) {
			return s, nil
		}
	}
	return Snapshot{}, fmt.Errorf("restic: snapshot %q not found in the repository", id)
}

// progressWriter keeps everything restic prints and reports each
// complete progress line to fn.
type progressWriter struct {
	fn   func(BackupProgress)
	all  bytes.Buffer
	line []byte
}

// Write implements io.Writer.
func (w *progressWriter) Write(p []byte) (int, error) {
	w.all.Write(p)
	w.line = append(w.line, p...)
	for {
		i := bytes.IndexByte(w.line, '\n')
		if i < 0 {
			break
		}
		line := bytes.TrimSpace(w.line[:i])
		w.line = w.line[i+1:]
		var ev wireBackupEvent
		if len(line) > 0 && json.Unmarshal(line, &ev) == nil && ev.MessageType == "status" {
			w.fn(BackupProgress{PercentDone: ev.PercentDone, BytesDone: ev.BytesDone, TotalBytes: ev.TotalBytes, SecondsRemaining: ev.SecondsLeft})
		}
	}
	return len(p), nil
}

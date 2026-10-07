// Package protondrive is Ballast's second-copy tier (spec §12, §19.8):
// it copies a run's verified backup files to Proton Drive by running
// Proton's official command-line tool (rule 4: Ballast never talks to
// Proton's API itself).
//
// The tool's login lives in a file-based session folder that only
// Ballast's user can open (mode 0700), under the credential directory.
// The folder is only ever handed to the tool by path; Ballast never
// reads it, and the Proton password and 2FA code are typed into
// Proton's own web page, never into Ballast.
package protondrive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/springledev/keel/runner"
)

// DefaultBin is the command Ballast runs when none is configured.
const DefaultBin = "proton-drive"

// TestedVersions is the CLI release series Ballast has been tested
// with; other versions work with a warning in `ballast doctor`.
const TestedVersions = "0.8"

// ErrSignedOut means Proton Drive refused the saved login (never signed
// in, expired or revoked). The fix is always "sign in again".
var ErrSignedOut = errors.New("Proton Drive is signed out: sign in to Proton Drive again")

// ErrNotInstalled means the Proton Drive command-line tool was not found.
var ErrNotInstalled = errors.New("the Proton Drive command-line tool is not installed")

// DownloadURL is where Proton publishes its command-line tool.
const DownloadURL = "https://proton.me/drive/download"

// Client runs the Proton Drive CLI for one destination. Every command
// of one Client (one session folder) is serialised: the CLI shares one
// session and cache between invocations.
type Client struct {
	// Bin is the CLI to run; empty means DefaultBin.
	Bin string
	// StateDir is this destination's session folder. It is created with
	// mode 0700 on first use.
	StateDir string
	// Runner executes the CLI. Required.
	Runner *runner.Runner
}

// StateDirFor is where destination's session folder lives under the
// credential directory.
func StateDirFor(credDir, destination string) string {
	return filepath.Join(credDir, "proton-drive", destination)
}

// Entry is one file or folder in Proton Drive.
type Entry struct {
	Name string `json:"name"`
	// Type is "folder" or "file" ("" when the tool does not say).
	Type string `json:"type"`
	Size int64  `json:"size"`
}

// IsFolder reports whether e is a folder.
func (e Entry) IsFolder() bool { return e.Type == "folder" || e.Type == "directory" }

func (c *Client) bin() string {
	if c.Bin != "" {
		return c.Bin
	}
	return DefaultBin
}

// env returns the settings that keep the CLI's login in a plain file in
// the session folder (no desktop keyring on a server) and its logging
// at errors only (the tool logs debug detail by default).
func (c *Client) env() []string {
	return []string{
		"PROTON_DRIVE_CREDENTIALS_STORE=unsafe_file",
		"PROTON_DRIVE_CACHE_DIR=" + c.StateDir,
		"PROTON_DRIVE_LOG_LEVEL=ERROR",
	}
}

// ensureState creates the session folder, owner-only.
func (c *Client) ensureState() error {
	if c.StateDir == "" {
		return fmt.Errorf("no Proton Drive session folder is configured (internal wiring error)")
	}
	if err := os.MkdirAll(c.StateDir, 0o700); err != nil {
		return fmt.Errorf("creating the Proton Drive session folder %s: %w", c.StateDir, err)
	}
	// MkdirAll leaves an existing folder's mode alone and obeys umask.
	if err := os.Chmod(c.StateDir, 0o700); err != nil {
		return fmt.Errorf("securing the Proton Drive session folder %s: %w", c.StateDir, err)
	}
	return nil
}

var (
	updateNoticeRE = regexp.MustCompile(`(?i)(newer version|new version|update available|upgrade)`)
	signedOutRE    = regexp.MustCompile(`(?i)(not (logged|signed) in|no (active )?session|session (has )?(expired|ended)|(login|sign.?in) (is )?required|please (log ?in|sign ?in)|unauthori[sz]ed|re-?authenticat|invalid (refresh )?token|\b401\b)`)
	versionRE      = regexp.MustCompile(`\d+\.\d+(\.\d+)?`)
)

// cleanOutput drops the tool's "newer version available" notice, which
// is information and never an error.
func cleanOutput(s string) string {
	var keep []string
	for _, l := range strings.Split(s, "\n") {
		if updateNoticeRE.MatchString(l) {
			continue
		}
		if t := strings.TrimSpace(l); t != "" {
			keep = append(keep, t)
		}
	}
	return strings.Join(keep, "\n")
}

// IsSignedOut reports whether output from the CLI means the login is
// gone.
func IsSignedOut(output string) bool { return signedOutRE.MatchString(cleanOutput(output)) }

// locks holds one lock per session folder: a one-slot channel so a
// waiter can give up when its context ends.
var (
	locksMu sync.Mutex
	locks   = map[string]chan struct{}{}
)

func lockFor(dir string) chan struct{} {
	locksMu.Lock()
	defer locksMu.Unlock()
	l, ok := locks[dir]
	if !ok {
		l = make(chan struct{}, 1)
		locks[dir] = l
	}
	return l
}

// Lock takes the destination's lock (one CLI user at a time) and
// returns the function that releases it.
func (c *Client) Lock(ctx context.Context) (func(), error) {
	l := lockFor(c.StateDir)
	select {
	case l <- struct{}{}:
		return func() { <-l }, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("waiting for another Proton Drive job to finish: %w", ctx.Err())
	}
}

// exec runs the CLI once. It does not take the lock; callers that do
// several steps hold Lock themselves.
func (c *Client) exec(ctx context.Context, args ...string) ([]byte, error) {
	if err := c.ensureState(); err != nil {
		return nil, err
	}
	res, err := c.Runner.Run(ctx, nil, nil, runner.Command{Name: c.bin(), Args: args, Env: c.env()})
	if err != nil {
		text := cleanOutput(string(res.Stderr) + "\n" + string(res.Stdout))
		switch {
		case isNotFound(err):
			return nil, fmt.Errorf("%w (looked for %q; install it from %s)", ErrNotInstalled, c.bin(), DownloadURL)
		case signedOutRE.MatchString(text):
			return nil, fmt.Errorf("%w: %s", ErrSignedOut, firstLine(text))
		case ctx.Err() != nil:
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("proton-drive %s: %w: %s", strings.Join(headArgs(args), " "), err, firstLine(text))
	}
	return res.Stdout, nil
}

func isNotFound(err error) bool {
	var ee *runner.ExitError
	if errors.As(err, &ee) {
		return false
	}
	return errors.Is(err, os.ErrNotExist) || strings.Contains(err.Error(), "executable file not found")
}

func headArgs(args []string) []string {
	if len(args) > 2 {
		return args[:2]
	}
	return args
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

// Version runs the CLI's --version and returns the version number, and
// whether it is in the series Ballast was tested with.
func (c *Client) Version(ctx context.Context) (version string, tested bool, err error) {
	out, err := c.exec(ctx, "--version")
	if err != nil {
		return "", false, err
	}
	v := versionRE.FindString(string(out))
	if v == "" {
		return "", false, fmt.Errorf("could not read a version number from the Proton Drive tool's answer %q", firstLine(cleanOutput(string(out))))
	}
	return v, v == TestedVersions || strings.HasPrefix(v, TestedVersions+"."), nil
}

// parseEntries reads the CLI's -j output for a folder listing: a list
// of entries, or an object holding one.
func parseEntries(out []byte) ([]Entry, error) {
	var list []Entry
	if err := json.Unmarshal(out, &list); err == nil {
		return list, nil
	}
	var wrapped struct {
		Items   []Entry `json:"items"`
		Entries []Entry `json:"entries"`
	}
	if err := json.Unmarshal(out, &wrapped); err != nil {
		return nil, fmt.Errorf("could not read the Proton Drive tool's folder listing: %w", err)
	}
	return append(wrapped.Items, wrapped.Entries...), nil
}

// List returns what is in the remote folder.
func (c *Client) List(ctx context.Context, remote string) ([]Entry, error) {
	out, err := c.exec(ctx, "filesystem", "list", "-j", remote)
	if err != nil {
		return nil, err
	}
	return parseEntries(out)
}

// Info returns one remote file's details.
func (c *Client) Info(ctx context.Context, remote string) (Entry, error) {
	out, err := c.exec(ctx, "filesystem", "info", "-j", remote)
	if err != nil {
		return Entry{}, err
	}
	var e Entry
	if err := json.Unmarshal(out, &e); err != nil {
		list, lerr := parseEntries(out)
		if lerr != nil || len(list) != 1 {
			return Entry{}, fmt.Errorf("could not read the Proton Drive tool's details for %s: %w", remote, err)
		}
		e = list[0]
	}
	return e, nil
}

// TestConnection proves the saved login works by listing folder, and
// creates the folder when it does not exist yet (the first sign-in).
func (c *Client) TestConnection(ctx context.Context, folder string) error {
	release, err := c.Lock(ctx)
	if err != nil {
		return err
	}
	defer release()
	_, err = c.List(ctx, folder)
	if err == nil || errors.Is(err, ErrSignedOut) || errors.Is(err, ErrNotInstalled) {
		return err
	}
	if _, mkErr := c.exec(ctx, "filesystem", "mkdir", folder); mkErr != nil {
		return fmt.Errorf("could not open or create the Proton Drive folder %s: %w", folder, mkErr)
	}
	if _, err := c.List(ctx, folder); err != nil {
		return fmt.Errorf("the Proton Drive folder %s was created but cannot be read: %w", folder, err)
	}
	return nil
}

// FolderFor is the Proton Drive folder a destination keeps its copies
// in: the one chosen in its settings, or the default.
func FolderFor(settingsPath string) string {
	if settingsPath != "" {
		return strings.TrimRight(settingsPath, "/")
	}
	return "/my-files/ballast"
}

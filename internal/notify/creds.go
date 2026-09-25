package notify

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/springledev/ballast/internal/runner"
)

// LoadCredentialValue reads the trimmed contents of a root-only
// credential file (systemd LoadCredential= semantics via
// $CREDENTIALS_DIRECTORY, spec §8) and returns it as the secret value
// itself — e.g. an ntfy topic URL or webhook URL.
//
// This is the counterpart to internal/runner.FileEnv for channels
// that are not subprocesses: ntfy/webhook delivery happens over
// net/http inside the Ballast process, so the channel needs the
// secret value in memory rather than a file path in an env var. It
// reuses runner.CheckCredentialFile so both paths enforce the exact
// same permission rule (spec rule 2).
//
// dir must be an absolute path to the credential directory; name must
// be a plain file name inside it (no path separators or ".."
// components). The file must be a regular file readable only by its
// owner (0600 or stricter) — anything looser is a loud error, not a
// warning.
//
// The file's contents are never included in a returned error or in
// any log line: only the directory and file name ever appear there,
// so callers can log or wrap this function's errors freely.
func LoadCredentialValue(dir, name string) (string, error) {
	if dir == "" {
		return "", fmt.Errorf("credential directory must not be empty")
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("credential directory %q must be an absolute path", dir)
	}
	if name == "" || filepath.Base(name) != name {
		return "", fmt.Errorf("credential file name %q must be a plain file name inside the credential directory", name)
	}

	path := filepath.Join(dir, name)
	if err := runner.CheckCredentialFile(path); err != nil {
		return "", fmt.Errorf("credential file %s: %w", name, err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading credential file %s: %w", name, err)
	}
	value := strings.TrimSpace(string(data))
	if value == "" {
		return "", fmt.Errorf("credential file %s is empty", name)
	}
	return value, nil
}

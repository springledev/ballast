package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// FileEnv builds KEY=<dir>/<name> environment entries pointing at
// credential files inside dir, for programs that accept credential
// *file* paths (RESTIC_PASSWORD_FILE, AWS_SHARED_CREDENTIALS_FILE,
// rclone's config file, ...).
//
// dir must be absolute — resolve it at runtime via the config package
// ($CREDENTIALS_DIRECTORY semantics, spec §8). Every file must exist,
// be a regular file, and be readable only by its owner (0600 or
// stricter); anything looser is a loud error, not a warning. File
// contents are never read: only paths ever leave this function, so the
// caller can log which directory was used without risking a leak.
func FileEnv(dir string, files map[string]string) ([]string, error) {
	if dir == "" {
		return nil, fmt.Errorf("credential directory must not be empty")
	}
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("credential directory %q must be an absolute path", dir)
	}
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	env := make([]string, 0, len(files))
	for _, k := range keys {
		name := files[k]
		if name == "" || filepath.Base(name) != name {
			return nil, fmt.Errorf("credential file name %q for %s must be a plain file name inside the credential directory", name, k)
		}
		path := filepath.Join(dir, name)
		if err := CheckCredentialFile(path); err != nil {
			return nil, fmt.Errorf("credential file for %s: %w", k, err)
		}
		env = append(env, k+"="+path)
	}
	return env, nil
}

// FindCredentialDir returns the first of dirs that holds an entry
// called name, so a credential can live either where the operator put
// it ($CREDENTIALS_DIRECTORY under systemd) or where Ballast saved it
// itself. When none holds it, the first non-empty dir is returned, so
// a "missing file" error names the operator-facing location. Empty
// dirs are skipped. Only paths are inspected, never contents.
func FindCredentialDir(name string, dirs ...string) string {
	first := ""
	for _, d := range dirs {
		if d == "" {
			continue
		}
		if first == "" {
			first = d
		}
		if _, err := os.Lstat(filepath.Join(d, name)); err == nil {
			return d
		}
	}
	return first
}

// CheckCredentialFile validates that path is a regular file readable
// only by its owner (0600 or stricter) — the same rule FileEnv
// applies to every credential path it resolves. Exported so other
// packages that read a credential file's *contents* directly (e.g.
// internal/notify, for secrets an in-process HTTP client needs in
// memory rather than as a subprocess env var pointing at a file) can
// apply the identical check without duplicating it.
//
// The file's contents are never touched here: only its mode is
// inspected, so this can be called before deciding whether it is
// even safe to read.
func CheckCredentialFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("credential file %s is not a regular file", path)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("credential file %s has permissions %04o: group and other access is not allowed; run: chmod 600 %s", path, perm, path)
	}
	return nil
}

// LoadCredentialValue reads the trimmed contents of a root-only
// credential file (systemd LoadCredential= semantics via
// $CREDENTIALS_DIRECTORY, spec §8) and returns it as the secret value
// itself — e.g. an ntfy topic URL, a webhook URL, or a catalog
// {{ .Secrets.NAME }} value.
//
// This is the counterpart to FileEnv for callers that need the secret
// value in memory rather than a file path in an env var: an in-process
// HTTP client (internal/notify) or a value about to be forwarded into
// a container's environment (internal/pipeline, for catalog secrets).
// It reuses CheckCredentialFile so every credential-reading path
// enforces the exact same permission rule (spec rule 2).
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
	if err := CheckCredentialFile(path); err != nil {
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

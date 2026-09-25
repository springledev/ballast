package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
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

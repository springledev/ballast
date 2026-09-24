package scheduler

import (
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
)

// maxTokenFileSize bounds how much of a trigger token file is read. A
// real token is 64 hex characters; anything near this size is a wrong
// path, not a token.
const maxTokenFileSize = 4096

// ReadTokenFile reads an external trigger token from path, so the
// token never has to appear on a command line where any local user
// could read it from the process list (spec rule 2). The file must be
// a regular file readable only by its owner (0600 or stricter) — a
// looser mode is a loud error, matching how Ballast treats every
// other credential file. The checks are made on the opened file
// itself, so the file cannot be swapped between checking and reading.
// Surrounding whitespace, such as a trailing newline, is ignored. The
// token itself never appears in errors.
func ReadTokenFile(path string) (string, error) {
	// O_NONBLOCK so a FIFO at path is rejected below instead of
	// hanging the open until something writes to it; it has no effect
	// on reading a regular file.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", fmt.Errorf("reading trigger token file: %w", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("reading trigger token file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("trigger token file %s is not a regular file", path)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return "", fmt.Errorf("trigger token file %s has permissions %04o: group and other access is not allowed; run: chmod 600 %s", path, perm, path)
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxTokenFileSize+1))
	if err != nil {
		return "", fmt.Errorf("reading trigger token file: %w", err)
	}
	if len(raw) > maxTokenFileSize {
		return "", fmt.Errorf("trigger token file %s is too large to be a trigger token (over %d bytes); check the path", path, maxTokenFileSize)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", fmt.Errorf("trigger token file %s is empty", path)
	}
	return token, nil
}

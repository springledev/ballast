package runner

// tailBuffer is an io.Writer that retains only the LAST cap bytes
// written to it. stderr of a chatty command (restic, pg_dump) can be
// megabytes; for diagnostics only the tail matters, so memory stays
// bounded while the error messages at the end are preserved.
type tailBuffer struct {
	buf []byte
	cap int
}

// Write appends p, dropping the oldest bytes once cap is exceeded. It
// never fails and always reports len(p) written.
func (t *tailBuffer) Write(p []byte) (int, error) {
	if len(p) >= t.cap {
		t.buf = append(t.buf[:0], p[len(p)-t.cap:]...)
		return len(p), nil
	}
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.cap {
		t.buf = append(t.buf[:0], t.buf[len(t.buf)-t.cap:]...)
	}
	return len(p), nil
}

// String returns the retained tail.
func (t *tailBuffer) String() string { return string(t.buf) }

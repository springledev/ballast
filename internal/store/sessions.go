package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// sessionTime is the fixed-width UTC format used for session times, so
// they compare correctly as text.
const sessionTime = "2006-01-02T15:04:05.000000000Z"

// CreateSession remembers a signed-in browser by the hash of its session
// token (never the token). Expired sessions are pruned in the same step.
func (s *Store) CreateSession(tokenHash string, created, expires time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("saving session: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM sessions WHERE expires_at <= ?`, created.UTC().Format(sessionTime)); err != nil {
		return fmt.Errorf("pruning old sessions: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO sessions (token_hash, created_at, expires_at) VALUES (?, ?, ?)`,
		tokenHash, created.UTC().Format(sessionTime), expires.UTC().Format(sessionTime)); err != nil {
		return fmt.Errorf("saving session: %w", err)
	}
	return tx.Commit()
}

// SessionValid reports whether a session with this token hash exists and
// has not expired at now. An expired one is deleted.
func (s *Store) SessionValid(tokenHash string, now time.Time) (bool, error) {
	var exp string
	err := s.db.QueryRow(`SELECT expires_at FROM sessions WHERE token_hash = ?`, tokenHash).Scan(&exp)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("looking up session: %w", err)
	}
	if exp > now.UTC().Format(sessionTime) {
		return true, nil
	}
	if err := s.DeleteSession(tokenHash); err != nil {
		return false, err
	}
	return false, nil
}

// DeleteSession forgets one session (sign out).
func (s *Store) DeleteSession(tokenHash string) error {
	if _, err := s.db.Exec(`DELETE FROM sessions WHERE token_hash = ?`, tokenHash); err != nil {
		return fmt.Errorf("deleting session: %w", err)
	}
	return nil
}

// DeleteAllSessions forgets every session (new access key, or "sign out
// everywhere").
func (s *Store) DeleteAllSessions() error {
	if _, err := s.db.Exec(`DELETE FROM sessions`); err != nil {
		return fmt.Errorf("deleting all sessions: %w", err)
	}
	return nil
}

// CountSessions returns how many sessions are stored, expired or not.
func (s *Store) CountSessions() (int, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&n); err != nil {
		return 0, fmt.Errorf("counting sessions: %w", err)
	}
	return n, nil
}

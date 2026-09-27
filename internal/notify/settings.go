package notify

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Settings is a service's notification configuration (spec §11):
// whether it participates in freshness sweeping at all, an optional
// override of the schedule-derived max-age, and the opt-in success
// heartbeat. Channel secrets (ntfy topic, webhook URL) are never
// stored here — they live in root-only credential files (spec rule
// 2), read at send time via LoadCredentialValue.
type Settings struct {
	Service          string `json:"service"`
	Enabled          bool   `json:"enabled"`
	MaxAgeHours      *int   `json:"max_age_hours,omitempty"`
	HeartbeatEnabled bool   `json:"heartbeat_enabled"`
}

// GetSettings returns service's notification settings, or the same
// defaults SweepFreshness's LEFT JOIN falls back to (enabled, no
// max-age override, heartbeat off) when it has no
// notification_settings row yet — so the GUI/CLI show exactly what a
// freshness sweep would actually use.
func GetSettings(db *sql.DB, service string) (Settings, error) {
	s := Settings{Service: service, Enabled: true}
	var enabled, heartbeat int
	var maxAge sql.NullInt64
	err := db.QueryRow(
		`SELECT enabled, max_age_hours, heartbeat_enabled FROM notification_settings WHERE service = ?`,
		service,
	).Scan(&enabled, &maxAge, &heartbeat)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return s, nil
	case err != nil:
		return Settings{}, fmt.Errorf("service %s: loading notification settings: %w", service, err)
	}
	s.Enabled = enabled != 0
	s.HeartbeatEnabled = heartbeat != 0
	if maxAge.Valid {
		v := int(maxAge.Int64)
		s.MaxAgeHours = &v
	}
	return s, nil
}

// ErrInvalidSettings marks a Settings value SetSettings refused
// because of what the caller asked for (not a database failure).
var ErrInvalidSettings = errors.New("invalid notification settings")

// SetSettings upserts service's notification settings. A value the
// caller got wrong wraps ErrInvalidSettings; anything else is a
// storage failure.
func SetSettings(db *sql.DB, service string, s Settings) error {
	if s.MaxAgeHours != nil && *s.MaxAgeHours <= 0 {
		return fmt.Errorf("service %s: %w: the maximum backup age must be at least 1 hour", service, ErrInvalidSettings)
	}
	var maxAge sql.NullInt64
	if s.MaxAgeHours != nil {
		maxAge = sql.NullInt64{Int64: int64(*s.MaxAgeHours), Valid: true}
	}
	_, err := db.Exec(`
		INSERT INTO notification_settings (service, enabled, max_age_hours, heartbeat_enabled, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(service) DO UPDATE SET enabled = excluded.enabled, max_age_hours = excluded.max_age_hours,
			heartbeat_enabled = excluded.heartbeat_enabled, updated_at = excluded.updated_at`,
		service, boolToInt(s.Enabled), maxAge, boolToInt(s.HeartbeatEnabled), time.Now().UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("service %s: saving notification settings: %w", service, err)
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

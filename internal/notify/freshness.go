package notify

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// FreshnessChecker checks whether a service's most recent verified
// successful backup is within the configured max-age window. It supports
// a clock argument for testing (fake clock).
//
// Note: Only SUCCESS counts as verified fresh; SUCCESS_UNVERIFIED (amber)
// does not satisfy freshness (spec §11).
type FreshnessChecker struct {
	db    *sql.DB
	clock func() time.Time // injectable for tests; defaults to time.Now
}

// NewFreshnessChecker creates a checker over the given database.
func NewFreshnessChecker(db *sql.DB) *FreshnessChecker {
	return &FreshnessChecker{
		db:    db,
		clock: time.Now,
	}
}

// SetClock injects a clock function for testing (e.g. fake-clock).
func (f *FreshnessChecker) SetClock(clock func() time.Time) {
	f.clock = clock
}

// LastSuccessfulRun returns the timestamp of the most recent verified
// successful run for the service (SUCCESS state only; amber does not
// count), or a zero time if no successful run exists.
func (f *FreshnessChecker) LastSuccessfulRun(service string) (time.Time, error) {
	var finishedAt sql.NullString
	err := f.db.QueryRow(`
		SELECT r.finished_at
		FROM runs r
		JOIN jobs j ON j.id = r.job_id
		WHERE j.service = ? AND r.state = 'SUCCESS'
		ORDER BY r.finished_at DESC
		LIMIT 1
	`, service).Scan(&finishedAt)

	if err == sql.ErrNoRows {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("querying last successful run for %s: %w", service, err)
	}

	if !finishedAt.Valid {
		return time.Time{}, nil
	}

	t, err := time.Parse(time.RFC3339Nano, finishedAt.String)
	if err != nil {
		return time.Time{}, fmt.Errorf("parsing finished_at for %s: %w", service, err)
	}
	return t, nil
}

// IsStale checks whether the service's most recent verified successful
// backup exceeds maxAge. Returns (stale, age, error). If no successful
// run exists, returns (true, neverBackedUpAge, nil) because a missing
// backup is stale.
func (f *FreshnessChecker) IsStale(service string, maxAge time.Duration) (bool, time.Duration, error) {
	lastRun, err := f.LastSuccessfulRun(service)
	if err != nil {
		return false, 0, err
	}

	if lastRun.IsZero() {
		// No successful run ever; consider it stale.
		return true, neverBackedUpAge, nil
	}

	now := f.clock().UTC()
	age := now.Sub(lastRun)
	return age > maxAge, age, nil
}

// SweepFreshness checks every known service for staleness and sends
// freshness breach notifications only when a service transitions from
// fresh to stale (deduped via freshness_alerts table).
//
// "Every known service" is the union of services from the jobs table
// (has run at least once) and the schedules table (configured to run,
// whether or not it has run yet): a service must not go unmonitored
// just because notification_settings has no row for it yet, or a
// homelab owner who never opened the notifications page would never
// hear about a service that has silently stopped backing up.
// notification_settings applies only as an override or opt-out on top
// of that set:
//   - max_age_hours, when set, overrides the schedule-derived default.
//   - enabled = 0 opts the service out of freshness sweeping entirely
//     (no check, no alert, no freshness_alerts bookkeeping).
//   - a service with no notification_settings row at all is swept
//     with default settings (enabled, no override), not skipped.
//
// A service is also excluded when its schedules row has enabled = 0
// (scheduler.SetEnabled(false)): run-due skips a disabled schedule
// (see scheduler.Coordinator.RunDue), so it is not expected to
// produce fresh backups on its own, and freshness breach alerts for
// it would just be noise. A service with no schedules row at all has
// no schedule to disable, so it is still swept.
//
// A service's max age is its notification_settings.max_age_hours
// override when set; when that column is NULL (or there is no row),
// the max age is derived from the service's schedule via
// DefaultMaxAge (spec §11) — or DefaultMaxAgeNightly when the service
// has no schedule row either.
//
// ctx bounds each notification delivery attempt (see Notifier.Send).
// Returns any errors encountered (joined), but continues sweeping all
// services — including a wrapped ErrNoChannels for a stale service
// with no registered channel, which is surfaced as a real error and,
// like any other Send failure, is never recorded as reported (see
// Notifier.Send and ErrNoChannels). Freshness sweep is typically run
// nightly and uses the wall clock to determine actual time.
func (f *FreshnessChecker) SweepFreshness(ctx context.Context, notifier *Notifier) error {
	// Fetch every known service (union of jobs and schedules), left
	// joined against notification_settings for its override/opt-out
	// and against schedules again for its cron expression. Read all
	// into a slice before querying per-service state to avoid holding
	// rows open during per-service queries (deadlock risk).
	rows, err := f.db.Query(`
		SELECT u.service, ns.enabled, ns.max_age_hours, s.cron_expr, s.enabled
		FROM (
			SELECT service FROM jobs
			UNION
			SELECT service FROM schedules
		) u
		LEFT JOIN notification_settings ns ON ns.service = u.service
		LEFT JOIN schedules s ON s.service = u.service
	`)
	if err != nil {
		return fmt.Errorf("querying services for freshness sweep: %w", err)
	}

	type serviceConfig struct {
		service         string
		enabled         sql.NullInt64
		maxAgeHours     sql.NullInt64
		cronExpr        sql.NullString
		scheduleEnabled sql.NullInt64
	}
	var configs []serviceConfig
	for rows.Next() {
		var cfg serviceConfig
		if err := rows.Scan(&cfg.service, &cfg.enabled, &cfg.maxAgeHours, &cfg.cronExpr, &cfg.scheduleEnabled); err != nil {
			rows.Close()
			return fmt.Errorf("scanning services for freshness sweep: %w", err)
		}
		configs = append(configs, cfg)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating services for freshness sweep: %w", err)
	}

	var errs []error
	now := f.clock().UTC()

	for _, cfg := range configs {
		service := cfg.service
		// enabled defaults to true (swept) both when there is no
		// notification_settings row at all and when its enabled
		// column is NULL; only an explicit 0 opts a service out.
		if cfg.enabled.Valid && cfg.enabled.Int64 == 0 {
			continue
		}
		// A disabled schedule (scheduler.SetEnabled(false)) means
		// run-due skips this service entirely (see
		// scheduler.Coordinator.RunDue / ListSchedules), so it is not
		// expected to produce fresh backups on its own; freshness
		// breach alerts for it would just be noise. A service with no
		// schedule row at all has no schedule to disable, so it is
		// still swept.
		if cfg.scheduleEnabled.Valid && cfg.scheduleEnabled.Int64 == 0 {
			continue
		}
		maxAge := effectiveMaxAge(cfg.maxAgeHours, cfg.cronExpr)

		// Check staleness
		stale, age, err := f.IsStale(service, maxAge)
		if err != nil {
			errs = append(errs, fmt.Errorf("checking freshness for %s: %w", service, err))
			continue
		}

		// Record that this service was checked, regardless of the
		// outcome below: last_checked_at reflects the sweep's last
		// look at the service, not only the last time it triggered or
		// cleared an alert.
		if err := f.touchFreshnessAlert(service, now); err != nil {
			errs = append(errs, fmt.Errorf("recording freshness check time for %s: %w", service, err))
		}

		// Get current alert status
		wasReported, err := f.getFreshnessAlertStatus(service)
		if err != nil {
			errs = append(errs, fmt.Errorf("getting freshness alert status for %s: %w", service, err))
			continue
		}

		// Transition logic:
		// - Fresh -> Fresh: do nothing
		// - Fresh -> Stale: send alert, record in freshness_alerts only if Send succeeded
		// - Stale -> Stale (already reported): do nothing
		// - Stale -> Fresh: clear freshness_alerts so next breach triggers again

		if !stale && wasReported {
			// Was stale, now fresh again: clear the alert
			if err := f.clearFreshnessAlert(service); err != nil {
				errs = append(errs, fmt.Errorf("clearing freshness alert for %s: %w", service, err))
			} else if err := notifier.Send(ctx, FormatRecovery(service)); err != nil && !errors.Is(err, ErrNoChannels) {
				errs = append(errs, fmt.Errorf("sending recovery notice for %s: %w", service, err))
			}
		} else if stale && !wasReported {
			// Newly stale: send notification; only record if Send succeeded.
			// A service with no registered channel returns ErrNoChannels
			// here, which falls into this same "don't record, retry next
			// sweep, but surface the error" path — silently marking an
			// unreachable breach as reported would hide it forever.
			event := FormatFreshnessBreached(service, maxAge, age)
			if err := notifier.Send(ctx, event); err != nil {
				errs = append(errs, fmt.Errorf("sending freshness breach notification for %s: %w", service, err))
				// Don't record alert if Send failed; retry next sweep
				continue
			}
			// Send succeeded: record this breach to avoid repeating notifications
			if err := f.recordFreshnessAlert(service, now); err != nil {
				errs = append(errs, fmt.Errorf("recording freshness alert for %s: %w", service, err))
			}
		}
	}

	if len(errs) == 0 {
		return nil
	}
	return errors.Join(errs...)
}

// effectiveMaxAge resolves the max age to check a service against:
// its explicit override when set, else the default derived from its
// schedule's cron expression, else the nightly default when the
// service has no schedule at all.
func effectiveMaxAge(maxAgeHours sql.NullInt64, cronExpr sql.NullString) time.Duration {
	if maxAgeHours.Valid {
		return time.Duration(maxAgeHours.Int64) * time.Hour
	}
	if cronExpr.Valid {
		return DefaultMaxAge(cronExpr.String)
	}
	return DefaultMaxAgeNightly
}

// touchFreshnessAlert records that a service was checked at now,
// without changing its recorded stale/reported status. It creates the
// freshness_alerts row (as fresh) on a service's first-ever check, or
// otherwise only advances last_checked_at.
func (f *FreshnessChecker) touchFreshnessAlert(service string, now time.Time) error {
	_, err := f.db.Exec(`
		INSERT INTO freshness_alerts (service, last_checked_at, is_stale, reported_at)
		VALUES (?, ?, 0, NULL)
		ON CONFLICT(service) DO UPDATE SET last_checked_at = excluded.last_checked_at
	`, service, now.Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("touching freshness check time for %s: %w", service, err)
	}
	return nil
}

// getFreshnessAlertStatus returns whether a service has a recorded
// freshness breach alert (i.e. has been reported as stale).
func (f *FreshnessChecker) getFreshnessAlertStatus(service string) (bool, error) {
	var isStale int
	err := f.db.QueryRow(
		`SELECT is_stale FROM freshness_alerts WHERE service = ?`,
		service,
	).Scan(&isStale)

	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("querying freshness alert for %s: %w", service, err)
	}
	return isStale != 0, nil
}

// recordFreshnessAlert records that a freshness breach was detected and
// reported for a service.
func (f *FreshnessChecker) recordFreshnessAlert(service string, now time.Time) error {
	_, err := f.db.Exec(`
		INSERT INTO freshness_alerts (service, last_checked_at, is_stale, reported_at)
		VALUES (?, ?, 1, ?)
		ON CONFLICT(service) DO UPDATE SET
			last_checked_at = excluded.last_checked_at,
			is_stale = 1,
			reported_at = excluded.reported_at
	`, service, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))

	if err != nil {
		return fmt.Errorf("recording freshness alert for %s: %w", service, err)
	}
	return nil
}

// clearFreshnessAlert clears the freshness breach alert for a service
// (marking it as fresh again). reported_at is reset to NULL along with
// is_stale: it recorded when the breach being cleared was reported, so
// leaving it set after the breach itself is cleared would be a stale
// timestamp for a breach that, as far as this row is concerned, no
// longer happened.
func (f *FreshnessChecker) clearFreshnessAlert(service string) error {
	_, err := f.db.Exec(`
		UPDATE freshness_alerts SET is_stale = 0, reported_at = NULL, last_checked_at = ?
		WHERE service = ?
	`, f.clock().UTC().Format(time.RFC3339Nano), service)

	if err != nil {
		return fmt.Errorf("clearing freshness alert for %s: %w", service, err)
	}
	return nil
}

// MaxAgeFor returns the max age the service's backups are held to: its
// notification_settings override when set, else the default derived
// from its schedule (spec §11), else the nightly default.
func (f *FreshnessChecker) MaxAgeFor(service string) (time.Duration, error) {
	var maxAge sql.NullInt64
	var cron sql.NullString
	err := f.db.QueryRow(`SELECT max_age_hours FROM notification_settings WHERE service = ?`, service).Scan(&maxAge)
	if err != nil && err != sql.ErrNoRows {
		return 0, fmt.Errorf("reading max age for %s: %w", service, err)
	}
	err = f.db.QueryRow(`SELECT cron_expr FROM schedules WHERE service = ?`, service).Scan(&cron)
	if err != nil && err != sql.ErrNoRows {
		return 0, fmt.Errorf("reading schedule for %s: %w", service, err)
	}
	return effectiveMaxAge(maxAge, cron), nil
}

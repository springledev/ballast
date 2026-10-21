package notify

import (
	"database/sql"
	"fmt"
	"log"
	"regexp"
	"time"
	"unicode/utf8"

	"github.com/springledev/keel/secret"
)

// Alert decision outcomes.
const (
	// OutcomeSent means the alert was delivered.
	OutcomeSent = "sent"
	// OutcomeHeld means the alert was held back (cool-down).
	OutcomeHeld = "held"
	// OutcomeFailed means delivery was attempted or needed and failed.
	OutcomeFailed = "failed"
)

// maxAlertDecisions is how many decisions are kept per service.
const maxAlertDecisions = 10

// AlertDecision is one remembered alert decision. It never holds a
// topic, URL or token (rule 2).
type AlertDecision struct {
	Service string    `json:"-"`
	At      time.Time `json:"at"`
	Type    string    `json:"type"`
	Outcome string    `json:"outcome"`
	Reason  string    `json:"reason,omitempty"`
}

var urlPattern = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://\S+`)

// maxReasonBytes is the longest reason kept before it is cut short.
const maxReasonBytes = 300

// scrubReason removes anything address-like from a reason, and
// shortens it (never in the middle of a character), before it is
// logged or stored. It is the backstop behind the app's secret
// redactor, which knows the exact tokens, topics and passwords.
func scrubReason(s string) string {
	s = urlPattern.ReplaceAllString(s, "[address hidden]")
	if len(s) > maxReasonBytes {
		cut := maxReasonBytes
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + "…"
	}
	return s
}

// RecordAlertDecision stores d and trims the service's history to the
// last few decisions.
func RecordAlertDecision(db *sql.DB, d AlertDecision) error {
	d.Reason = scrubReason(d.Reason)
	return storeDecision(db, d)
}

// storeDecision saves d as given; the caller has already cleaned the
// reason.
func storeDecision(db *sql.DB, d AlertDecision) error {
	if _, err := db.Exec(`INSERT INTO alert_decisions (service, at, type, outcome, reason) VALUES (?,?,?,?,?)`,
		d.Service, d.At.UTC().Format(time.RFC3339Nano), d.Type, d.Outcome, d.Reason); err != nil {
		return fmt.Errorf("service %s: saving alert decision: %w", d.Service, err)
	}
	if _, err := db.Exec(`DELETE FROM alert_decisions WHERE service = ? AND id NOT IN
		(SELECT id FROM alert_decisions WHERE service = ? ORDER BY id DESC LIMIT ?)`,
		d.Service, d.Service, maxAlertDecisions); err != nil {
		return fmt.Errorf("service %s: trimming alert decisions: %w", d.Service, err)
	}
	return nil
}

// ListAlertDecisions returns up to limit of service's latest alert
// decisions, newest first.
func ListAlertDecisions(db *sql.DB, service string, limit int) ([]AlertDecision, error) {
	rows, err := db.Query(`SELECT at, type, outcome, reason FROM alert_decisions WHERE service = ? ORDER BY id DESC LIMIT ?`, service, limit)
	if err != nil {
		return nil, fmt.Errorf("service %s: reading alert decisions: %w", service, err)
	}
	defer rows.Close()
	var out []AlertDecision
	for rows.Next() {
		d := AlertDecision{Service: service}
		var at string
		if err := rows.Scan(&at, &d.Type, &d.Outcome, &d.Reason); err != nil {
			return nil, fmt.Errorf("service %s: reading alert decisions: %w", service, err)
		}
		d.At, _ = time.Parse(time.RFC3339Nano, at)
		out = append(out, d)
	}
	return out, rows.Err()
}

// logDecision writes the one-line journal entry and stores the
// decision. Storage problems are logged, never returned: remembering a
// decision must not change what happens to the alert.
//
// The reason is cleaned exactly once, here: first by redact (every
// secret the app knows, including the channels' own tokens, topics and
// passwords; nil means none), then by the address backstop.
func logDecision(db *sql.DB, d AlertDecision, dest string, redact *secret.Set) {
	d.Reason = scrubReason(redact.Redact(d.Reason))
	switch d.Outcome {
	case OutcomeSent:
		log.Printf("ballast: alert: sent %s for %s to %s", d.Type, d.Service, dest)
	case OutcomeHeld:
		log.Printf("ballast: alert: held back %s for %s (%s)", d.Type, d.Service, d.Reason)
	default:
		log.Printf("ballast: alert: failed to send %s for %s: %s", d.Type, d.Service, d.Reason)
	}
	if err := storeDecision(db, d); err != nil {
		log.Printf("ballast: alert: %v", err)
	}
}

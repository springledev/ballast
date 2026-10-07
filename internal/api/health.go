package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/springledev/ballast/internal/inventory"
)

// selfBackupMetaKey records when the last verified self-backup finished
// (RFC 3339, UTC) in the meta table.
const selfBackupMetaKey = "self_backup_last_verified"

// SelfBackupMaxAge is how old the last verified self-backup may get
// before the health banner asks for a new one.
const SelfBackupMaxAge = 7 * 24 * time.Hour

// HealthItem is one line of Ballast's own health: something about the
// backup tool itself, not about a service.
type HealthItem struct {
	// Area is "self-backup", "disk", "credentials" or "runtime".
	Area   string       `json:"area"`
	Status DoctorStatus `json:"status"`
	// Message is plain English, with the fix when it is not ok.
	Message string `json:"message"`
}

// HealthReport is Ballast's own health, for the UI banner. It uses only
// local, fast checks (no network), so it is safe to load on every
// dashboard visit; `ballast doctor` does the slower checks.
type HealthReport struct {
	Items []HealthItem `json:"items"`
	// LastSelfBackupAt is when the last verified self-backup finished,
	// or nil if there never was one.
	LastSelfBackupAt *time.Time `json:"last_self_backup_at,omitempty"`
}

// Health reports the last self-backup age, the free disk space for
// verification drills and any credential files or .env sources that
// services and destinations point at but that cannot be found. Every
// check runs, so one problem never hides another.
func (o *Ops) Health(ctx context.Context) (HealthReport, error) {
	var rep HealthReport
	add := func(area string, st DoctorStatus, msg string) {
		rep.Items = append(rep.Items, HealthItem{Area: area, Status: st, Message: o.redact(msg)})
	}

	last, err := o.lastSelfBackup(ctx)
	if err != nil {
		return rep, fmt.Errorf("ballast health: %w", err)
	}
	switch {
	case last == nil:
		add("self-backup", DoctorWarn, "Ballast has never backed up its own settings. Run a self-backup (ballast self-backup <destination>) so you can rebuild Ballast if this machine is lost.")
	case time.Since(*last) > SelfBackupMaxAge:
		rep.LastSelfBackupAt = last
		add("self-backup", DoctorWarn, fmt.Sprintf("Ballast's own settings were last backed up %s ago. Run a self-backup (ballast self-backup <destination>).", humanAge(time.Since(*last))))
	default:
		rep.LastSelfBackupAt = last
		add("self-backup", DoctorOK, fmt.Sprintf("Ballast's own settings were backed up %s ago", humanAge(time.Since(*last))))
	}

	if o.InTerminal {
		add("runtime", DoctorWarn, TerminalNotice)
	}

	if o.WorkRoot != "" {
		st, detail := diskCheck(o.WorkRoot)
		add("disk", st, detail)
	}

	problems, err := o.CredentialProblems(ctx)
	if err != nil {
		return rep, fmt.Errorf("ballast health: %w", err)
	}
	for _, p := range problems {
		add("credentials", DoctorFail, p)
	}
	if len(problems) == 0 {
		add("credentials", DoctorOK, "every password, key and .env file your setup points at was found")
	}
	return rep, nil
}

// CredentialProblems lists, in plain English, every credential file and
// .env source that a destination or service refers to but that cannot
// be found or read, and every service whose destination does not exist.
// Only names are reported, never contents. It is shared by
// `ballast config check` and the health banner.
func (o *Ops) CredentialProblems(ctx context.Context) ([]string, error) {
	hasCred := func(name string) bool {
		for _, d := range o.credDirs() {
			if _, err := os.Stat(filepath.Join(d, name)); err == nil {
				return true
			}
		}
		return false
	}
	var problems []string
	dests, err := o.ListDestinations(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing destinations: %w", err)
	}
	known := map[string]bool{}
	for _, d := range dests {
		known[d.Name] = true
		for _, c := range []string{d.PasswordCredential, d.BackendCredential} {
			if c != "" && !hasCred(c) {
				problems = append(problems, fmt.Sprintf("destination %q needs the credential file %q, which was not found in %s", d.Name, c, o.credDirs()))
			}
		}
	}
	svcs, err := o.ListServices(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing services: %w", err)
	}
	for _, s := range svcs {
		if s.Destination != "" && !known[s.Destination] {
			problems = append(problems, fmt.Sprintf("service %q backs up to %q, which is not a configured destination", s.Name, s.Destination))
		}
		for name, ref := range s.Secrets {
			if path, key, isEnv := inventory.ParseEnvRef(ref); isEnv {
				keys, kerr := o.TestEnvSource(ctx, path)
				if kerr != nil {
					problems = append(problems, fmt.Sprintf("service %q: the .env file for %q cannot be read: %v", s.Name, name, kerr))
					continue
				}
				found := false
				for _, k := range keys {
					found = found || k == key
				}
				if !found {
					problems = append(problems, fmt.Sprintf("service %q: %s does not contain the key %s", s.Name, path, key))
				}
				continue
			}
			if !hasCred(ref) {
				problems = append(problems, fmt.Sprintf("service %q needs the credential file %q for %q, which was not found", s.Name, ref, name))
			}
		}
	}
	return problems, nil
}

// recordSelfBackup remembers when a verified self-backup finished.
func (o *Ops) recordSelfBackup(ctx context.Context, at time.Time) error {
	_, err := o.DB.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, selfBackupMetaKey, at.UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("recording the self-backup time: %w", err)
	}
	return nil
}

// lastSelfBackup returns when the last verified self-backup finished,
// or nil if there never was one.
func (o *Ops) lastSelfBackup(ctx context.Context) (*time.Time, error) {
	var v string
	err := o.DB.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, selfBackupMetaKey).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the self-backup time: %w", err)
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return nil, nil // an unreadable value counts as never
	}
	return &t, nil
}

// humanAge renders d as whole days, hours or minutes.
func humanAge(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%d days", int(d/(24*time.Hour)))
	case d >= 2*time.Hour:
		return fmt.Sprintf("%d hours", int(d/time.Hour))
	case d >= 2*time.Minute:
		return fmt.Sprintf("%d minutes", int(d/time.Minute))
	}
	return "moments"
}

func (h *handler) ballastHealth(w http.ResponseWriter, r *http.Request) {
	rep, err := h.ops.Health(r.Context())
	if err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

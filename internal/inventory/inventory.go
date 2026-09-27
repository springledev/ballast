// Package inventory stores which services the user has configured and
// where they back up to: the non-secret CRUD layer behind `ballast
// service`/`ballast destination` and their REST API equivalents (spec
// §14 step 11).
//
// Only non-secret fields ever reach these tables (spec rule 2:
// secrets never appear in the DB). A destination's restic repository
// password lives in a root-only credential file; this package stores
// only that file's *name* (Destination.PasswordCredential), resolved
// against the credential directory at run time by the caller — never
// the password itself, and a destination's Location must not embed
// backend credentials as URL userinfo either.
package inventory

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"time"

	"github.com/springledev/ballast/internal/restic"
	"github.com/springledev/ballast/internal/store"
)

// ErrNotFound is returned when a named destination or service does
// not exist.
var ErrNotFound = errors.New("not found")

// ErrInUse is returned by DeleteDestination when a service still
// references it.
var ErrInUse = errors.New("still in use")

// nameRE is the allowed shape for a destination or service name:
// names end up in file names (run locks, work directories, credential
// file defaults), so they are restricted to a safe, predictable
// alphabet.
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// ValidateName reports whether name is safe to use as a destination
// or service name.
func ValidateName(name string) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("%q can't be used as a name: use lowercase letters, numbers, dashes and underscores, starting with a letter or number (up to 63 characters)", name)
	}
	return nil
}

// Destination is one configured restic backup destination.
//
// JSON tags make this struct directly usable as an API response/
// request shape (spec §14 step 11 D3: snake_case, stable field
// names) without a separate DTO layer.
type Destination struct {
	Name     string `json:"name"`
	Location string `json:"location"` // restic repository URL/path; no embedded credentials
	// PasswordCredential is the file NAME (not path) inside the
	// credential directory holding the repository password.
	PasswordCredential string `json:"password_credential"`
	KeepDaily          int    `json:"keep_daily"`
	KeepWeekly         int    `json:"keep_weekly"`
	KeepMonthly        int    `json:"keep_monthly"`
}

// Retention returns d's restic retention policy.
func (d Destination) Retention() restic.RetentionPolicy {
	return restic.RetentionPolicy{KeepDaily: d.KeepDaily, KeepWeekly: d.KeepWeekly, KeepMonthly: d.KeepMonthly}
}

// locationUserinfoRE matches a URL scheme followed by embedded
// userinfo ("scheme://user:pass@..."), the shape a credential leaked
// into Location would take.
var locationUserinfoRE = regexp.MustCompile(`://[^/@]*@`)

// PutDestination inserts or updates a destination. name is validated;
// Location must not embed backend credentials; PasswordCredential
// defaults to "ballast-restic-password.<name>" when empty; the
// retention policy must keep at least one snapshot somewhere (all-zero
// would mean every backup is forgotten, which is never what a
// retention setting means — restic.Repository.Forget rejects the same
// shape).
func PutDestination(db *sql.DB, d Destination) error {
	if err := ValidateName(d.Name); err != nil {
		return fmt.Errorf("destination: %w", err)
	}
	if d.Location == "" {
		return fmt.Errorf("destination %s: location must not be empty", d.Name)
	}
	if locationUserinfoRE.MatchString(d.Location) {
		return fmt.Errorf("destination %s: location must not embed credentials (found user:pass@ in the URL); put backend credentials in a credential file instead", d.Name)
	}
	if d.PasswordCredential == "" {
		d.PasswordCredential = "ballast-restic-password." + d.Name
	}
	if filepath.Base(d.PasswordCredential) != d.PasswordCredential {
		return fmt.Errorf("destination %s: password credential file name %q must be a plain file name (no path separators)", d.Name, d.PasswordCredential)
	}
	if d.KeepDaily < 0 || d.KeepWeekly < 0 || d.KeepMonthly < 0 {
		return fmt.Errorf("destination %s: retention counts must not be negative", d.Name)
	}
	if d.KeepDaily == 0 && d.KeepWeekly == 0 && d.KeepMonthly == 0 {
		return fmt.Errorf("destination %s: retention policy must keep at least one snapshot (keep_daily, keep_weekly and keep_monthly are all zero)", d.Name)
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := db.Exec(`
		INSERT INTO destinations (name, location, password_credential, keep_daily, keep_weekly, keep_monthly, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			location = excluded.location,
			password_credential = excluded.password_credential,
			keep_daily = excluded.keep_daily,
			keep_weekly = excluded.keep_weekly,
			keep_monthly = excluded.keep_monthly,
			updated_at = excluded.updated_at`,
		d.Name, d.Location, d.PasswordCredential, d.KeepDaily, d.KeepWeekly, d.KeepMonthly, now, now,
	)
	if err != nil {
		return fmt.Errorf("saving destination %s: %w", d.Name, err)
	}
	return nil
}

// GetDestination returns the named destination, or ErrNotFound.
func GetDestination(db *sql.DB, name string) (Destination, error) {
	var d Destination
	err := db.QueryRow(`
		SELECT name, location, password_credential, keep_daily, keep_weekly, keep_monthly
		FROM destinations WHERE name = ?`, name,
	).Scan(&d.Name, &d.Location, &d.PasswordCredential, &d.KeepDaily, &d.KeepWeekly, &d.KeepMonthly)
	if errors.Is(err, sql.ErrNoRows) {
		return Destination{}, fmt.Errorf("destination %s: %w", name, ErrNotFound)
	}
	if err != nil {
		return Destination{}, fmt.Errorf("getting destination %s: %w", name, err)
	}
	return d, nil
}

// ListDestinations returns every configured destination, ordered by name.
func ListDestinations(db *sql.DB) ([]Destination, error) {
	rows, err := db.Query(`
		SELECT name, location, password_credential, keep_daily, keep_weekly, keep_monthly
		FROM destinations ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("listing destinations: %w", err)
	}
	defer rows.Close()

	// A non-nil empty slice, not nil: this list is a JSON API response
	// body (spec §9), and json.Marshal renders a nil slice as `null`,
	// not `[]` — a needless null the web UI would have to guard against
	// on every "no destinations yet" case.
	out := []Destination{}
	for rows.Next() {
		var d Destination
		if err := rows.Scan(&d.Name, &d.Location, &d.PasswordCredential, &d.KeepDaily, &d.KeepWeekly, &d.KeepMonthly); err != nil {
			return nil, fmt.Errorf("listing destinations: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing destinations: %w", err)
	}
	return out, nil
}

// DeleteDestination removes a destination. It fails with ErrInUse if
// any service still references it, and with ErrNotFound if it does
// not exist. The in-use check and the delete run in one transaction
// (store.Open's _txlock=immediate makes BEGIN itself take the write
// lock) so a service cannot be added referencing this destination
// between the check and the delete; SQLite's own foreign-key
// enforcement (also on by default) is a second line of defence for
// the reverse race.
func DeleteDestination(db *sql.DB, name string) (err error) {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("deleting destination %s: %w", name, err)
	}
	defer func() { err = store.RollbackOnError(tx, err) }()

	var count int
	if err = tx.QueryRow(`SELECT COUNT(*) FROM services WHERE destination = ?`, name).Scan(&count); err != nil {
		return fmt.Errorf("checking destination %s usage: %w", name, err)
	}
	if count > 0 {
		return fmt.Errorf("destination %s: %w (used by %d service(s))", name, ErrInUse, count)
	}
	res, err := tx.Exec(`DELETE FROM destinations WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("deleting destination %s: %w", name, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("destination %s: %w", name, ErrNotFound)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("deleting destination %s: %w", name, err)
	}
	return nil
}

// Service is one configured backup service: a catalog entry bound to
// a destination and its resolved template parameters.
type Service struct {
	Name         string `json:"name"`
	CatalogEntry string `json:"catalog_entry"` // catalog.Entry.Service, e.g. "postgres"
	Destination  string `json:"destination"`
	// Params fills {{ .Params.* }} placeholders in the catalog entry.
	Params map[string]string `json:"params,omitempty"`
	// Containers fills {{ .Services.* }} placeholders (detected
	// container names).
	Containers map[string]string `json:"containers,omitempty"`
	// Secrets maps a catalog {{ .Secrets.NAME }} name to the *name*
	// (never the value) of the root-only credential file holding it,
	// e.g. {"db_password": "ballast-secret.myservice.db_password"} —
	// the same non-secret-file-name pattern Destination.PasswordCredential
	// uses. `ballast secret set` fills in a default name
	// ("ballast-secret.<service>.<name>") when empty.
	Secrets map[string]string `json:"secrets,omitempty"`
}

// PutService inserts or updates a service. name is validated; the
// destination must already exist. Catalog-entry existence and
// required-parameter checks are NOT done here (this package has no
// catalog dependency) — the API layer validates those against the
// loaded catalog registry.
func PutService(db *sql.DB, s Service) error {
	if err := ValidateName(s.Name); err != nil {
		return fmt.Errorf("service: %w", err)
	}
	if s.CatalogEntry == "" {
		return fmt.Errorf("service %s: catalog entry must not be empty", s.Name)
	}
	if s.Destination == "" {
		return fmt.Errorf("service %s: destination must not be empty", s.Name)
	}
	if _, err := GetDestination(db, s.Destination); err != nil {
		return fmt.Errorf("service %s: %w", s.Name, err)
	}
	paramsJSON, err := marshalMap(s.Params)
	if err != nil {
		return fmt.Errorf("service %s: encoding params: %w", s.Name, err)
	}
	containersJSON, err := marshalMap(s.Containers)
	if err != nil {
		return fmt.Errorf("service %s: encoding containers: %w", s.Name, err)
	}
	secrets, err := defaultedSecretNames(s.Name, s.Secrets)
	if err != nil {
		return fmt.Errorf("service %s: %w", s.Name, err)
	}
	secretsJSON, err := marshalMap(secrets)
	if err != nil {
		return fmt.Errorf("service %s: encoding secrets: %w", s.Name, err)
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = db.Exec(`
		INSERT INTO services (name, catalog_entry, destination, params_json, containers_json, secrets_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			catalog_entry = excluded.catalog_entry,
			destination = excluded.destination,
			params_json = excluded.params_json,
			containers_json = excluded.containers_json,
			secrets_json = excluded.secrets_json,
			updated_at = excluded.updated_at`,
		s.Name, s.CatalogEntry, s.Destination, paramsJSON, containersJSON, secretsJSON, now, now,
	)
	if err != nil {
		return fmt.Errorf("saving service %s: %w", s.Name, err)
	}
	return nil
}

// GetService returns the named service, or ErrNotFound.
func GetService(db *sql.DB, name string) (Service, error) {
	var s Service
	var paramsJSON, containersJSON, secretsJSON string
	err := db.QueryRow(`
		SELECT name, catalog_entry, destination, params_json, containers_json, secrets_json
		FROM services WHERE name = ?`, name,
	).Scan(&s.Name, &s.CatalogEntry, &s.Destination, &paramsJSON, &containersJSON, &secretsJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return Service{}, fmt.Errorf("service %s: %w", name, ErrNotFound)
	}
	if err != nil {
		return Service{}, fmt.Errorf("getting service %s: %w", name, err)
	}
	if s.Params, err = unmarshalMap(paramsJSON); err != nil {
		return Service{}, fmt.Errorf("service %s: decoding params: %w", name, err)
	}
	if s.Containers, err = unmarshalMap(containersJSON); err != nil {
		return Service{}, fmt.Errorf("service %s: decoding containers: %w", name, err)
	}
	if s.Secrets, err = unmarshalMap(secretsJSON); err != nil {
		return Service{}, fmt.Errorf("service %s: decoding secrets: %w", name, err)
	}
	return s, nil
}

// ListServices returns every configured service, ordered by name.
func ListServices(db *sql.DB) ([]Service, error) {
	rows, err := db.Query(`SELECT name, catalog_entry, destination, params_json, containers_json, secrets_json FROM services ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("listing services: %w", err)
	}
	defer rows.Close()

	// Non-nil empty slice for the same reason as ListDestinations: this
	// is a JSON API response body, and a nil slice marshals as `null`.
	out := []Service{}
	for rows.Next() {
		var s Service
		var paramsJSON, containersJSON, secretsJSON string
		if err := rows.Scan(&s.Name, &s.CatalogEntry, &s.Destination, &paramsJSON, &containersJSON, &secretsJSON); err != nil {
			return nil, fmt.Errorf("listing services: %w", err)
		}
		if s.Params, err = unmarshalMap(paramsJSON); err != nil {
			return nil, fmt.Errorf("service %s: decoding params: %w", s.Name, err)
		}
		if s.Containers, err = unmarshalMap(containersJSON); err != nil {
			return nil, fmt.Errorf("service %s: decoding containers: %w", s.Name, err)
		}
		if s.Secrets, err = unmarshalMap(secretsJSON); err != nil {
			return nil, fmt.Errorf("service %s: decoding secrets: %w", s.Name, err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing services: %w", err)
	}
	return out, nil
}

// DeleteService removes a service. Nothing else references a service
// by name, so no ErrInUse case exists here (unlike DeleteDestination).
func DeleteService(db *sql.DB, name string) error {
	res, err := db.Exec(`DELETE FROM services WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("deleting service %s: %w", name, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("service %s: %w", name, ErrNotFound)
	}
	return nil
}

// defaultedSecretNames returns secrets with every empty credential
// file name defaulted to "ballast-secret.<service>.<name>" (the same
// default-naming pattern PutDestination applies to
// PasswordCredential), validates that every name is a plain file name
// (no path separators — it is joined onto a credential directory
// verbatim), and leaves the caller's map untouched.
func defaultedSecretNames(service string, secrets map[string]string) (map[string]string, error) {
	if len(secrets) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(secrets))
	for name, cred := range secrets {
		if cred == "" {
			cred = "ballast-secret." + service + "." + name
		}
		if filepath.Base(cred) != cred {
			return nil, fmt.Errorf("secret %s: credential file name %q must be a plain file name (no path separators)", name, cred)
		}
		out[name] = cred
	}
	return out, nil
}

// marshalMap encodes m as JSON, treating a nil map as "{}".
func marshalMap(m map[string]string) (string, error) {
	if m == nil {
		m = map[string]string{}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// unmarshalMap decodes JSON produced by marshalMap.
func unmarshalMap(s string) (map[string]string, error) {
	if s == "" {
		return map[string]string{}, nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string]string{}
	}
	return m, nil
}

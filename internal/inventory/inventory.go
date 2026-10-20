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
// backend credentials as URL userinfo either: backend secrets live in
// their own credential file (Destination.BackendCredential).
package inventory

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/springledev/ballast/internal/backend"
	"github.com/springledev/ballast/internal/catalog"
	"github.com/springledev/ballast/internal/restic"
	"github.com/springledev/ballast/internal/store"
	"github.com/springledev/keel/dockerhost"
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
// names) without a separate DTO layer. It holds no secrets: backend
// credentials are only ever referenced by credential file name.
type Destination struct {
	Name string `json:"name"`
	// Type is the storage backend (spec §17.1). Empty on input means
	// "infer from Location" (an absolute path is local, anything else
	// a custom restic repository string), for older callers.
	Type backend.Type `json:"type"`
	// Settings are the backend's non-secret settings (bucket, host...).
	Settings backend.Settings `json:"settings"`
	// Location is the restic repository string PutDestination
	// generates from Type and Settings. On input it is only read when
	// Settings is empty (the pre-typed "--location" shape).
	Location string `json:"location"`
	// PasswordCredential is the file NAME (not path) inside the
	// credential directory holding the repository password.
	PasswordCredential string `json:"password_credential"`
	// BackendCredential is the file NAME holding the backend's own
	// credentials (access key pair or SSH key); empty when the type
	// needs none.
	BackendCredential string `json:"backend_credential,omitempty"`
	// KeepLast keeps the newest N backups regardless of age (0 = off).
	KeepLast    int `json:"keep_last,omitempty"`
	KeepDaily   int `json:"keep_daily"`
	KeepWeekly  int `json:"keep_weekly"`
	KeepMonthly int `json:"keep_monthly"`
}

// SelfBackupService is the reserved name under which Ballast's own
// settings backup is scheduled, run, shown and alerted on like a
// service. No configured service may use it.
const SelfBackupService = "ballast-self"

// Retention returns d's restic retention policy.
func (d Destination) Retention() restic.RetentionPolicy {
	return Retention{KeepLast: d.KeepLast, KeepDaily: d.KeepDaily, KeepWeekly: d.KeepWeekly, KeepMonthly: d.KeepMonthly}.Policy()
}

// Target returns what backend.Resolver needs to open d's repository.
func (d Destination) Target() backend.Target {
	return backend.Target{
		Name:               d.Name,
		Type:               d.Type,
		Settings:           d.Settings,
		Location:           d.Location,
		PasswordCredential: d.PasswordCredential,
		BackendCredential:  d.BackendCredential,
	}
}

// Normalize validates d and fills everything PutDestination derives:
// the inferred Type, Settings from a bare Location, the generated
// Location, and default credential file names. It never touches the
// database, so callers can validate a destination before saving it.
func (d *Destination) Normalize() error {
	if err := ValidateName(d.Name); err != nil {
		return fmt.Errorf("destination: %w", err)
	}
	if d.Type == "" {
		if d.Location == "" {
			return fmt.Errorf("destination %s: choose a destination type (%s)", d.Name, typeNames())
		}
		d.Type = backend.Custom
		if strings.HasPrefix(d.Location, "/") {
			d.Type = backend.Local
		}
	}
	if d.Settings == (backend.Settings{}) {
		switch d.Type {
		case backend.Local:
			d.Settings.Path = d.Location
		case backend.Custom:
			d.Settings.URL = d.Location
		}
	}
	loc, err := backend.Location(d.Type, d.Settings)
	if err != nil {
		return fmt.Errorf("destination %s: %w", d.Name, err)
	}
	d.Location = loc
	if !d.Type.IsResticRepository() {
		// A Proton Drive copy has no repository password and keeps only
		// the newest N run folders.
		if d.PasswordCredential != "" {
			return fmt.Errorf("destination %s: a %s copy has no repository password", d.Name, d.Type)
		}
		if d.KeepLast == 0 {
			d.KeepLast = DefaultCopyKeepLast
		}
		d.KeepDaily, d.KeepWeekly, d.KeepMonthly = 0, 0, 0
	} else if d.PasswordCredential == "" {
		d.PasswordCredential = "ballast-restic-password." + d.Name
	}
	if d.PasswordCredential != "" && filepath.Base(d.PasswordCredential) != d.PasswordCredential {
		return fmt.Errorf("destination %s: password credential file name %q must be a plain file name (no path separators)", d.Name, d.PasswordCredential)
	}
	if d.Type.CredentialKind() == backend.NoCredential {
		if d.BackendCredential != "" {
			return fmt.Errorf("destination %s: a %s destination does not use a backend credential file", d.Name, d.Type)
		}
	} else {
		if d.BackendCredential == "" {
			d.BackendCredential = d.Type.DefaultCredentialName(d.Name)
		}
		if filepath.Base(d.BackendCredential) != d.BackendCredential {
			return fmt.Errorf("destination %s: backend credential file name %q must be a plain file name (no path separators)", d.Name, d.BackendCredential)
		}
		if d.BackendCredential == d.PasswordCredential {
			return fmt.Errorf("destination %s: the backend credential file and the repository password file must be different files", d.Name)
		}
	}
	if err := (Retention{KeepLast: d.KeepLast, KeepDaily: d.KeepDaily, KeepWeekly: d.KeepWeekly, KeepMonthly: d.KeepMonthly}).Validate(); err != nil {
		return fmt.Errorf("destination %s: %w", d.Name, err)
	}
	return nil
}

// DefaultCopyKeepLast is how many run folders a Proton Drive copy keeps
// per service when nothing else is chosen.
const DefaultCopyKeepLast = 7

// Retention is a keep_daily/keep_weekly/keep_monthly policy: how many
// of the newest daily, weekly and monthly backups to keep. Older
// backups are removed and their space reclaimed after each verified
// backup.
type Retention struct {
	// KeepLast keeps the newest N backups regardless of age (0 = off).
	KeepLast    int `json:"keep_last,omitempty"`
	KeepDaily   int `json:"keep_daily"`
	KeepWeekly  int `json:"keep_weekly"`
	KeepMonthly int `json:"keep_monthly"`
}

// Validate rejects negative counts and a policy that keeps nothing
// (which would delete every backup).
func (r Retention) Validate() error {
	if r.KeepLast < 0 || r.KeepDaily < 0 || r.KeepWeekly < 0 || r.KeepMonthly < 0 {
		return fmt.Errorf("retention counts must not be negative")
	}
	if r.KeepLast == 0 && r.KeepDaily == 0 && r.KeepWeekly == 0 && r.KeepMonthly == 0 {
		return fmt.Errorf("retention policy must keep at least one snapshot (keep_last, keep_daily, keep_weekly and keep_monthly are all zero)")
	}
	return nil
}

// Policy converts r to the restic forget policy.
func (r Retention) Policy() restic.RetentionPolicy {
	return restic.RetentionPolicy{KeepLast: r.KeepLast, KeepDaily: r.KeepDaily, KeepWeekly: r.KeepWeekly, KeepMonthly: r.KeepMonthly}
}

func typeNames() string {
	names := make([]string, len(backend.Types))
	for i, t := range backend.Types {
		names[i] = string(t)
	}
	return strings.Join(names, ", ")
}

// PutDestination inserts or updates a destination after Normalize:
// the name is validated, the restic repository string is generated
// from Type and Settings (which rejects embedded credentials),
// credential file names get their defaults, and the retention policy
// must keep at least one snapshot somewhere (all-zero would mean every
// backup is forgotten, which is never what a retention setting means —
// restic.Repository.Forget rejects the same shape).
func PutDestination(db *sql.DB, d Destination) error {
	if err := d.Normalize(); err != nil {
		return err
	}
	settings, err := json.Marshal(d.Settings)
	if err != nil {
		return fmt.Errorf("destination %s: encoding settings: %w", d.Name, err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = db.Exec(`
		INSERT INTO destinations (name, type, settings_json, location, password_credential, backend_credential, keep_daily, keep_weekly, keep_monthly, keep_last, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			type = excluded.type,
			settings_json = excluded.settings_json,
			location = excluded.location,
			password_credential = excluded.password_credential,
			backend_credential = excluded.backend_credential,
			keep_daily = excluded.keep_daily,
			keep_weekly = excluded.keep_weekly,
			keep_monthly = excluded.keep_monthly,
			keep_last = excluded.keep_last,
			updated_at = excluded.updated_at`,
		d.Name, string(d.Type), string(settings), d.Location, d.PasswordCredential, d.BackendCredential, d.KeepDaily, d.KeepWeekly, d.KeepMonthly, d.KeepLast, now, now,
	)
	if err != nil {
		return fmt.Errorf("saving destination %s: %w", d.Name, err)
	}
	return nil
}

const destinationColumns = `name, type, settings_json, location, password_credential, backend_credential, keep_daily, keep_weekly, keep_monthly, keep_last`

type rowScanner interface{ Scan(dest ...any) error }

func scanDestination(row rowScanner) (Destination, error) {
	var d Destination
	var typ, settings string
	if err := row.Scan(&d.Name, &typ, &settings, &d.Location, &d.PasswordCredential, &d.BackendCredential, &d.KeepDaily, &d.KeepWeekly, &d.KeepMonthly, &d.KeepLast); err != nil {
		return Destination{}, err
	}
	d.Type = backend.Type(typ)
	if err := json.Unmarshal([]byte(settings), &d.Settings); err != nil {
		return Destination{}, fmt.Errorf("destination %s: decoding settings: %w", d.Name, err)
	}
	return d, nil
}

// GetDestination returns the named destination, or ErrNotFound.
func GetDestination(db *sql.DB, name string) (Destination, error) {
	d, err := scanDestination(db.QueryRow(`SELECT `+destinationColumns+` FROM destinations WHERE name = ?`, name))
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
	rows, err := db.Query(`SELECT ` + destinationColumns + ` FROM destinations ORDER BY name`)
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
		d, err := scanDestination(rows)
		if err != nil {
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
	if err = tx.QueryRow(`SELECT COUNT(DISTINCT service) FROM service_targets WHERE destination = ?`, name).Scan(&count); err != nil {
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
	// DockerHost names the Docker host every Docker operation for this
	// service runs against (spec §17.2). Empty means the built-in
	// "local" host.
	DockerHost string `json:"docker_host"`
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
	// Retention overrides the destination's retention policy for this
	// service alone. Nil means "same as the destination".
	Retention *Retention `json:"retention,omitempty"`
	// Project groups this service with others that make up one Docker
	// Compose stack (a compose-config snapshot, its volumes, a DB
	// dump), so the full-stack restore wizard (spec §14 step 23) can
	// restore them together in dependency order. Empty means the
	// service stands alone.
	Project string `json:"project,omitempty"`
	// ProxmoxNode and ProxmoxVMID attach this service to the Proxmox
	// guest it runs in (spec §17.4): the guest Ballast will back up as
	// a whole via vzdump once that orchestration exists (spec §14 step
	// 25). ProxmoxVMID 0 means "not attached to a guest" — the common
	// case, a service that runs directly in Docker or bare metal with
	// no Proxmox tier involved.
	ProxmoxNode string `json:"proxmox_node,omitempty"`
	ProxmoxVMID int    `json:"proxmox_vmid,omitempty"`
	// Excludes are "skip these" patterns applied on top of the catalog
	// entry's own, to every folder the service backs up (the way to
	// skip things in the generic folder service).
	Excludes []string `json:"excludes,omitempty"`
	// Targets are the destinations this service's backups go to, first
	// one primary (spec §19.4). Destination and Retention mirror the
	// first target. Empty on input means one target made of Destination
	// and Retention.
	Targets []Target `json:"targets,omitempty"`
	// Kind is "" for an ordinary catalog service or KindProxmoxGuest
	// for a Proxmox guest backup job (spec §19.6).
	Kind string `json:"kind,omitempty"`
	// Guest holds the settings of a KindProxmoxGuest service; the guest
	// itself is named by ProxmoxNode and ProxmoxVMID.
	Guest *GuestJob `json:"guest,omitempty"`
}

// KindProxmoxGuest is the Service.Kind of a Proxmox guest backup job.
// Such a job has no catalog entry; CatalogEntry holds this same value.
const KindProxmoxGuest = "proxmox-guest"

// GuestJob is the configuration of a Proxmox guest backup job.
type GuestJob struct {
	// Storage is the Proxmox storage the dump is written to.
	Storage string `json:"storage"`
	// Mode is snapshot (default), suspend or stop.
	Mode string `json:"mode,omitempty"`
	// KeepLast is how many of the dumps Ballast made to keep (0 = keep
	// them all). Dumps Ballast did not make are never deleted.
	KeepLast int `json:"keep_last,omitempty"`
	// CopyPath is the folder on the Ballast machine where Storage's dump
	// files appear (for example an NFS mount of it). It is needed only
	// when the service also copies each dump to a restic destination.
	CopyPath string `json:"copy_path,omitempty"`
	// ReadBackSampleMiB, when above zero, makes the check of a copy read
	// back only the first this-many MiB of the dump instead of
	// downloading all of it again. Faster and cheaper for large dumps,
	// but the rest of the file is not read back. Zero reads it all.
	ReadBackSampleMiB int `json:"read_back_sample_mib,omitempty"`
}

// KindProxmoxConfig is the Service.Kind of the built-in Proxmox config
// snapshot (spec §19.6): it reads the host's settings through the API
// and sends them to its destination(s) as JSON files.
const KindProxmoxConfig = "proxmox-config"

// IsConfigSnapshot reports whether s is a Proxmox config snapshot job.
func (s Service) IsConfigSnapshot() bool { return s.Kind == KindProxmoxConfig }

// IsGuestJob reports whether s is a Proxmox guest backup job.
func (s Service) IsGuestJob() bool { return s.Kind == KindProxmoxGuest }

// Target is one destination a service backs up to.
type Target struct {
	// Destination names the destination.
	Destination string `json:"destination"`
	// Parts limits what this target receives, by part name (a dump's
	// name, or a folder group's name). Empty means every part.
	Parts []string `json:"parts,omitempty"`
	// Retention overrides the destination's policy for this target.
	// Nil means the destination's own.
	Retention *Retention `json:"retention,omitempty"`
}

// EffectiveTargets returns the service's targets, never empty: a
// service saved without any gets one made of its Destination and
// Retention.
func (s Service) EffectiveTargets() []Target {
	var ts []Target
	if len(s.Targets) > 0 {
		ts = append(ts, s.Targets...)
	} else if s.IsGuestJob() && s.Destination == "" {
		return nil
	} else {
		ts = []Target{{Destination: s.Destination, Retention: s.Retention}}
	}
	if s.IsGuestJob() && s.Guest != nil && s.Guest.KeepLast > 0 {
		// A job's "keep the newest N" also covers its copies unless the
		// copy was given a retention of its own.
		for i := range ts {
			if ts[i].Retention == nil {
				ts[i].Retention = &Retention{KeepLast: s.Guest.KeepLast}
			}
		}
	}
	return ts
}

// Wants reports whether the target receives the named part.
func (t Target) Wants(part string) bool {
	if len(t.Parts) == 0 {
		return true
	}
	for _, p := range t.Parts {
		if p == part {
			return true
		}
	}
	return false
}

// EffectiveRetention is the policy applied to the target's backups in
// d: its own override when set, otherwise the destination's.
func (t Target) EffectiveRetention(d Destination) Retention {
	if t.Retention != nil {
		return *t.Retention
	}
	return Retention{KeepLast: d.KeepLast, KeepDaily: d.KeepDaily, KeepWeekly: d.KeepWeekly, KeepMonthly: d.KeepMonthly}
}

// normalizeTargets validates a service's targets and makes the first
// one authoritative for Destination and Retention. Duplicate
// destinations are refused: two targets in one repository would
// retention-prune each other.
func normalizeTargets(db *sql.DB, s *Service) error {
	if s.IsGuestJob() && len(s.Targets) == 0 && s.Destination == "" {
		return nil // a guest job may keep its dumps on Proxmox only
	}
	if len(s.Targets) == 0 {
		s.Targets = []Target{{Destination: s.Destination, Retention: s.Retention}}
	}
	seen := map[string]bool{}
	for i, t := range s.Targets {
		if t.Destination == "" {
			return fmt.Errorf("target %d has no destination", i+1)
		}
		if seen[t.Destination] {
			return fmt.Errorf("destination %s is listed twice; each target needs a different destination", t.Destination)
		}
		seen[t.Destination] = true
		dest, err := GetDestination(db, t.Destination)
		if err != nil {
			return err
		}
		if !dest.Type.IsResticRepository() {
			if i == 0 {
				return fmt.Errorf("%s is a Proton Drive copy, which only holds a second copy: choose a backup destination first and add %s under \"also send to\"", t.Destination, t.Destination)
			}
			if s.IsGuestJob() {
				return fmt.Errorf("target %s: Proton Drive copies are for small files (configs and database dumps); a Proxmox guest backup is too large to send there", t.Destination)
			}
		}
		if t.Retention != nil {
			if err := t.Retention.Validate(); err != nil {
				if i == 0 {
					return err
				}
				return fmt.Errorf("target %s: %w", t.Destination, err)
			}
		}
		for _, p := range t.Parts {
			if strings.TrimSpace(p) == "" {
				return fmt.Errorf("target %s: a part name is empty", t.Destination)
			}
		}
	}
	s.Destination = s.Targets[0].Destination
	s.Retention = s.Targets[0].Retention
	return nil
}

// saveTargets replaces the service's stored targets.
func saveTargets(tx *sql.Tx, s Service) error {
	if _, err := tx.Exec(`DELETE FROM service_targets WHERE service = ?`, s.Name); err != nil {
		return err
	}
	for i, t := range s.Targets {
		partsJSON, retJSON := "", ""
		if len(t.Parts) > 0 {
			b, err := json.Marshal(t.Parts)
			if err != nil {
				return err
			}
			partsJSON = string(b)
		}
		if t.Retention != nil {
			b, err := json.Marshal(t.Retention)
			if err != nil {
				return err
			}
			retJSON = string(b)
		}
		if _, err := tx.Exec(`INSERT INTO service_targets (service, position, destination, parts_json, retention_json) VALUES (?, ?, ?, ?, ?)`,
			s.Name, i, t.Destination, partsJSON, retJSON); err != nil {
			return err
		}
	}
	return nil
}

// loadTargets fills in Targets for each service in list.
func loadTargets(db *sql.DB, list []Service) error {
	for i := range list {
		rows, err := db.Query(`SELECT destination, parts_json, retention_json FROM service_targets WHERE service = ? ORDER BY position`, list[i].Name)
		if err != nil {
			return fmt.Errorf("service %s: loading targets: %w", list[i].Name, err)
		}
		var targets []Target
		for rows.Next() {
			var t Target
			var partsJSON, retJSON string
			if err := rows.Scan(&t.Destination, &partsJSON, &retJSON); err != nil {
				rows.Close()
				return fmt.Errorf("service %s: loading targets: %w", list[i].Name, err)
			}
			if partsJSON != "" {
				if err := json.Unmarshal([]byte(partsJSON), &t.Parts); err != nil {
					rows.Close()
					return fmt.Errorf("service %s: decoding target parts: %w", list[i].Name, err)
				}
			}
			if retJSON != "" {
				t.Retention = &Retention{}
				if err := json.Unmarshal([]byte(retJSON), t.Retention); err != nil {
					rows.Close()
					return fmt.Errorf("service %s: decoding target retention: %w", list[i].Name, err)
				}
			}
			targets = append(targets, t)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return fmt.Errorf("service %s: loading targets: %w", list[i].Name, err)
		}
		list[i].Targets = targets
	}
	return nil
}

// EffectiveRetention is the policy applied to s's backups in d: its
// own override when set, otherwise the destination's.
func (s Service) EffectiveRetention(d Destination) Retention {
	for _, t := range s.EffectiveTargets() {
		if t.Destination == d.Name && t.Retention != nil {
			return *t.Retention
		}
	}
	if s.Retention != nil {
		return *s.Retention
	}
	return Retention{KeepLast: d.KeepLast, KeepDaily: d.KeepDaily, KeepWeekly: d.KeepWeekly, KeepMonthly: d.KeepMonthly}
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
	if s.Name == SelfBackupService {
		return fmt.Errorf("service %s: that name is reserved for Ballast's own settings backup; choose another name", s.Name)
	}
	guestJSON := ""
	if s.IsGuestJob() {
		s.CatalogEntry = KindProxmoxGuest
		if s.Guest == nil || strings.TrimSpace(s.Guest.Storage) == "" {
			return fmt.Errorf("service %s: choose the Proxmox storage the backup is written to", s.Name)
		}
		if s.ProxmoxVMID <= 0 || s.ProxmoxNode == "" {
			return fmt.Errorf("service %s: a Proxmox guest job needs the guest's node and VMID", s.Name)
		}
		switch s.Guest.Mode {
		case "", "snapshot", "suspend", "stop":
		default:
			return fmt.Errorf("service %s: unknown backup mode %q — use snapshot, suspend or stop", s.Name, s.Guest.Mode)
		}
		if s.Guest.KeepLast < 0 {
			return fmt.Errorf("service %s: keep-last must not be negative", s.Name)
		}
		if (s.Destination != "" || len(s.Targets) > 0) && s.Guest.CopyPath == "" {
			return fmt.Errorf("service %s: to copy the dump to a destination, give the folder where Proxmox's %q storage appears on this machine", s.Name, s.Guest.Storage)
		}
		b, err := json.Marshal(s.Guest)
		if err != nil {
			return fmt.Errorf("service %s: encoding guest job: %w", s.Name, err)
		}
		guestJSON = string(b)
	} else if s.IsConfigSnapshot() {
		s.CatalogEntry = KindProxmoxConfig
	} else if s.Kind != "" {
		return fmt.Errorf("service %s: unknown kind %q", s.Name, s.Kind)
	}
	if s.CatalogEntry == "" {
		return fmt.Errorf("service %s: catalog entry must not be empty", s.Name)
	}
	if s.Destination == "" && !s.IsGuestJob() {
		return fmt.Errorf("service %s: destination must not be empty", s.Name)
	}
	if err := normalizeTargets(db, &s); err != nil {
		return fmt.Errorf("service %s: %w", s.Name, err)
	}
	if s.DockerHost == "" {
		s.DockerHost = dockerhost.LocalName
	}
	if _, err := GetDockerHost(db, s.DockerHost); err != nil {
		return fmt.Errorf("service %s: %w", s.Name, err)
	}
	if s.Project != "" {
		if err := ValidateName(s.Project); err != nil {
			return fmt.Errorf("service %s: project: %w", s.Name, err)
		}
	}
	if (s.ProxmoxVMID != 0) != (s.ProxmoxNode != "") {
		return fmt.Errorf("service %s: proxmox_node and proxmox_vmid must be given together", s.Name)
	}
	if s.ProxmoxVMID < 0 {
		return fmt.Errorf("service %s: proxmox_vmid must not be negative", s.Name)
	}
	if err := catalog.ValidateExcludes(s.Excludes); err != nil {
		return fmt.Errorf("service %s: %w", s.Name, err)
	}
	excludesJSON := ""
	if len(s.Excludes) > 0 {
		b, err := json.Marshal(s.Excludes)
		if err != nil {
			return fmt.Errorf("service %s: encoding excludes: %w", s.Name, err)
		}
		excludesJSON = string(b)
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
	retentionJSON := ""
	if s.Retention != nil {
		if err := s.Retention.Validate(); err != nil {
			return fmt.Errorf("service %s: %w", s.Name, err)
		}
		b, err := json.Marshal(s.Retention)
		if err != nil {
			return fmt.Errorf("service %s: encoding retention: %w", s.Name, err)
		}
		retentionJSON = string(b)
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("saving service %s: %w", s.Name, err)
	}
	defer func() { err = store.RollbackOnError(tx, err) }()
	_, err = tx.Exec(`
		INSERT INTO services (name, catalog_entry, destination, docker_host, params_json, containers_json, secrets_json, retention_json, project, proxmox_node, proxmox_vmid, excludes_json, kind, guest_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			catalog_entry = excluded.catalog_entry,
			destination = excluded.destination,
			docker_host = excluded.docker_host,
			params_json = excluded.params_json,
			containers_json = excluded.containers_json,
			secrets_json = excluded.secrets_json,
			retention_json = excluded.retention_json,
			excludes_json = excluded.excludes_json,
			project = excluded.project,
			proxmox_node = excluded.proxmox_node,
			proxmox_vmid = excluded.proxmox_vmid,
			kind = excluded.kind,
			guest_json = excluded.guest_json,
			updated_at = excluded.updated_at`,
		s.Name, s.CatalogEntry, s.Destination, s.DockerHost, paramsJSON, containersJSON, secretsJSON, retentionJSON, s.Project, s.ProxmoxNode, s.ProxmoxVMID, excludesJSON, s.Kind, guestJSON, now, now,
	)
	if err != nil {
		return fmt.Errorf("saving service %s: %w", s.Name, err)
	}
	if err = saveTargets(tx, s); err != nil {
		return fmt.Errorf("saving service %s: saving targets: %w", s.Name, err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("saving service %s: %w", s.Name, err)
	}
	return nil
}

// GetService returns the named service, or ErrNotFound.
func GetService(db *sql.DB, name string) (Service, error) {
	s, err := scanService(db.QueryRow(`SELECT `+serviceColumns+` FROM services WHERE name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return Service{}, fmt.Errorf("service %s: %w", name, ErrNotFound)
	}
	if err != nil {
		return Service{}, fmt.Errorf("getting service %s: %w", name, err)
	}
	one := []Service{s}
	if err := loadTargets(db, one); err != nil {
		return Service{}, err
	}
	return one[0], nil
}

const serviceColumns = `name, catalog_entry, destination, docker_host, params_json, containers_json, secrets_json, retention_json, project, proxmox_node, proxmox_vmid, excludes_json, kind, guest_json`

// scanService reads one serviceColumns row.
func scanService(row interface{ Scan(...any) error }) (Service, error) {
	var s Service
	var paramsJSON, containersJSON, secretsJSON, retentionJSON, excludesJSON, guestJSON string
	if err := row.Scan(&s.Name, &s.CatalogEntry, &s.Destination, &s.DockerHost, &paramsJSON, &containersJSON, &secretsJSON, &retentionJSON, &s.Project, &s.ProxmoxNode, &s.ProxmoxVMID, &excludesJSON, &s.Kind, &guestJSON); err != nil {
		return Service{}, err
	}
	var err error
	if s.Params, err = unmarshalMap(paramsJSON); err != nil {
		return Service{}, fmt.Errorf("service %s: decoding params: %w", s.Name, err)
	}
	if s.Containers, err = unmarshalMap(containersJSON); err != nil {
		return Service{}, fmt.Errorf("service %s: decoding containers: %w", s.Name, err)
	}
	if s.Secrets, err = unmarshalMap(secretsJSON); err != nil {
		return Service{}, fmt.Errorf("service %s: decoding secrets: %w", s.Name, err)
	}
	if excludesJSON != "" {
		if err := json.Unmarshal([]byte(excludesJSON), &s.Excludes); err != nil {
			return Service{}, fmt.Errorf("service %s: decoding excludes: %w", s.Name, err)
		}
	}
	if guestJSON != "" {
		s.Guest = &GuestJob{}
		if err := json.Unmarshal([]byte(guestJSON), s.Guest); err != nil {
			return Service{}, fmt.Errorf("service %s: decoding guest job: %w", s.Name, err)
		}
	}
	if retentionJSON != "" {
		s.Retention = &Retention{}
		if err := json.Unmarshal([]byte(retentionJSON), s.Retention); err != nil {
			return Service{}, fmt.Errorf("service %s: decoding retention: %w", s.Name, err)
		}
	}
	return s, nil
}

// ListServices returns every configured service, ordered by name.
func ListServices(db *sql.DB) ([]Service, error) {
	rows, err := db.Query(`SELECT ` + serviceColumns + ` FROM services ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("listing services: %w", err)
	}
	defer rows.Close()

	// Non-nil empty slice for the same reason as ListDestinations: this
	// is a JSON API response body, and a nil slice marshals as `null`.
	out := []Service{}
	for rows.Next() {
		s, err := scanService(rows)
		if err != nil {
			return nil, fmt.Errorf("listing services: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing services: %w", err)
	}
	rows.Close()
	if err := loadTargets(db, out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListServicesByProject returns every service in project, ordered by
// name, for the full-stack restore wizard (spec §14 step 23).
func ListServicesByProject(db *sql.DB, project string) ([]Service, error) {
	rows, err := db.Query(`SELECT `+serviceColumns+` FROM services WHERE project = ? ORDER BY name`, project)
	if err != nil {
		return nil, fmt.Errorf("listing services in project %s: %w", project, err)
	}
	defer rows.Close()

	out := []Service{}
	for rows.Next() {
		s, err := scanService(rows)
		if err != nil {
			return nil, fmt.Errorf("listing services in project %s: %w", project, err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing services in project %s: %w", project, err)
	}
	rows.Close()
	if err := loadTargets(db, out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListProjects returns the distinct, non-empty project names in use,
// ordered alphabetically.
func ListProjects(db *sql.DB) ([]string, error) {
	rows, err := db.Query(`SELECT DISTINCT project FROM services WHERE project != '' ORDER BY project`)
	if err != nil {
		return nil, fmt.Errorf("listing projects: %w", err)
	}
	defer rows.Close()

	out := []string{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("listing projects: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing projects: %w", err)
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
		if _, _, ok := ParseEnvRef(cred); ok {
			if _, _, err := validEnvRef(cred); err != nil {
				return nil, fmt.Errorf("secret %s: %w", name, err)
			}
			out[name] = cred
			continue
		}
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

// EnvRefPrefix marks a Service.Secrets value as a reference to a key in
// an application's .env file ("env-file:<absolute path>#<KEY>") rather
// than the name of a Ballast credential file (spec §18.2). Only the
// path and key are stored; the value is read at job-run time.
const EnvRefPrefix = "env-file:"

// EnvRef builds the Service.Secrets value pointing at key in the .env
// file at path.
func EnvRef(path, key string) string { return EnvRefPrefix + path + "#" + key }

// ParseEnvRef splits an env-file reference into its path and key. ok is
// false when ref is an ordinary credential file name.
func ParseEnvRef(ref string) (path, key string, ok bool) {
	rest, found := strings.CutPrefix(ref, EnvRefPrefix)
	if !found {
		return "", "", false
	}
	i := strings.LastIndex(rest, "#")
	if i < 0 {
		return rest, "", true
	}
	return rest[:i], rest[i+1:], true
}

// validEnvRef checks that ref names an absolute path and a plain key.
func validEnvRef(ref string) (path, key string, err error) {
	path, key, _ = ParseEnvRef(ref)
	if !filepath.IsAbs(path) {
		return "", "", fmt.Errorf("the .env file path %q must be an absolute path (for example /opt/immich/.env)", path)
	}
	if key == "" || strings.ContainsAny(key, " \t=#") {
		return "", "", fmt.Errorf("the .env key %q must be a plain variable name such as DB_PASSWORD", key)
	}
	return path, key, nil
}

// ValidateEnvRef checks an env-file reference and returns its path and key.
func ValidateEnvRef(ref string) (path, key string, err error) { return validEnvRef(ref) }

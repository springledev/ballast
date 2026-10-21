// Package store wraps the SQLite database (pure-Go
// modernc.org/sqlite driver so the binary stays fully static) and
// applies embedded SQL migrations in order.
//
// Migrations live in ./migrations as numbered .sql files
// (`001_something.sql`). Each runs exactly once, inside one
// transaction, and is recorded in schema_migrations before Open
// returns.
package store

import (
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Store is a handle to the Ballast SQLite database.
type Store struct {
	db *sql.DB
}

// connOptions are applied by the driver to every connection in the
// database/sql pool. Setting them with a one-off `PRAGMA` statement
// would only reach whichever pooled connection ran it, leaving the
// others with no busy timeout (instant "database is locked" under
// concurrent writers — parallel sweeps, run-due racing trigger) and
// without foreign-key enforcement. _txlock=immediate makes every
// transaction take the write lock up front, so a writer waits in the
// busy handler instead of failing on a mid-transaction lock upgrade.
const connOptions = "_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_txlock=immediate"

// Open opens (creating it if necessary) the database at path, applies
// all pending migrations, and returns a ready Store.
func Open(path string) (*Store, error) {
	if strings.Contains(path, "?") {
		// The driver splits its DSN at the first "?", so such a path
		// would silently open the wrong file.
		return nil, fmt.Errorf("opening sqlite db %s: the database path must not contain \"?\"", path)
	}
	db, err := sql.Open("sqlite", path+"?"+connOptions)
	if err != nil {
		return nil, fmt.Errorf("opening sqlite db %s: %w", path, err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("opening sqlite db %s: %w", path, err)
	}

	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the raw database handle to packages that build on the
// store (engine, api). Treated as internal by convention.
func (s *Store) DB() *sql.DB { return s.db }

// migrate applies every unapplied migration in ascending order.
func migrate(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		name    TEXT NOT NULL,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("creating schema_migrations: %w", err)
	}

	entries, err := fs.Glob(migrationsFS, "migrations/*.sql")
	if err != nil {
		return fmt.Errorf("listing embedded migrations: %w", err)
	}
	if len(entries) == 0 {
		return fmt.Errorf("no embedded migrations found")
	}
	sort.Strings(entries)

	for _, entry := range entries {
		name := migrationName(entry)
		version, err := migrationVersion(entry)
		if err != nil {
			return err
		}

		var exists int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, version,
		).Scan(&exists); err != nil {
			return fmt.Errorf("checking migration %d state: %w", version, err)
		}
		if exists > 0 {
			continue
		}

		sqlBytes, err := migrationsFS.ReadFile(entry)
		if err != nil {
			return fmt.Errorf("reading embedded migration %s: %w", name, err)
		}

		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("beginning transaction for migration %d: %w", version, err)
		}
		if _, err := tx.Exec(string(sqlBytes)); err != nil {
			return RollbackOnError(tx, fmt.Errorf("applying migration %d (%s): %w", version, name, err))
		}
		if _, err := tx.Exec(
			`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
			version, name, time.Now().UTC().Format(time.RFC3339),
		); err != nil {
			return RollbackOnError(tx, fmt.Errorf("recording migration %d state: %w", version, err))
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("committing migration %d: %w", version, err)
		}
	}
	return nil
}

// RollbackOnError rolls tx back when err is set, reporting a failed
// rollback alongside err instead of dropping it, and returns err
// unchanged otherwise. sql.ErrTxDone is not a failure: it means the
// transaction already ended (e.g. a failed Commit), so there was
// nothing left to roll back. Intended for a deferred call:
//
//	defer func() { err = store.RollbackOnError(tx, err) }()
func RollbackOnError(tx *sql.Tx, err error) error {
	if err == nil {
		return nil
	}
	if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
		return errors.Join(err, fmt.Errorf("rolling back: %w", rbErr))
	}
	return err
}

// SchemaVersion returns the highest applied migration version (-1 when
// none), for diagnostics and API surface.
func (s *Store) SchemaVersion() (int64, error) {
	return SchemaVersionOf(s.db)
}

// SchemaVersionOf is SchemaVersion for a bare database handle, so code
// that only holds a *sql.DB (such as the API's doctor check) can ask.
func SchemaVersionOf(db *sql.DB) (int64, error) {
	var v sql.NullInt64
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&v); err != nil {
		return 0, fmt.Errorf("querying schema version: %w", err)
	}
	return v.Int64, nil
}

// MetaGet returns the value stored under key, or "" and
// ErrMetaNotFound when absent.
func (s *Store) MetaGet(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", ErrMetaNotFound
	}
	if err != nil {
		return "", fmt.Errorf("meta get %q: %w", key, err)
	}
	return v, nil
}

// ErrMetaNotFound is returned by MetaGet when the key is absent.
var ErrMetaNotFound = fmt.Errorf("meta key not found")

// MetaSet stores value under key, upserting.
func (s *Store) MetaSet(key, value string) error {
	if strings.TrimSpace(key) == "" {
		return fmt.Errorf("meta key must not be empty")
	}
	if _, err := s.db.Exec(
		`INSERT INTO meta (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value,
	); err != nil {
		return fmt.Errorf("meta set %q: %w", key, err)
	}
	return nil
}

// migrationName strips the directory and extension: "001_meta.sql" ->
// "meta".
func migrationName(entry string) string {
	n := entry[strings.LastIndex(entry, "/")+1:]
	n = strings.TrimSuffix(n, ".sql")
	if idx := strings.Index(n, "_"); idx >= 0 {
		return n[idx+1:]
	}
	return n
}

// migrationVersion parses the leading numeric prefix ("001_x" -> 1).
func migrationVersion(entry string) (int64, error) {
	n := entry[strings.LastIndex(entry, "/")+1:]
	idx := strings.Index(n, "_")
	if idx == -1 {
		return 0, fmt.Errorf("embedded migration %s lacks a numeric prefix", entry)
	}
	v, err := strconv.ParseInt(n[:idx], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing version prefix of migration %s: %w", entry, err)
	}
	return v, nil
}

// HistoryFloorKeyPrefix starts the meta key ("history_floor:<service>")
// that holds the newest run id belonging to a service that has since been
// removed. Removing a service keeps its run history, but a later service
// with the same name starts clean: status and freshness queries only look
// at runs newer than this floor.
const HistoryFloorKeyPrefix = "history_floor:"

// RunsAfterFloorSQL is a SQL condition for queries over runs r joined to
// jobs j: it keeps only runs newer than the service's history floor.
const RunsAfterFloorSQL = `r.id > COALESCE((SELECT CAST(m.value AS INTEGER) FROM meta m WHERE m.key = '` + HistoryFloorKeyPrefix + `' || j.service), 0)`

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	_ "modernc.org/sqlite"
)

func openTest(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ballast.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func TestOpenCreatesAndMigrates(t *testing.T) {
	s, _ := openTest(t)
	v, err := s.SchemaVersion()
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if v < 1 {
		t.Errorf("schema version = %d, want >= 1", v)
	}
	// Migration 001 must have created the meta table.
	if err := s.MetaSet("k", "v"); err != nil {
		t.Fatalf("MetaSet: %v", err)
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ballast.db")
	s1, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := s1.MetaSet("marker", "x"); err != nil {
		t.Fatalf("MetaSet: %v", err)
	}
	v1, _ := s1.SchemaVersion()
	s1.Close()

	// Reopening must apply no migrations again and keep data.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer s2.Close()
	v2, _ := s2.SchemaVersion()
	if v1 != v2 {
		t.Errorf("schema version changed after reopen: %d -> %d", v1, v2)
	}
	got, err := s2.MetaGet("marker")
	if err != nil || got != "x" {
		t.Errorf("MetaGet marker = %q, %v", got, err)
	}
}

func TestOpenRejectsBadPath(t *testing.T) {
	if _, err := Open("/proc/definitely/not/a/dir/x.db"); err == nil {
		t.Fatal("expected error opening db under /proc")
	}
}

func TestMetaRoundTrip(t *testing.T) {
	s, _ := openTest(t)
	if _, err := s.MetaGet("missing"); err != ErrMetaNotFound {
		t.Errorf("missing key: err = %v, want ErrMetaNotFound", err)
	}
	if err := s.MetaSet("job", "abc"); err != nil {
		t.Fatalf("MetaSet: %v", err)
	}
	if err := s.MetaSet("job", "def"); err != nil {
		t.Fatalf("MetaSet overwrite: %v", err)
	}
	got, err := s.MetaGet("job")
	if err != nil || got != "def" {
		t.Errorf("MetaGet = %q, %v; want def", got, err)
	}
	if err := s.MetaSet("  ", "v"); err == nil {
		t.Error("expected error for empty meta key")
	}
}

func TestMigrationVersion(t *testing.T) {
	cases := []struct {
		entry string
		want  int64
		fail  bool
	}{
		{entry: "migrations/001_meta.sql", want: 1},
		{entry: "migrations/012_add_things.sql", want: 12},
		{entry: "migrations/bad.sql", fail: true},
		{entry: "migrations/xx_bad.sql", fail: true},
	}
	for _, tc := range cases {
		v, err := migrationVersion(tc.entry)
		if tc.fail {
			if err == nil {
				t.Errorf("%s: expected error", tc.entry)
			}
			continue
		}
		if err != nil || v != tc.want {
			t.Errorf("%s: v=%d err=%v, want %d", tc.entry, v, err, tc.want)
		}
	}
}

func TestMigrationName(t *testing.T) {
	if got := migrationName("migrations/001_meta.sql"); got != "meta" {
		t.Errorf("migrationName = %q, want meta", got)
	}
}

func TestSchemaVersionOnFreshDB(t *testing.T) {
	// A fresh Open applies every embedded migration (versions 1..n)
	// and switches the database to WAL mode.
	s, _ := openTest(t)
	entries, err := fs.Glob(migrationsFS, "migrations/*.sql")
	if err != nil {
		t.Fatalf("listing embedded migrations: %v", err)
	}
	v, err := s.SchemaVersion()
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if v != int64(len(entries)) {
		t.Errorf("schema version = %d, want %d (one per embedded migration)", v, len(entries))
	}
	var mode string
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal mode = %q, want wal", mode)
	}
}

// TestOpenConfiguresEveryPooledConnection guards against settings that
// only reach one connection of the database/sql pool.
func TestOpenConfiguresEveryPooledConnection(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "ballast.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	// Hold several connections at once so the pool must open new ones.
	var conns []*sql.Conn
	for i := 0; i < 4; i++ {
		c, err := s.DB().Conn(ctx)
		if err != nil {
			t.Fatalf("Conn %d: %v", i, err)
		}
		conns = append(conns, c)
	}
	for i, c := range conns {
		var busy, fk int
		var journal string
		if err := c.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busy); err != nil {
			t.Fatalf("conn %d busy_timeout: %v", i, err)
		}
		if err := c.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil {
			t.Fatalf("conn %d foreign_keys: %v", i, err)
		}
		if err := c.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journal); err != nil {
			t.Fatalf("conn %d journal_mode: %v", i, err)
		}
		if busy != 5000 || fk != 1 || journal != "wal" {
			t.Errorf("conn %d: busy_timeout=%d foreign_keys=%d journal_mode=%s; want 5000, 1, wal", i, busy, fk, journal)
		}
		c.Close()
	}
}

func TestOpenConcurrentWritersDoNotFailBusy(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "ballast.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	const writers, writes = 8, 25
	var wg sync.WaitGroup
	errs := make(chan error, writers*writes)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < writes; i++ {
				tx, err := s.DB().Begin()
				if err != nil {
					errs <- err
					return
				}
				if _, err := tx.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES (?, ?)`,
					fmt.Sprintf("k%d", w), fmt.Sprint(i)); err != nil {
					errs <- errors.Join(err, tx.Rollback())
					return
				}
				if err := tx.Commit(); err != nil {
					errs <- err
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent write failed: %v", err)
	}
}

func TestOpenRejectsQuestionMarkInPath(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "odd?name.db")); err == nil || !strings.Contains(err.Error(), `must not contain "?"`) {
		t.Errorf("Open error = %v, want a clear rejection", err)
	}
}

// TestMigration007DestinationBackends exercises migration 007 in
// isolation: a `destinations` table shaped exactly as migrations
// 001-006 left it (006 only touched `services`, so 005's shape is
// still current) gets two pre-existing rows — one with an absolute
// local path, one with a raw restic repository string — and then the
// 007 SQL itself is applied directly. store.Open always migrates a
// fresh DB straight to the latest version, so there is no supported
// way to stop it at 006; running the literal migration SQL against a
// table in that shape is the simplest faithful reproduction.
func TestMigration007DestinationBackends(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "mig007.db")+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("opening sqlite: %v", err)
	}
	defer db.Close()

	// The pre-007 (== post-005) destinations shape.
	if _, err := db.Exec(`CREATE TABLE destinations (
		name                TEXT PRIMARY KEY,
		location            TEXT NOT NULL,
		password_credential TEXT NOT NULL,
		keep_daily   INTEGER NOT NULL DEFAULT 7,
		keep_weekly  INTEGER NOT NULL DEFAULT 4,
		keep_monthly INTEGER NOT NULL DEFAULT 6,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`); err != nil {
		t.Fatalf("creating pre-migration destinations table: %v", err)
	}

	now := "2024-01-01T00:00:00Z"
	if _, err := db.Exec(
		`INSERT INTO destinations (name, location, password_credential, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		"local1", "/srv/b", "pw.local1", now, now,
	); err != nil {
		t.Fatalf("inserting local destination: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO destinations (name, location, password_credential, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		"custom1", "rest:http://x/", "pw.custom1", now, now,
	); err != nil {
		t.Fatalf("inserting custom destination: %v", err)
	}

	sqlBytes, err := migrationsFS.ReadFile("migrations/007_destination_backends.sql")
	if err != nil {
		t.Fatalf("reading migration 007: %v", err)
	}
	if _, err := db.Exec(string(sqlBytes)); err != nil {
		t.Fatalf("applying migration 007: %v", err)
	}

	tests := []struct {
		name         string
		wantType     string
		wantSettings string
	}{
		{"local1", "local", `{"path":"/srv/b"}`},
		{"custom1", "custom", `{"url":"rest:http://x/"}`},
	}
	for _, tt := range tests {
		var typ, settings string
		if err := db.QueryRow(`SELECT type, settings_json FROM destinations WHERE name = ?`, tt.name).Scan(&typ, &settings); err != nil {
			t.Fatalf("querying %s: %v", tt.name, err)
		}
		if typ != tt.wantType {
			t.Errorf("%s: type = %q, want %q", tt.name, typ, tt.wantType)
		}
		if settings != tt.wantSettings {
			t.Errorf("%s: settings_json = %q, want %q", tt.name, settings, tt.wantSettings)
		}
		var backendCred string
		if err := db.QueryRow(`SELECT backend_credential FROM destinations WHERE name = ?`, tt.name).Scan(&backendCred); err != nil {
			t.Fatalf("querying %s backend_credential: %v", tt.name, err)
		}
		if backendCred != "" {
			t.Errorf("%s: backend_credential = %q, want empty default", tt.name, backendCred)
		}
	}
}

func TestRollbackOnError(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name   string
		commit bool // commit first, as when a failed Commit ended the tx
		err    error
		want   error
	}{
		{name: "no error leaves the transaction open", err: nil, want: nil},
		{name: "error rolls back and is returned unchanged", err: boom, want: boom},
		{name: "already-ended transaction is not a rollback failure", commit: true, err: boom, want: boom},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := openTest(t)
			tx, err := s.DB().Begin()
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			if _, err := tx.Exec(`INSERT INTO meta (key, value) VALUES ('probe', 'x')`); err != nil {
				t.Fatalf("Exec: %v", err)
			}
			if tt.commit {
				if err := tx.Commit(); err != nil {
					t.Fatalf("Commit: %v", err)
				}
			}
			if got := RollbackOnError(tx, tt.err); got != tt.want {
				t.Errorf("RollbackOnError = %v, want exactly %v", got, tt.want)
			}
			// The transaction must be finished exactly when an error
			// was passed (or it was already committed).
			open := tx.Rollback() == nil
			if wantOpen := tt.err == nil; open != wantOpen {
				t.Errorf("transaction still open = %v, want %v", open, wantOpen)
			}
			// Only a committed insert survives; every other case has
			// now been rolled back (by RollbackOnError or just above).
			_, err = s.MetaGet("probe")
			if kept := err == nil; kept != tt.commit {
				t.Errorf("insert kept = %v (MetaGet err %v), want %v", kept, err, tt.commit)
			}
		})
	}
}

// TestMigration017GivesEveryServiceOneTarget proves an existing service
// becomes one target with all parts, carrying its retention override.
func TestMigration017GivesEveryServiceOneTarget(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "ballast.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	db := s.DB()
	for _, q := range []string{
		`DROP TABLE service_targets`,
		`DROP TABLE run_targets`,
		`ALTER TABLE verification_results DROP COLUMN target`,
		`DELETE FROM schema_migrations WHERE version = 17`,
		`INSERT INTO destinations (name, location, password_credential, created_at, updated_at) VALUES ('ssd', '/srv', 'pw', 'now', 'now')`,
		`INSERT INTO services (name, catalog_entry, destination, retention_json, created_at, updated_at) VALUES ('immich', 'immich', 'ssd', '{"keep_daily":2}', 'now', 'now')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var dest, parts, ret string
	if err := db.QueryRow(`SELECT destination, parts_json, retention_json FROM service_targets WHERE service = 'immich' AND position = 0`).Scan(&dest, &parts, &ret); err != nil {
		t.Fatalf("reading migrated target: %v", err)
	}
	if dest != "ssd" || parts != "" || ret != `{"keep_daily":2}` {
		t.Errorf("target = %q parts %q retention %q, want ssd, all parts, the service's retention", dest, parts, ret)
	}
}

// TestMigration018KeepsServicesAndTargets proves rebuilding the services
// table (to allow a guest job with no destination) loses no service and
// no destination target.
func TestMigration018KeepsServicesAndTargets(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "ballast.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	db := s.DB()
	for _, q := range []string{
		`DROP TABLE proxmox_dumps`,
		`DELETE FROM schema_migrations WHERE version = 18`,
		`INSERT INTO destinations (name, location, password_credential, created_at, updated_at) VALUES ('ssd', '/srv', 'pw', 'now', 'now')`,
		`INSERT INTO services (name, catalog_entry, destination, retention_json, project, created_at, updated_at) VALUES ('immich', 'immich', 'ssd', '{"keep_daily":2}', 'photos', 'now', 'now')`,
		`INSERT INTO service_targets (service, position, destination) VALUES ('immich', 0, 'ssd')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var dest, project, kind string
	if err := db.QueryRow(`SELECT destination, project, kind FROM services WHERE name = 'immich'`).Scan(&dest, &project, &kind); err != nil {
		t.Fatalf("reading migrated service: %v", err)
	}
	if dest != "ssd" || project != "photos" || kind != "" {
		t.Errorf("service = %q %q %q", dest, project, kind)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM service_targets WHERE service = 'immich'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("targets after rebuild = %d (%v), want 1", n, err)
	}
	if _, err := db.Exec(`INSERT INTO services (name, catalog_entry, created_at, updated_at) VALUES ('g', 'proxmox-guest', 'now', 'now')`); err != nil {
		t.Errorf("a service with no destination must be allowed: %v", err)
	}
}

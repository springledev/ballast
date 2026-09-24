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

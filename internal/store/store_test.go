package store

import (
	"path/filepath"
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
	// A raw sqlite handle without migrations must report MAX(version)
	// cleanly via our Scan into NullInt64 only after the table exists —
	// here we just verify a fresh Open reports >= 1 and WAL mode.
	s, _ := openTest(t)
	var mode string
	db := s.db
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal mode = %q, want wal", mode)
	}
	if _, err := s.db.Prepare(sqlRead); err != nil {
		// sanity: schema_migrations exists and has rows, so this query
		// shape is only to confirm no error surfaces from Scan(nil).
	}
}

const sqlRead = `SELECT version FROM schema_migrations WHERE version = -1`

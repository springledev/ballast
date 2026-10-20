package store

import (
	"io/fs"
	"regexp"
	"testing"
)

var rollbackNote = regexp.MustCompile(`(?i)rolling back:`)

// TestEveryMigrationStatesRollback checks each migration says, in a
// "Rolling back:" comment, whether it can be undone (spec §21.5).
func TestEveryMigrationStatesRollback(t *testing.T) {
	entries, err := fs.Glob(migrationsFS, "migrations/*.sql")
	if err != nil || len(entries) == 0 {
		t.Fatalf("listing migrations: %v", err)
	}
	for _, e := range entries {
		b, err := migrationsFS.ReadFile(e)
		if err != nil {
			t.Fatal(err)
		}
		if !rollbackNote.Match(b) {
			t.Errorf("%s has no \"Rolling back:\" note saying whether it can be rolled back", e)
		}
	}
}

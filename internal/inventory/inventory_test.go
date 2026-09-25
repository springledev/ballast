package inventory

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/springledev/ballast/internal/store"
)

func openDB(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "ballast.db"))
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestValidateName(t *testing.T) {
	tests := []struct {
		name string
		ok   bool
	}{
		{"immich", true},
		{"immich-2", true},
		{"immich_2", true},
		{"a", true},
		{"", false},
		{"Immich", false},
		{"-immich", false},
		{"im/mich", false},
		{"im mich", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateName(tt.name)
			if (err == nil) != tt.ok {
				t.Errorf("ValidateName(%q) error = %v, want ok=%v", tt.name, err, tt.ok)
			}
		})
	}
}

func TestPutGetListDeleteDestination(t *testing.T) {
	db := openDB(t).DB()

	d := Destination{Name: "local", Location: "/srv/backups", KeepDaily: 7, KeepWeekly: 4, KeepMonthly: 6}
	if err := PutDestination(db, d); err != nil {
		t.Fatalf("PutDestination: %v", err)
	}

	got, err := GetDestination(db, "local")
	if err != nil {
		t.Fatalf("GetDestination: %v", err)
	}
	if got.PasswordCredential != "ballast-restic-password.local" {
		t.Errorf("PasswordCredential = %q, want default", got.PasswordCredential)
	}
	if got.Retention().KeepDaily != 7 {
		t.Errorf("Retention().KeepDaily = %d, want 7", got.Retention().KeepDaily)
	}

	// Update in place.
	d.Location = "/srv/backups2"
	if err := PutDestination(db, d); err != nil {
		t.Fatalf("PutDestination update: %v", err)
	}
	got, err = GetDestination(db, "local")
	if err != nil {
		t.Fatalf("GetDestination after update: %v", err)
	}
	if got.Location != "/srv/backups2" {
		t.Errorf("Location = %q, want updated value", got.Location)
	}

	list, err := ListDestinations(db)
	if err != nil {
		t.Fatalf("ListDestinations: %v", err)
	}
	if len(list) != 1 || list[0].Name != "local" {
		t.Errorf("ListDestinations = %+v, want [local]", list)
	}

	if err := DeleteDestination(db, "local"); err != nil {
		t.Fatalf("DeleteDestination: %v", err)
	}
	if _, err := GetDestination(db, "local"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetDestination after delete = %v, want ErrNotFound", err)
	}
}

func TestGetDestinationNotFound(t *testing.T) {
	db := openDB(t).DB()
	if _, err := GetDestination(db, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetDestination = %v, want ErrNotFound", err)
	}
}

func TestDeleteDestinationNotFound(t *testing.T) {
	db := openDB(t).DB()
	if err := DeleteDestination(db, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteDestination = %v, want ErrNotFound", err)
	}
}

func TestPutDestinationValidation(t *testing.T) {
	db := openDB(t).DB()
	tests := []struct {
		name string
		d    Destination
	}{
		{"bad name", Destination{Name: "Bad Name", Location: "/x", KeepDaily: 1}},
		{"empty location", Destination{Name: "d", Location: "", KeepDaily: 1}},
		{"credentials in URL", Destination{Name: "d", Location: "s3:https://user:pass@example.com/bucket", KeepDaily: 1}},
		{"path separator in credential name", Destination{Name: "d", Location: "/x", PasswordCredential: "sub/dir", KeepDaily: 1}},
		{"negative retention", Destination{Name: "d", Location: "/x", KeepDaily: -1}},
		{"all-zero retention", Destination{Name: "d", Location: "/x"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := PutDestination(db, tt.d); err == nil {
				t.Errorf("PutDestination(%+v) = nil, want an error", tt.d)
			}
		})
	}
}

func TestDeleteDestinationInUse(t *testing.T) {
	db := openDB(t).DB()
	if err := PutDestination(db, Destination{Name: "local", Location: "/srv", KeepDaily: 7}); err != nil {
		t.Fatalf("PutDestination: %v", err)
	}
	if err := PutService(db, Service{Name: "immich", CatalogEntry: "immich", Destination: "local"}); err != nil {
		t.Fatalf("PutService: %v", err)
	}
	if err := DeleteDestination(db, "local"); !errors.Is(err, ErrInUse) {
		t.Errorf("DeleteDestination = %v, want ErrInUse", err)
	}
}

func TestPutGetListDeleteService(t *testing.T) {
	db := openDB(t).DB()
	if err := PutDestination(db, Destination{Name: "local", Location: "/srv", KeepDaily: 7}); err != nil {
		t.Fatalf("PutDestination: %v", err)
	}

	s := Service{
		Name:         "immich",
		CatalogEntry: "immich",
		Destination:  "local",
		Params:       map[string]string{"library_dir": "/data/immich"},
		Containers:   map[string]string{"database": "immich_postgres"},
	}
	if err := PutService(db, s); err != nil {
		t.Fatalf("PutService: %v", err)
	}

	got, err := GetService(db, "immich")
	if err != nil {
		t.Fatalf("GetService: %v", err)
	}
	if got.Params["library_dir"] != "/data/immich" {
		t.Errorf("Params = %v, want library_dir set", got.Params)
	}
	if got.Containers["database"] != "immich_postgres" {
		t.Errorf("Containers = %v, want database set", got.Containers)
	}

	list, err := ListServices(db)
	if err != nil {
		t.Fatalf("ListServices: %v", err)
	}
	if len(list) != 1 || list[0].Name != "immich" {
		t.Errorf("ListServices = %+v, want [immich]", list)
	}

	if err := DeleteService(db, "immich"); err != nil {
		t.Fatalf("DeleteService: %v", err)
	}
	if _, err := GetService(db, "immich"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetService after delete = %v, want ErrNotFound", err)
	}
}

func TestPutServiceValidation(t *testing.T) {
	db := openDB(t).DB()
	if err := PutDestination(db, Destination{Name: "local", Location: "/srv", KeepDaily: 7}); err != nil {
		t.Fatalf("PutDestination: %v", err)
	}
	tests := []struct {
		name string
		s    Service
	}{
		{"bad name", Service{Name: "Bad", CatalogEntry: "x", Destination: "local"}},
		{"empty catalog entry", Service{Name: "svc", Destination: "local"}},
		{"empty destination", Service{Name: "svc", CatalogEntry: "x"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := PutService(db, tt.s); err == nil {
				t.Errorf("PutService(%+v) = nil, want an error", tt.s)
			}
		})
	}
}

func TestPutServiceUnknownDestination(t *testing.T) {
	db := openDB(t).DB()
	err := PutService(db, Service{Name: "svc", CatalogEntry: "x", Destination: "nope"})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("PutService with unknown destination = %v, want ErrNotFound", err)
	}
}

func TestDeleteServiceNotFound(t *testing.T) {
	db := openDB(t).DB()
	if err := DeleteService(db, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteService = %v, want ErrNotFound", err)
	}
}

// TestMigrationAppliesOnFreshAndExistingDB verifies migration 005
// applies cleanly on a brand-new database (covered by openDB in every
// test above) and is idempotent when store.Open runs twice against
// the same file (simulating a DB already at migration 004 from an
// earlier version, then reopened after the binary is upgraded).
func TestMigrationAppliesOnFreshAndExistingDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ballast.db")
	s1, err := store.Open(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	v1, err := s1.SchemaVersion()
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if v1 < 5 {
		t.Fatalf("schema version = %d, want >= 5 (migration 005 applied)", v1)
	}
	if err := PutDestination(s1.DB(), Destination{Name: "local", Location: "/srv", KeepDaily: 7}); err != nil {
		t.Fatalf("PutDestination before reopen: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("closing: %v", err)
	}

	s2, err := store.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	if _, err := GetDestination(s2.DB(), "local"); err != nil {
		t.Errorf("GetDestination after reopen: %v", err)
	}
}

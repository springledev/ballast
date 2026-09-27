package inventory

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/springledev/ballast/internal/backend"
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

	// ListDestinations is a JSON API response body (spec §9): it must
	// come back as a non-nil empty slice, not nil, or it marshals as
	// `null` instead of `[]` and breaks a caller that assumes an array
	// (the web UI's Destinations/Services pages did exactly this).
	list, err = ListDestinations(db)
	if err != nil {
		t.Fatalf("ListDestinations after delete: %v", err)
	}
	if list == nil {
		t.Error("ListDestinations() = nil, want a non-nil empty slice")
	}
	if len(list) != 0 {
		t.Errorf("ListDestinations() = %+v, want empty", list)
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

	// See the matching ListDestinations assertion: a JSON API response
	// body must never be a nil slice.
	list, err = ListServices(db)
	if err != nil {
		t.Fatalf("ListServices after delete: %v", err)
	}
	if list == nil {
		t.Error("ListServices() = nil, want a non-nil empty slice")
	}
	if len(list) != 0 {
		t.Errorf("ListServices() = %+v, want empty", list)
	}
}

// TestPutServiceSecrets checks that a secret's credential file name
// defaults to "ballast-secret.<service>.<name>" when left empty, that
// an explicitly supplied name round-trips as given, and that a name
// containing a path separator is a loud validation error rather than
// something that could later be joined onto a credential directory
// unsafely.
func TestPutServiceSecrets(t *testing.T) {
	db := openDB(t).DB()
	if err := PutDestination(db, Destination{Name: "local", Location: "/srv", KeepDaily: 7}); err != nil {
		t.Fatalf("PutDestination: %v", err)
	}

	s := Service{
		Name: "mysql", CatalogEntry: "mysql", Destination: "local",
		Secrets: map[string]string{
			"db_password": "",
			"api_key":     "my-custom-cred-file",
		},
	}
	if err := PutService(db, s); err != nil {
		t.Fatalf("PutService: %v", err)
	}

	got, err := GetService(db, "mysql")
	if err != nil {
		t.Fatalf("GetService: %v", err)
	}
	if want := "ballast-secret.mysql.db_password"; got.Secrets["db_password"] != want {
		t.Errorf("Secrets[db_password] = %q, want default %q", got.Secrets["db_password"], want)
	}
	if got.Secrets["api_key"] != "my-custom-cred-file" {
		t.Errorf("Secrets[api_key] = %q, want the supplied name unchanged", got.Secrets["api_key"])
	}

	bad := Service{Name: "mysql2", CatalogEntry: "mysql", Destination: "local", Secrets: map[string]string{"x": "../escape"}}
	if err := PutService(db, bad); err == nil {
		t.Error("PutService(path-separator credential name) = nil error, want error")
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

// TestNormalizeInfersTypeFromLocation: a destination with no Type set
// (the pre-step-17 "--location" shape) is inferred from Location — an
// absolute path becomes local (with Settings.Path filled in), anything
// else becomes a raw custom restic repository string (Settings.URL
// filled in) — so callers that never learned about backend types keep
// working unchanged.
func TestNormalizeInfersTypeFromLocation(t *testing.T) {
	d := Destination{Name: "d", Location: "/srv/backups", KeepDaily: 1}
	if err := d.Normalize(); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if d.Type != backend.Local {
		t.Errorf("Type = %q, want local", d.Type)
	}
	if d.Settings.Path != "/srv/backups" {
		t.Errorf("Settings.Path = %q, want /srv/backups", d.Settings.Path)
	}

	d2 := Destination{Name: "d2", Location: "rest:https://backup.example.com/ballast", KeepDaily: 1}
	if err := d2.Normalize(); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if d2.Type != backend.Custom {
		t.Errorf("Type = %q, want custom", d2.Type)
	}
	if d2.Settings.URL != "rest:https://backup.example.com/ballast" {
		t.Errorf("Settings.URL = %q, want the location", d2.Settings.URL)
	}
}

func TestNormalizeNoTypeNoLocation(t *testing.T) {
	d := Destination{Name: "d", KeepDaily: 1}
	if err := d.Normalize(); err == nil {
		t.Error("Normalize with no type and no location = nil error, want failure")
	}
}

// TestNormalizeGeneratesLocationForTypedDestination: a destination
// created with a Type and Settings (the step-17 shape) gets its
// Location generated by backend.Location, for both an AWS-credential
// backend (b2) and an SSH-credential one (sftp).
func TestNormalizeGeneratesLocationForTypedDestination(t *testing.T) {
	d := Destination{
		Name: "b2dest", Type: backend.B2,
		Settings:  backend.Settings{Endpoint: "s3.us-west-004.backblazeb2.com", Bucket: "photos"},
		KeepDaily: 1,
	}
	if err := d.Normalize(); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if want := "s3:https://s3.us-west-004.backblazeb2.com/photos"; d.Location != want {
		t.Errorf("Location = %q, want %q", d.Location, want)
	}

	sftp := Destination{
		Name: "sftpdest", Type: backend.SFTP,
		Settings:  backend.Settings{Host: "h", User: "u", Path: "/srv/x"},
		KeepDaily: 1,
	}
	if err := sftp.Normalize(); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if want := "sftp://u@h:22//srv/x"; sftp.Location != want {
		t.Errorf("Location = %q, want %q", sftp.Location, want)
	}
}

// TestNormalizeDefaultBackendCredentialNames: a typed destination that
// needs backend credentials gets a default credential file name when
// none is given, matching backend.Type.DefaultCredentialName exactly.
func TestNormalizeDefaultBackendCredentialNames(t *testing.T) {
	tests := []struct {
		name string
		d    Destination
		want string
	}{
		{
			name: "b2 default",
			d: Destination{
				Name: "myb2", Type: backend.B2,
				Settings: backend.Settings{Endpoint: "s3.us-west-004.backblazeb2.com", Bucket: "photos"}, KeepDaily: 1,
			},
			want: "ballast-dest-keys.myb2",
		},
		{
			name: "sftp default",
			d: Destination{
				Name: "myssh", Type: backend.SFTP,
				Settings: backend.Settings{Host: "h", User: "u", Path: "/srv/x"}, KeepDaily: 1,
			},
			want: "ballast-dest-ssh-key.myssh",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := tt.d
			if err := d.Normalize(); err != nil {
				t.Fatalf("Normalize: %v", err)
			}
			if d.BackendCredential != tt.want {
				t.Errorf("BackendCredential = %q, want %q", d.BackendCredential, tt.want)
			}
		})
	}
}

// TestNormalizeRejectsBackendCredentialForLocal: a local destination
// needs no backend credentials, so setting one is a validation error —
// it would otherwise silently be ignored by Target/Resolver.
func TestNormalizeRejectsBackendCredentialForLocal(t *testing.T) {
	d := Destination{Name: "d", Type: backend.Local, Settings: backend.Settings{Path: "/srv"}, BackendCredential: "some-file", KeepDaily: 1}
	if err := d.Normalize(); err == nil {
		t.Error("Normalize with a backend credential on a local destination = nil error, want failure")
	}
}

// TestNormalizeRejectsPathSeparatorsInCredentialNames covers both
// credential-name fields, for a typed destination.
func TestNormalizeRejectsPathSeparatorsInCredentialNames(t *testing.T) {
	d1 := Destination{Name: "d", Type: backend.Local, Settings: backend.Settings{Path: "/srv"}, PasswordCredential: "sub/dir", KeepDaily: 1}
	if err := d1.Normalize(); err == nil {
		t.Error("Normalize with a path separator in PasswordCredential = nil error, want failure")
	}

	d2 := Destination{
		Name: "myb2", Type: backend.B2,
		Settings:          backend.Settings{Endpoint: "s3.us-west-004.backblazeb2.com", Bucket: "photos"},
		BackendCredential: "../escape",
		KeepDaily:         1,
	}
	if err := d2.Normalize(); err == nil {
		t.Error("Normalize with a path separator in BackendCredential = nil error, want failure")
	}
}

// TestNormalizeRejectsSameCredentialNameTwice: the repository password
// and the backend credential must be different files — sharing one
// would mean restic's password file also gets overwritten with (or
// mistaken for) the access-key/SSH-key file.
func TestNormalizeRejectsSameCredentialNameTwice(t *testing.T) {
	d := Destination{
		Name: "myb2", Type: backend.B2,
		Settings:           backend.Settings{Endpoint: "s3.us-west-004.backblazeb2.com", Bucket: "photos"},
		PasswordCredential: "shared-name",
		BackendCredential:  "shared-name",
		KeepDaily:          1,
	}
	if err := d.Normalize(); err == nil {
		t.Error("Normalize with the same name for both credentials = nil error, want failure")
	}
}

// TestPutDestinationRoundTripsBackendFields: Type, Settings and
// BackendCredential all survive a PutDestination/GetDestination and a
// ListDestinations round trip, not just Location.
func TestPutDestinationRoundTripsBackendFields(t *testing.T) {
	db := openDB(t).DB()
	d := Destination{
		Name:      "myb2",
		Type:      backend.B2,
		Settings:  backend.Settings{Endpoint: "s3.us-west-004.backblazeb2.com", Bucket: "photos"},
		KeepDaily: 7,
	}
	if err := PutDestination(db, d); err != nil {
		t.Fatalf("PutDestination: %v", err)
	}

	got, err := GetDestination(db, "myb2")
	if err != nil {
		t.Fatalf("GetDestination: %v", err)
	}
	if got.Type != backend.B2 {
		t.Errorf("Type = %q, want b2", got.Type)
	}
	if got.Settings.Bucket != "photos" || got.Settings.Endpoint != "s3.us-west-004.backblazeb2.com" {
		t.Errorf("Settings = %+v, want the saved bucket/endpoint", got.Settings)
	}
	if got.BackendCredential != "ballast-dest-keys.myb2" {
		t.Errorf("BackendCredential = %q, want the default", got.BackendCredential)
	}
	if want := "s3:https://s3.us-west-004.backblazeb2.com/photos"; got.Location != want {
		t.Errorf("Location = %q, want %q", got.Location, want)
	}

	list, err := ListDestinations(db)
	if err != nil {
		t.Fatalf("ListDestinations: %v", err)
	}
	if len(list) != 1 || list[0].Type != backend.B2 || list[0].Settings.Bucket != "photos" {
		t.Errorf("ListDestinations = %+v, want the b2 destination with its settings", list)
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

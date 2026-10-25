package inventory

import (
	"errors"
	"path/filepath"
	"reflect"
	"strings"
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

// TestServiceProjectRoundTripAndListing covers spec §14 step 23:
// services can be tagged with a project so the full-stack restore
// wizard can find them together, an invalid project name is a loud
// validation error (it is used the same way a service/destination
// name is), and ListServicesByProject/ListProjects only ever surface
// services that actually opted in.
func TestServiceProjectRoundTripAndListing(t *testing.T) {
	db := openDB(t).DB()
	if err := PutDestination(db, Destination{Name: "local", Location: "/srv", KeepDaily: 7}); err != nil {
		t.Fatalf("PutDestination: %v", err)
	}

	if err := PutService(db, Service{Name: "web-config", CatalogEntry: "docker-compose", Destination: "local", Project: "myapp"}); err != nil {
		t.Fatalf("PutService web-config: %v", err)
	}
	if err := PutService(db, Service{Name: "web-db", CatalogEntry: "postgres", Destination: "local", Project: "myapp"}); err != nil {
		t.Fatalf("PutService web-db: %v", err)
	}
	if err := PutService(db, Service{Name: "standalone", CatalogEntry: "folder", Destination: "local"}); err != nil {
		t.Fatalf("PutService standalone: %v", err)
	}

	got, err := GetService(db, "web-config")
	if err != nil {
		t.Fatalf("GetService: %v", err)
	}
	if got.Project != "myapp" {
		t.Errorf("Project = %q, want %q", got.Project, "myapp")
	}

	members, err := ListServicesByProject(db, "myapp")
	if err != nil {
		t.Fatalf("ListServicesByProject: %v", err)
	}
	if len(members) != 2 || members[0].Name != "web-config" || members[1].Name != "web-db" {
		t.Errorf("ListServicesByProject(myapp) = %+v, want [web-config web-db]", members)
	}

	none, err := ListServicesByProject(db, "ghost")
	if err != nil {
		t.Fatalf("ListServicesByProject(ghost): %v", err)
	}
	if len(none) != 0 {
		t.Errorf("ListServicesByProject(ghost) = %+v, want empty", none)
	}

	projects, err := ListProjects(db)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(projects) != 1 || projects[0] != "myapp" {
		t.Errorf("ListProjects = %v, want [myapp] (standalone has no project)", projects)
	}

	if err := PutService(db, Service{Name: "bad-project", CatalogEntry: "folder", Destination: "local", Project: "Not Valid!"}); err == nil {
		t.Error("PutService with an invalid project name = nil error, want error")
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

// TestRetentionValidate checks that Validate rejects negative counts
// and all-zero policies, while accepting valid policies.
func TestRetentionValidate(t *testing.T) {
	tests := []struct {
		name    string
		r       Retention
		wantErr bool
	}{
		{"valid 7/4/6", Retention{KeepDaily: 7, KeepWeekly: 4, KeepMonthly: 6}, false},
		{"valid only monthly", Retention{KeepDaily: 0, KeepWeekly: 0, KeepMonthly: 6}, false},
		{"valid only daily", Retention{KeepDaily: 7, KeepWeekly: 0, KeepMonthly: 0}, false},
		{"valid only weekly", Retention{KeepDaily: 0, KeepWeekly: 4, KeepMonthly: 0}, false},
		{"negative daily", Retention{KeepDaily: -1, KeepWeekly: 4, KeepMonthly: 6}, true},
		{"negative weekly", Retention{KeepDaily: 7, KeepWeekly: -1, KeepMonthly: 6}, true},
		{"negative monthly", Retention{KeepDaily: 7, KeepWeekly: 4, KeepMonthly: -1}, true},
		{"all zero", Retention{KeepDaily: 0, KeepWeekly: 0, KeepMonthly: 0}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.r.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Retention.Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestServiceRetentionRoundTrip checks that service retention overrides
// round-trip through the database correctly.
func TestServiceRetentionRoundTrip(t *testing.T) {
	tests := []struct {
		name            string
		retention       *Retention
		updateTo        *Retention
		wantAfterGet    *Retention
		wantInList      *Retention
		testUpdate      bool // whether to test an update
		wantAfterUpdate *Retention
		wantPutErr      bool
		errMsgSubstr    string
	}{
		{
			name:         "nil override stays nil",
			retention:    nil,
			wantAfterGet: nil,
			wantInList:   nil,
		},
		{
			name:         "override 1/0/3 round-trips",
			retention:    &Retention{KeepDaily: 1, KeepWeekly: 0, KeepMonthly: 3},
			wantAfterGet: &Retention{KeepDaily: 1, KeepWeekly: 0, KeepMonthly: 3},
			wantInList:   &Retention{KeepDaily: 1, KeepWeekly: 0, KeepMonthly: 3},
		},
		{
			name:            "update override to nil clears it",
			retention:       &Retention{KeepDaily: 1, KeepWeekly: 2, KeepMonthly: 3},
			updateTo:        nil,
			wantAfterGet:    &Retention{KeepDaily: 1, KeepWeekly: 2, KeepMonthly: 3},
			wantInList:      &Retention{KeepDaily: 1, KeepWeekly: 2, KeepMonthly: 3},
			testUpdate:      true,
			wantAfterUpdate: nil,
		},
		{
			name:         "invalid override (all zero) rejected",
			retention:    &Retention{KeepDaily: 0, KeepWeekly: 0, KeepMonthly: 0},
			wantPutErr:   true,
			errMsgSubstr: "myservice",
		},
		{
			name:         "invalid override (negative) rejected",
			retention:    &Retention{KeepDaily: -1, KeepWeekly: 0, KeepMonthly: 0},
			wantPutErr:   true,
			errMsgSubstr: "myservice",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := openDB(t).DB()
			if err := PutDestination(db, Destination{Name: "dest", Location: "/srv", KeepDaily: 7}); err != nil {
				t.Fatalf("PutDestination: %v", err)
			}

			// Put service with the retention override.
			s := Service{Name: "myservice", CatalogEntry: "postgres", Destination: "dest", Retention: tt.retention}
			err := PutService(db, s)
			if (err != nil) != tt.wantPutErr {
				t.Errorf("PutService() error = %v, wantErr %v", err, tt.wantPutErr)
			}
			if tt.wantPutErr {
				if tt.errMsgSubstr != "" && !strings.Contains(err.Error(), tt.errMsgSubstr) {
					t.Errorf("PutService() error message %q does not contain %q", err.Error(), tt.errMsgSubstr)
				}
				// Verify nothing was stored when PutService fails.
				if got, err := GetService(db, "myservice"); err == nil {
					t.Errorf("GetService after failed PutService = %+v, want ErrNotFound", got)
				}
				return
			}

			// GetService should return the same retention.
			got, err := GetService(db, "myservice")
			if err != nil {
				t.Fatalf("GetService: %v", err)
			}
			if !retentionEqual(got.Retention, tt.wantAfterGet) {
				t.Errorf("GetService().Retention = %+v, want %+v", got.Retention, tt.wantAfterGet)
			}

			// ListServices should also return the same retention.
			list, err := ListServices(db)
			if err != nil {
				t.Fatalf("ListServices: %v", err)
			}
			if len(list) != 1 {
				t.Fatalf("ListServices() = %d services, want 1", len(list))
			}
			if !retentionEqual(list[0].Retention, tt.wantInList) {
				t.Errorf("ListServices()[0].Retention = %+v, want %+v", list[0].Retention, tt.wantInList)
			}

			// If testUpdate is true, update the service and check again.
			if tt.testUpdate {
				s.Retention = tt.updateTo
				if err := PutService(db, s); err != nil {
					t.Fatalf("PutService update: %v", err)
				}
				got, err := GetService(db, "myservice")
				if err != nil {
					t.Fatalf("GetService after update: %v", err)
				}
				if !retentionEqual(got.Retention, tt.wantAfterUpdate) {
					t.Errorf("GetService after update = %+v, want %+v", got.Retention, tt.wantAfterUpdate)
				}
			}
		})
	}
}

// retentionEqual checks if two Retention pointers are equal.
func retentionEqual(a, b *Retention) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return a.KeepDaily == b.KeepDaily && a.KeepWeekly == b.KeepWeekly && a.KeepMonthly == b.KeepMonthly
}

// TestEffectiveRetention checks that Service.EffectiveRetention returns
// the override when set, otherwise the destination's retention policy.
func TestEffectiveRetention(t *testing.T) {
	tests := []struct {
		name        string
		override    *Retention
		dest        Destination
		wantDaily   int
		wantWeekly  int
		wantMonthly int
	}{
		{
			name:        "nil override uses destination",
			override:    nil,
			dest:        Destination{KeepDaily: 7, KeepWeekly: 4, KeepMonthly: 6},
			wantDaily:   7,
			wantWeekly:  4,
			wantMonthly: 6,
		},
		{
			name:        "override replaces destination",
			override:    &Retention{KeepDaily: 1, KeepWeekly: 2, KeepMonthly: 3},
			dest:        Destination{KeepDaily: 7, KeepWeekly: 4, KeepMonthly: 6},
			wantDaily:   1,
			wantWeekly:  2,
			wantMonthly: 3,
		},
		{
			name:        "override with zeros uses those zeros",
			override:    &Retention{KeepDaily: 0, KeepWeekly: 0, KeepMonthly: 5},
			dest:        Destination{KeepDaily: 7, KeepWeekly: 4, KeepMonthly: 6},
			wantDaily:   0,
			wantWeekly:  0,
			wantMonthly: 5,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := Service{Retention: tt.override}
			got := s.EffectiveRetention(tt.dest)
			if got.KeepDaily != tt.wantDaily || got.KeepWeekly != tt.wantWeekly || got.KeepMonthly != tt.wantMonthly {
				t.Errorf("EffectiveRetention() = {%d, %d, %d}, want {%d, %d, %d}",
					got.KeepDaily, got.KeepWeekly, got.KeepMonthly,
					tt.wantDaily, tt.wantWeekly, tt.wantMonthly)
			}
		})
	}
}

// TestRetentionPolicy checks that Retention.Policy converts the
// retention fields to a restic.RetentionPolicy with other fields zero.
func TestRetentionPolicy(t *testing.T) {
	tests := []struct {
		name      string
		r         Retention
		wantDay   int
		wantWeek  int
		wantMonth int
	}{
		{
			name:      "standard 7/4/6",
			r:         Retention{KeepDaily: 7, KeepWeekly: 4, KeepMonthly: 6},
			wantDay:   7,
			wantWeek:  4,
			wantMonth: 6,
		},
		{
			name:      "only monthly",
			r:         Retention{KeepDaily: 0, KeepWeekly: 0, KeepMonthly: 12},
			wantDay:   0,
			wantWeek:  0,
			wantMonth: 12,
		},
		{
			name:      "custom 2/3/4",
			r:         Retention{KeepDaily: 2, KeepWeekly: 3, KeepMonthly: 4},
			wantDay:   2,
			wantWeek:  3,
			wantMonth: 4,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.r.Policy()
			if got.KeepDaily != tt.wantDay || got.KeepWeekly != tt.wantWeek || got.KeepMonthly != tt.wantMonth {
				t.Errorf("Policy() = {Daily: %d, Weekly: %d, Monthly: %d}, want {Daily: %d, Weekly: %d, Monthly: %d}",
					got.KeepDaily, got.KeepWeekly, got.KeepMonthly,
					tt.wantDay, tt.wantWeek, tt.wantMonth)
			}
			// Verify other fields are zero (should be if correctly mapping).
			if got.KeepLast != 0 || got.KeepHourly != 0 || got.KeepYearly != 0 {
				t.Errorf("Policy() has non-zero other fields: KeepLast=%d, KeepHourly=%d, KeepYearly=%d",
					got.KeepLast, got.KeepHourly, got.KeepYearly)
			}
		})
	}
}

func TestEnvRef(t *testing.T) {
	ref := EnvRef("/opt/a#b/.env", "DB_PASSWORD")
	path, key, ok := ParseEnvRef(ref)
	if !ok || path != "/opt/a#b/.env" || key != "DB_PASSWORD" {
		t.Fatalf("ParseEnvRef = %q %q %v", path, key, ok)
	}
	if _, _, ok := ParseEnvRef("ballast-secret.x.y"); ok {
		t.Error("plain credential name parsed as env ref")
	}
	for _, bad := range []string{EnvRef("rel/.env", "K"), EnvRef("/a/.env", ""), EnvRef("/a/.env", "A B")} {
		if _, err := defaultedSecretNames("svc", map[string]string{"x": bad}); err == nil {
			t.Errorf("defaultedSecretNames accepted %q", bad)
		}
	}
	got, err := defaultedSecretNames("svc", map[string]string{"x": ref, "y": ""})
	if err != nil || got["x"] != ref || got["y"] != "ballast-secret.svc.y" {
		t.Fatalf("defaultedSecretNames = %v, %v", got, err)
	}
}

// TestKeepLastAndExcludesRoundTrip: keep_last on a destination and
// skip patterns on a service survive the database, and bad patterns are
// refused.
func TestKeepLastAndExcludesRoundTrip(t *testing.T) {
	db := openDB(t).DB()
	if err := PutDestination(db, Destination{Name: "ssd", Location: "/srv", KeepLast: 2}); err != nil {
		t.Fatalf("PutDestination (keep-last only): %v", err)
	}
	d, err := GetDestination(db, "ssd")
	if err != nil || d.KeepLast != 2 || d.Retention().KeepLast != 2 {
		t.Fatalf("destination = %+v, %v; want keep_last 2", d, err)
	}
	if err := PutDestination(db, Destination{Name: "none", Location: "/srv2"}); err == nil {
		t.Error("a policy that keeps nothing must still be refused")
	}
	if err := PutDestination(db, Destination{Name: "neg", Location: "/srv3", KeepLast: -1, KeepDaily: 1}); err == nil {
		t.Error("a negative keep_last must be refused")
	}

	s := Service{Name: "docs", CatalogEntry: "folder", Destination: "ssd", Excludes: []string{"*.log", "node_modules"},
		Retention: &Retention{KeepLast: 3}}
	if err := PutService(db, s); err != nil {
		t.Fatalf("PutService: %v", err)
	}
	got, err := GetService(db, "docs")
	if err != nil {
		t.Fatalf("GetService: %v", err)
	}
	if !reflect.DeepEqual(got.Excludes, s.Excludes) || got.Retention == nil || got.Retention.KeepLast != 3 {
		t.Errorf("service = %+v", got)
	}
	got.Excludes = nil
	if err := PutService(db, got); err != nil {
		t.Fatal(err)
	}
	if again, _ := GetService(db, "docs"); len(again.Excludes) != 0 {
		t.Errorf("clearing excludes failed: %+v", again.Excludes)
	}
	s.Excludes = []string{"!x"}
	if err := PutService(db, s); err == nil {
		t.Error("a bad skip pattern must be refused")
	}
}

func TestServiceTargets(t *testing.T) {
	keep := &Retention{KeepDaily: 2}
	tests := []struct {
		name    string
		targets []Target
		wantErr string
		want    []Target
	}{
		{name: "none given means one target with all parts", want: []Target{{Destination: "ssd"}}},
		{
			name:    "two targets keep their parts and retention",
			targets: []Target{{Destination: "ssd", Retention: keep}, {Destination: "b2", Parts: []string{"library"}}},
			want:    []Target{{Destination: "ssd", Retention: keep}, {Destination: "b2", Parts: []string{"library"}}},
		},
		{name: "same destination twice", targets: []Target{{Destination: "ssd"}, {Destination: "ssd"}}, wantErr: "listed twice"},
		{name: "unknown destination", targets: []Target{{Destination: "ssd"}, {Destination: "nope"}}, wantErr: "nope"},
		{name: "empty part name", targets: []Target{{Destination: "ssd", Parts: []string{" "}}}, wantErr: "part name is empty"},
		{name: "keeps nothing", targets: []Target{{Destination: "ssd"}, {Destination: "b2", Retention: &Retention{}}}, wantErr: "target b2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := openDB(t).DB()
			for _, n := range []string{"ssd", "b2"} {
				if err := PutDestination(db, Destination{Name: n, Location: "/srv/" + n, KeepDaily: 7}); err != nil {
					t.Fatal(err)
				}
			}
			s := Service{Name: "immich", CatalogEntry: "immich", Destination: "ssd", Targets: tt.targets}
			err := PutService(db, s)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("PutService error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("PutService: %v", err)
			}
			got, err := GetService(db, "immich")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Targets, tt.want) {
				t.Errorf("Targets = %+v, want %+v", got.Targets, tt.want)
			}
			if got.Destination != tt.want[0].Destination {
				t.Errorf("Destination = %q, want the first target's %q", got.Destination, tt.want[0].Destination)
			}
			list, err := ListServices(db)
			if err != nil || len(list) != 1 || !reflect.DeepEqual(list[0].Targets, tt.want) {
				t.Errorf("ListServices targets = %+v (err %v), want %+v", list, err, tt.want)
			}
		})
	}
}

func TestDeleteDestinationInUseByASecondaryTarget(t *testing.T) {
	db := openDB(t).DB()
	for _, n := range []string{"ssd", "b2"} {
		if err := PutDestination(db, Destination{Name: n, Location: "/srv/" + n, KeepDaily: 7}); err != nil {
			t.Fatal(err)
		}
	}
	s := Service{Name: "immich", CatalogEntry: "immich", Destination: "ssd", Targets: []Target{{Destination: "ssd"}, {Destination: "b2"}}}
	if err := PutService(db, s); err != nil {
		t.Fatal(err)
	}
	if err := DeleteDestination(db, "b2"); !errors.Is(err, ErrInUse) {
		t.Errorf("DeleteDestination(b2) = %v, want ErrInUse", err)
	}
	if err := DeleteService(db, "immich"); err != nil {
		t.Fatal(err)
	}
	if err := DeleteDestination(db, "b2"); err != nil {
		t.Errorf("DeleteDestination after the service is gone: %v", err)
	}
}

func TestTargetWants(t *testing.T) {
	tests := []struct {
		parts []string
		part  string
		want  bool
	}{
		{nil, "library", true},
		{[]string{"database"}, "library", false},
		{[]string{"database", "library"}, "library", true},
	}
	for _, tt := range tests {
		if got := (Target{Parts: tt.parts}).Wants(tt.part); got != tt.want {
			t.Errorf("Target{Parts: %v}.Wants(%q) = %v, want %v", tt.parts, tt.part, got, tt.want)
		}
	}
}

// TestProtonDriveDestination: a Proton Drive copy has no repository
// password, defaults to keeping the newest 7 run folders, and can only
// ever be an "also send to" target of a service.
func TestProtonDriveDestination(t *testing.T) {
	db := openDB(t).DB()
	if err := PutDestination(db, Destination{Name: "local", Location: "/srv/backups", KeepDaily: 7}); err != nil {
		t.Fatal(err)
	}
	if err := PutDestination(db, Destination{Name: "proton", Type: backend.ProtonDrive}); err != nil {
		t.Fatalf("PutDestination: %v", err)
	}
	got, err := GetDestination(db, "proton")
	if err != nil {
		t.Fatal(err)
	}
	if got.PasswordCredential != "" || got.BackendCredential != "" || got.KeepLast != DefaultCopyKeepLast || got.Location != "proton-drive:/my-files/ballast" {
		t.Errorf("proton destination = %+v", got)
	}
	if err := PutDestination(db, Destination{Name: "p2", Type: backend.ProtonDrive, PasswordCredential: "x"}); err == nil || !strings.Contains(err.Error(), "no repository password") {
		t.Errorf("a password on a Proton Drive copy: err = %v", err)
	}
	tests := []struct {
		name    string
		svc     Service
		wantErr string
	}{
		{"as the main destination", Service{Name: "a", CatalogEntry: "x", Destination: "proton"}, "also send to"},
		{"as an extra target", Service{Name: "b", CatalogEntry: "x", Destination: "local", Targets: []Target{{Destination: "local"}, {Destination: "proton"}}}, ""},
		{"on a guest job", Service{Name: "c", Kind: KindProxmoxGuest, ProxmoxNode: "pve", ProxmoxVMID: 100, Guest: &GuestJob{Storage: "backup", CopyPath: t.TempDir()}, Destination: "local", Targets: []Target{{Destination: "local"}, {Destination: "proton"}}}, "too large"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := PutService(db, tt.svc)
			if tt.wantErr == "" && err != nil || tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Errorf("PutService err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

// TestDeleteServiceClearsNameKeyedState: schedule, trigger token and
// alert bookkeeping are keyed by service name with no foreign key, so
// removing a service must remove them too; otherwise a new service (or
// Ballast's own settings backup) with the same name would inherit them.
func TestDeleteServiceClearsNameKeyedState(t *testing.T) {
	db := openDB(t).DB()
	if err := PutDestination(db, Destination{Name: "local", Location: "/srv/backups", KeepDaily: 7}); err != nil {
		t.Fatal(err)
	}
	if err := PutService(db, Service{Name: "vault", CatalogEntry: "folder", Destination: "local", Params: map[string]string{"path": "/x"}}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO schedules (service, cron_expr, enabled, updated_at) VALUES ('vault', '* * * * *', 1, 'x')`,
		`INSERT INTO trigger_tokens (service, token_hash, created_at) VALUES ('vault', 'h', 'x')`,
		`INSERT INTO alert_state (service, last_alert_at) VALUES ('vault', 'x')`,
		`INSERT INTO alert_decisions (service, at, type, outcome) VALUES ('vault', 'x', 'failure', 'sent')`,
		`INSERT INTO freshness_alerts (service, last_checked_at, is_stale) VALUES ('vault', 'x', 1)`,
		`INSERT INTO notification_settings (service, enabled, updated_at) VALUES ('vault', 0, 'x')`,
		// Another service's rows must stay.
		`INSERT INTO schedules (service, cron_expr, enabled, updated_at) VALUES ('other', '* * * * *', 1, 'x')`,
		`INSERT INTO alert_state (service, last_alert_at) VALUES ('other', 'x')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := DeleteService(db, "vault"); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"schedules", "trigger_tokens", "alert_state", "alert_decisions", "freshness_alerts", "notification_settings"} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table + ` WHERE service = 'vault'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%s still has %d row(s) for the removed service", table, n)
		}
	}
	var n int
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM schedules WHERE service='other') + (SELECT COUNT(*) FROM alert_state WHERE service='other')`).Scan(&n); err != nil || n != 2 {
		t.Errorf("another service's rows = %d, %v; want untouched (2)", n, err)
	}
}

// TestDeleteServiceClearsChannelLinks: the links from a service to its
// notification channels are keyed by name only, so removing the service
// must remove them; a re-added service must not inherit them.
func TestDeleteServiceClearsChannelLinks(t *testing.T) {
	db := openDB(t).DB()
	if err := PutDestination(db, Destination{Name: "local", Location: "/srv/backups", KeepDaily: 7}); err != nil {
		t.Fatal(err)
	}
	if err := PutService(db, Service{Name: "vault", CatalogEntry: "folder", Destination: "local", Params: map[string]string{"path": "/x"}}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO notification_channels (id, name, type, url_credential, created_at, updated_at) VALUES (1, 'c', 'ntfy', 'u', 'x', 'x')`,
		`INSERT INTO service_channels (service, channel_id) VALUES ('vault', 1)`,
		`INSERT INTO service_channels (service, channel_id) VALUES ('other', 1)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := DeleteService(db, "vault"); err != nil {
		t.Fatal(err)
	}
	var gone, kept int
	_ = db.QueryRow(`SELECT COUNT(*) FROM service_channels WHERE service = 'vault'`).Scan(&gone)
	_ = db.QueryRow(`SELECT COUNT(*) FROM service_channels WHERE service = 'other'`).Scan(&kept)
	if gone != 0 || kept != 1 {
		t.Errorf("service_channels rows: removed service %d (want 0), other service %d (want 1)", gone, kept)
	}
}

func TestServiceNames(t *testing.T) {
	db := openDB(t).DB()
	if got, err := ServiceNames(db); err != nil || len(got) != 0 {
		t.Fatalf("empty ServiceNames = %v, %v", got, err)
	}
	if err := PutDestination(db, Destination{Name: "local", Location: "/srv/backups", KeepDaily: 7}); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"b", "a"} {
		if err := PutService(db, Service{Name: n, CatalogEntry: "folder", Destination: "local", Params: map[string]string{"path": "/x"}}); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := ServiceNames(db); err != nil || !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("ServiceNames = %v, %v; want [a b]", got, err)
	}
}

func TestBackupCount(t *testing.T) {
	tests := []struct {
		n        int
		wantWhat string
		wantThem string
	}{
		{1, "1 backup", "it"},
		{2, "2 backups", "them"},
		{45, "45 backups", "them"},
	}
	for _, tt := range tests {
		what, them := backupCount(tt.n)
		if what != tt.wantWhat || them != tt.wantThem {
			t.Errorf("backupCount(%d) = %q, %q; want %q, %q", tt.n, what, them, tt.wantWhat, tt.wantThem)
		}
	}
}

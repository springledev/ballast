package inventory

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/springledev/keel/dockerhost"
)

func TestDockerHostCRUD(t *testing.T) {
	db := openDB(t).DB()

	// Migration 008 seeds the built-in local host.
	hosts, err := ListDockerHosts(db)
	if err != nil {
		t.Fatal(err)
	}
	if want := []dockerhost.Host{{Name: "local", Kind: dockerhost.Local}}; !reflect.DeepEqual(hosts, want) {
		t.Fatalf("fresh hosts = %+v, want %+v", hosts, want)
	}

	saved, err := PutDockerHost(db, dockerhost.Host{Name: "nas", Kind: dockerhost.RemoteSSH, Address: "ballast@nas", Credential: "ballast-dockerhost.nas"})
	if err != nil {
		t.Fatal(err)
	}
	if saved.Address != "ssh://ballast@nas" {
		t.Errorf("saved address = %q, want the normalized form", saved.Address)
	}
	if _, err := PutDockerHost(db, dockerhost.Host{Name: "a-tls", Kind: dockerhost.RemoteTLS, Address: "media", Credential: "c"}); err != nil {
		t.Fatal(err)
	}
	got, err := GetDockerHost(db, "nas")
	if err != nil || got != saved {
		t.Fatalf("GetDockerHost = %+v, %v; want %+v", got, err, saved)
	}
	hosts, _ = ListDockerHosts(db)
	var names []string
	for _, h := range hosts {
		names = append(names, h.Name)
	}
	if want := []string{"local", "a-tls", "nas"}; !reflect.DeepEqual(names, want) {
		t.Errorf("list order = %v, want local first then by name %v", names, want)
	}

	// Update in place.
	if _, err := PutDockerHost(db, dockerhost.Host{Name: "nas", Kind: dockerhost.RemoteSSH, Address: "ssh://root@nas:2222"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := GetDockerHost(db, "nas"); got.Address != "ssh://root@nas:2222" || got.Credential != "" {
		t.Errorf("updated host = %+v", got)
	}

	if err := DeleteDockerHost(db, "nas"); err != nil {
		t.Fatal(err)
	}
	if _, err := GetDockerHost(db, "nas"); !errors.Is(err, ErrNotFound) {
		t.Errorf("after delete: %v, want ErrNotFound", err)
	}
	if err := DeleteDockerHost(db, "nas"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete: %v, want ErrNotFound", err)
	}
}

func TestPutDockerHostValidation(t *testing.T) {
	db := openDB(t).DB()
	tests := []struct {
		name    string
		host    dockerhost.Host
		wantErr string
	}{
		{"bad name", dockerhost.Host{Name: "Bad Name", Kind: dockerhost.Local}, "can't be used as a name"},
		{"local turned remote", dockerhost.Host{Name: "local", Kind: dockerhost.RemoteSSH, Address: "a@b"}, "must stay a local host"},
		{"bad address", dockerhost.Host{Name: "x", Kind: dockerhost.RemoteSSH, Address: "nouser"}, "include the user"},
		{"bad kind", dockerhost.Host{Name: "x", Kind: "vm"}, "unknown kind"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := PutDockerHost(db, tt.host); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
	// The local host can point at another socket.
	if _, err := PutDockerHost(db, dockerhost.Host{Name: "local", Kind: dockerhost.Local, Address: "/run/user/1000/docker.sock"}); err != nil {
		t.Errorf("repointing local: %v", err)
	}
}

func TestServiceDockerHost(t *testing.T) {
	db := openDB(t).DB()
	if err := PutDestination(db, Destination{Name: "d", Location: "/srv", KeepDaily: 7}); err != nil {
		t.Fatal(err)
	}
	// Empty means the built-in local host.
	if err := PutService(db, Service{Name: "a", CatalogEntry: "folder", Destination: "d"}); err != nil {
		t.Fatal(err)
	}
	if s, _ := GetService(db, "a"); s.DockerHost != "local" {
		t.Errorf("default docker host = %q, want local", s.DockerHost)
	}
	if err := PutService(db, Service{Name: "b", CatalogEntry: "folder", Destination: "d", DockerHost: "nas"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("service on an unknown host: %v, want ErrNotFound", err)
	}
	if _, err := PutDockerHost(db, dockerhost.Host{Name: "nas", Kind: dockerhost.RemoteSSH, Address: "me@nas"}); err != nil {
		t.Fatal(err)
	}
	if err := PutService(db, Service{Name: "b", CatalogEntry: "folder", Destination: "d", DockerHost: "nas"}); err != nil {
		t.Fatal(err)
	}
	list, _ := ListServices(db)
	if len(list) != 2 || list[1].DockerHost != "nas" {
		t.Errorf("ListServices = %+v, want b on nas", list)
	}
	if err := DeleteDockerHost(db, "nas"); !errors.Is(err, ErrInUse) {
		t.Errorf("deleting a host in use: %v, want ErrInUse", err)
	}
	if err := DeleteDockerHost(db, "local"); err == nil || !strings.Contains(err.Error(), "can't be removed") {
		t.Errorf("deleting local: %v", err)
	}
}

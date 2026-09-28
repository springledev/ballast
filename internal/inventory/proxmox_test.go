package inventory

import (
	"errors"
	"testing"

	"github.com/springledev/ballast/internal/proxmox"
)

func TestProxmoxConnectionCRUD(t *testing.T) {
	db := openDB(t).DB()

	if _, err := GetProxmoxConnection(db); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetProxmoxConnection before any save = %v, want ErrNotFound", err)
	}

	conn := proxmox.Connection{Host: "https://pve.local:8006", TokenID: "ballast@pve!backup", Credential: "ballast-proxmox-token"}
	if err := PutProxmoxConnection(db, conn); err != nil {
		t.Fatal(err)
	}
	got, err := GetProxmoxConnection(db)
	if err != nil {
		t.Fatal(err)
	}
	if got != conn {
		t.Fatalf("GetProxmoxConnection = %+v, want %+v", got, conn)
	}

	// Saving again replaces the singleton row rather than erroring.
	conn.Insecure = true
	conn.Host = "https://pve2.local:8006"
	if err := PutProxmoxConnection(db, conn); err != nil {
		t.Fatal(err)
	}
	got, err = GetProxmoxConnection(db)
	if err != nil {
		t.Fatal(err)
	}
	if got != conn {
		t.Fatalf("GetProxmoxConnection after update = %+v, want %+v", got, conn)
	}

	if err := DeleteProxmoxConnection(db); err != nil {
		t.Fatal(err)
	}
	if _, err := GetProxmoxConnection(db); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetProxmoxConnection after delete = %v, want ErrNotFound", err)
	}
	if err := DeleteProxmoxConnection(db); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteProxmoxConnection on empty = %v, want ErrNotFound", err)
	}
}

func TestServiceProxmoxGuestAttachment(t *testing.T) {
	db := openDB(t).DB()
	if err := PutDestination(db, Destination{Name: "d", Location: t.TempDir(), PasswordCredential: "pw", KeepDaily: 7}); err != nil {
		t.Fatal(err)
	}

	// Node and VMID must be given together.
	err := PutService(db, Service{Name: "s1", CatalogEntry: "folder", Destination: "d", ProxmoxNode: "pve1"})
	if err == nil {
		t.Fatal("expected an error when proxmox_node is set without proxmox_vmid")
	}
	err = PutService(db, Service{Name: "s1", CatalogEntry: "folder", Destination: "d", ProxmoxVMID: 100})
	if err == nil {
		t.Fatal("expected an error when proxmox_vmid is set without proxmox_node")
	}

	if err := PutService(db, Service{Name: "s1", CatalogEntry: "folder", Destination: "d", ProxmoxNode: "pve1", ProxmoxVMID: 100}); err != nil {
		t.Fatal(err)
	}
	got, err := GetService(db, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ProxmoxNode != "pve1" || got.ProxmoxVMID != 100 {
		t.Fatalf("got = %+v, want proxmox_node=pve1 proxmox_vmid=100", got)
	}

	// A service with no Proxmox attachment (the default/common case).
	if err := PutService(db, Service{Name: "s2", CatalogEntry: "folder", Destination: "d"}); err != nil {
		t.Fatal(err)
	}
	got, err = GetService(db, "s2")
	if err != nil {
		t.Fatal(err)
	}
	if got.ProxmoxNode != "" || got.ProxmoxVMID != 0 {
		t.Fatalf("got = %+v, want no proxmox attachment", got)
	}
}

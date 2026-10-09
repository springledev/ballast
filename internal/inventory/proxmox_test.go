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

func TestGuestJobServiceRoundTrip(t *testing.T) {
	db := openDB(t).DB()
	s := Service{Name: "pve-ct100", Kind: KindProxmoxGuest, ProxmoxNode: "pve", ProxmoxVMID: 100,
		Guest: &GuestJob{Storage: "backup", Mode: "stop", KeepLast: 2}}
	if err := PutService(db, s); err != nil {
		t.Fatalf("PutService: %v", err)
	}
	got, err := GetService(db, "pve-ct100")
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsGuestJob() || got.Guest == nil || got.Guest.KeepLast != 2 || got.Guest.Mode != "stop" || got.Destination != "" || len(got.EffectiveTargets()) != 0 {
		t.Errorf("round trip = %+v", got)
	}
	bad := s
	bad.Guest = &GuestJob{}
	if err := PutService(db, bad); err == nil {
		t.Error("a guest job without a storage must be refused")
	}
	if err := RecordProxmoxDump(db, "pve-ct100", "pve", "backup", "backup:backup/x", 1, 10); err != nil {
		t.Fatal(err)
	}
	owned, _ := OwnedProxmoxDumps(db, "pve-ct100", "pve", "backup")
	if len(owned) != 1 {
		t.Fatalf("owned = %v", owned)
	}
	_ = MarkProxmoxDumpsPruned(db, "pve", owned)
	if owned, _ = OwnedProxmoxDumps(db, "pve-ct100", "pve", "backup"); len(owned) != 0 {
		t.Errorf("pruned dump still owned: %v", owned)
	}
}

func TestPutConfigSnapshotService(t *testing.T) {
	tests := []struct {
		name    string
		dest    string
		wantErr bool
	}{
		{name: "stored in a destination", dest: "d"},
		{name: "needs a destination", dest: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := openDB(t).DB()
			if err := PutDestination(db, Destination{Name: "d", Location: t.TempDir(), PasswordCredential: "pw", KeepDaily: 7}); err != nil {
				t.Fatal(err)
			}
			err := PutService(db, Service{Name: "pve-config", Kind: KindProxmoxConfig, Destination: tt.dest})
			if (err != nil) != tt.wantErr {
				t.Fatalf("PutService err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			got, err := GetService(db, "pve-config")
			if err != nil || !got.IsConfigSnapshot() || got.CatalogEntry != KindProxmoxConfig {
				t.Fatalf("GetService = %+v, %v", got, err)
			}
		})
	}
}

func TestTargetSizeAndLastDumpSize(t *testing.T) {
	db := openDB(t).DB()
	if n, err := TargetSize(db, "immich", "usb"); err != nil || n != 0 {
		t.Fatalf("TargetSize before any run = %d, %v; want 0", n, err)
	}
	for _, size := range []int64{100, 250} {
		if err := RecordTargetSize(db, "immich", "usb", size); err != nil {
			t.Fatal(err)
		}
	}
	if n, _ := TargetSize(db, "immich", "usb"); n != 250 {
		t.Errorf("TargetSize = %d, want the latest (250)", n)
	}
	if n, err := LastProxmoxDumpSize(db, "vm1"); err != nil || n != 0 {
		t.Errorf("LastProxmoxDumpSize with no dumps = %d, %v; want 0", n, err)
	}
	if err := RecordProxmoxDump(db, "vm1", "pve", "bk", "v1", 1, 500); err != nil {
		t.Fatal(err)
	}
	if n, _ := LastProxmoxDumpSize(db, "vm1"); n != 500 {
		t.Errorf("LastProxmoxDumpSize = %d, want 500", n)
	}
	if n, _ := OwnedProxmoxDumpBytes(db, "vm1", "pve", "bk"); n != 500 {
		t.Errorf("OwnedProxmoxDumpBytes = %d, want 500", n)
	}
}

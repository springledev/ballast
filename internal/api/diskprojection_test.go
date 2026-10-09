package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/springledev/ballast/internal/backend"
	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/proxmox"
)

func TestDiskProjectionWarn(t *testing.T) {
	const gib = int64(1 << 30)
	tests := []struct {
		name string
		p    DiskProjection
		warn bool
	}{
		{"roomy", DiskProjection{Total: 100 * gib, Used: 20 * gib, Existing: 5 * gib, Planned: 20 * gib}, false},
		{"over 80 after retention", DiskProjection{Total: 100 * gib, Used: 60 * gib, Existing: 10 * gib, Planned: 40 * gib}, true},
		{"existing backups not double counted", DiskProjection{Total: 100 * gib, Used: 70 * gib, Existing: 60 * gib, Planned: 60 * gib}, false},
		{"unknown size", DiskProjection{Planned: gib}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.p.Warn(); got != tt.warn {
				t.Errorf("Warn() = %v (%.2f), want %v", got, tt.p.Fraction(), tt.warn)
			}
		})
	}
}

func TestKeptCount(t *testing.T) {
	if got := keptCount(inventory.Retention{}); got != 1 {
		t.Errorf("empty = %d, want 1", got)
	}
	if got := keptCount(inventory.Retention{KeepLast: 2, KeepDaily: 7, KeepWeekly: 4, KeepMonthly: 6}); got != 19 {
		t.Errorf("full = %d, want 19", got)
	}
}

func TestProxmoxAccessAdvice(t *testing.T) {
	tests := []struct {
		name   string
		ac     proxmox.AccessCheck
		status DoctorStatus
		want   string
	}{
		{"no guests", proxmox.AccessCheck{}, DoctorFail, "PVEAuditor"},
		{"list error", proxmox.AccessCheck{GuestsErr: errors.New("boom")}, DoctorFail, "/vms"},
		{"storage denied", proxmox.AccessCheck{Guests: 2, StorageName: "bk", StorageErr: errors.New("403")}, DoctorWarn, "/storage/bk"},
		{"all fine", proxmox.AccessCheck{Guests: 2, StorageName: "bk", StorageRead: true}, DoctorOK, "2 guest"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, msg := proxmoxAccessAdvice(tt.ac)
			if st != tt.status || !strings.Contains(msg, tt.want) {
				t.Errorf("got %s %q, want %s containing %q", st, msg, tt.status, tt.want)
			}
		})
	}
}

func TestLocalDiskProjectionsInDoctorAndPreview(t *testing.T) {
	h := newHarness(t)
	h.addDestination("usb")
	repo := t.TempDir()
	d, _ := inventory.GetDestination(h.ops.DB, "usb")
	d.Type, d.Settings = backend.Local, backend.Settings{Path: repo}
	d.Location, d.KeepLast, d.KeepDaily, d.KeepWeekly, d.KeepMonthly = repo, 0, 7, 4, 6
	if err := inventory.PutDestination(h.ops.DB, d); err != nil {
		t.Fatal(err)
	}
	if err := inventory.PutService(h.ops.DB, inventory.Service{Name: "svc", CatalogEntry: "folder", Destination: "usb", Params: map[string]string{"path": "/x"}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "data"), make([]byte, 1024), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// No recorded size yet: no projection, no noise.
	if projs, err := h.ops.LocalDiskProjections(ctx); err != nil || len(projs) != 0 {
		t.Fatalf("projections before any run = %v, %v; want none", projs, err)
	}
	// A last backup so large that 17 kept copies overflow the disk.
	if err := inventory.RecordTargetSize(h.ops.DB, "svc", "usb", 1<<50); err != nil {
		t.Fatal(err)
	}
	projs, err := h.ops.LocalDiskProjections(ctx)
	if err != nil || len(projs) != 1 || !projs[0].Warn() {
		t.Fatalf("projections = %+v, %v; want one warning", projs, err)
	}
	var sawWarn bool
	h.ops.diskProjectionDoctor(ctx, func(area, name string, st DoctorStatus, detail string) {
		sawWarn = sawWarn || (area == "disk" && st == DoctorWarn && strings.Contains(detail, "svc"))
	})
	if !sawWarn {
		t.Error("doctor did not warn about the projected disk use")
	}
	pv, err := h.ops.Preview(ctx, "svc")
	if err != nil {
		t.Fatal(err)
	}
	var noted bool
	for _, n := range pv.Notes {
		noted = noted || strings.Contains(n, "full once backups reach their full retention")
	}
	if !noted {
		t.Errorf("preview notes = %v, want a disk warning", pv.Notes)
	}
}

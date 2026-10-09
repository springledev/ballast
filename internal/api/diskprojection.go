package api

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/springledev/ballast/internal/backend"
	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/proxmox"
)

// DiskProjection is how full one destination disk (or Proxmox storage)
// is expected to get once every service has the number of backups its
// retention keeps (last backup size x backups kept).
type DiskProjection struct {
	// Name is the folder or "node/storage" the projection is for.
	Name string
	// Total and Used are the disk's size and current use in bytes.
	Total, Used int64
	// Existing is what Ballast's own backups use there now; Planned is
	// what they are expected to use once retention is full.
	Existing, Planned int64
	// Services are the services that write there, sorted.
	Services []string
}

// Fraction is the projected share of the disk in use (0..1+).
func (p DiskProjection) Fraction() float64 {
	return proxmox.ProjectUse(p.Total, p.Used-p.Existing, p.Planned)
}

// Warn reports whether the projection passes the warning line.
func (p DiskProjection) Warn() bool { return p.Total > 0 && p.Fraction() > proxmox.WarnUseFraction }

// Detail is the plain-English doctor line for the projection.
func (p DiskProjection) Detail() string {
	pct := p.Fraction() * 100
	base := fmt.Sprintf("%.0f%% of the disk is expected to be used once backups are at their full retention (%s of %s; Ballast backups for %s would take about %s)",
		pct, proxmox.FormatBytes(p.Used-p.Existing+p.Planned), proxmox.FormatBytes(p.Total), strings.Join(p.Services, ", "), proxmox.FormatBytes(p.Planned))
	if p.Warn() {
		return base + ". Free up space, keep fewer backups, or send some services to a different disk before it fills."
	}
	return base
}

// keptCount is how many backups a retention policy keeps at most (at
// least 1).
func keptCount(r inventory.Retention) int64 {
	n := int64(r.KeepLast + r.KeepDaily + r.KeepWeekly + r.KeepMonthly)
	if n < 1 {
		return 1
	}
	return n
}

// diskKey identifies the filesystem holding path (its nearest existing
// parent), so two folders on one disk are added together.
func diskKey(path string) (probe string, dev uint64, st syscall.Statfs_t, ok bool) {
	probe = path
	for {
		if _, err := os.Stat(probe); err == nil {
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			break
		}
		probe = parent
	}
	var s syscall.Stat_t
	if syscall.Stat(probe, &s) != nil || syscall.Statfs(probe, &st) != nil {
		return probe, 0, st, false
	}
	return probe, uint64(s.Dev), st, true
}

// dirBytes adds up the file sizes under dir; unreadable parts count 0.
func dirBytes(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, ierr := d.Info(); ierr == nil {
			n += info.Size()
		}
		return nil
	})
	return n
}

// LocalDiskProjections works out, for every disk holding a local
// destination, what the services writing there are expected to use at
// full retention. Disks with no recorded backup sizes are left out.
func (o *Ops) LocalDiskProjections(ctx context.Context) ([]DiskProjection, error) {
	services, err := inventory.ListServices(o.DB)
	if err != nil {
		return nil, err
	}
	dests, err := inventory.ListDestinations(o.DB)
	if err != nil {
		return nil, err
	}
	byName := map[string]inventory.Destination{}
	for _, d := range dests {
		byName[d.Name] = d
	}
	type acc struct {
		DiskProjection
		counted map[string]bool
		svcs    map[string]bool
	}
	disks := map[uint64]*acc{}
	for _, s := range services {
		for _, t := range s.EffectiveTargets() {
			d, ok := byName[t.Destination]
			if !ok || d.Type != backend.Local || d.Settings.Path == "" {
				continue
			}
			size, err := inventory.TargetSize(o.DB, s.Name, d.Name)
			if err != nil {
				return nil, err
			}
			if size <= 0 {
				continue
			}
			probe, dev, st, ok := diskKey(d.Settings.Path)
			if !ok {
				continue
			}
			a := disks[dev]
			if a == nil {
				bs := int64(st.Bsize)
				a = &acc{DiskProjection: DiskProjection{Name: probe, Total: int64(st.Blocks) * bs, Used: (int64(st.Blocks) - int64(st.Bfree)) * bs}, counted: map[string]bool{}, svcs: map[string]bool{}}
				disks[dev] = a
			}
			if !a.counted[d.Settings.Path] {
				a.counted[d.Settings.Path] = true
				a.Name = d.Settings.Path
				a.Existing += dirBytes(d.Settings.Path)
			}
			a.Planned += size * keptCount(t.EffectiveRetention(d))
			a.svcs[s.Name] = true
		}
	}
	var out []DiskProjection
	for _, a := range disks {
		for s := range a.svcs {
			a.Services = append(a.Services, s)
		}
		sort.Strings(a.Services)
		out = append(out, a.DiskProjection)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ProxmoxStorageProjections does the same for guest jobs: the Proxmox
// storage holds the dumps, so last dump size x backups kept is added
// to what the storage uses besides Ballast's current dumps.
func (o *Ops) ProxmoxStorageProjections(ctx context.Context, cl *proxmox.Client) ([]DiskProjection, error) {
	services, err := inventory.ListServices(o.DB)
	if err != nil {
		return nil, err
	}
	acc := map[string]*DiskProjection{}
	for _, s := range services {
		if !s.IsGuestJob() || s.Guest == nil {
			continue
		}
		size, err := inventory.LastProxmoxDumpSize(o.DB, s.Name)
		if err != nil {
			return nil, err
		}
		if size <= 0 {
			continue
		}
		key := s.ProxmoxNode + "/" + s.Guest.Storage
		p := acc[key]
		if p == nil {
			st, err := cl.StorageStatus(ctx, s.ProxmoxNode, s.Guest.Storage)
			if err != nil {
				continue // doctor's access check reports why
			}
			p = &DiskProjection{Name: key, Total: st.Total, Used: st.Used}
			acc[key] = p
		}
		owned, err := inventory.OwnedProxmoxDumpBytes(o.DB, s.Name, s.ProxmoxNode, s.Guest.Storage)
		if err != nil {
			return nil, err
		}
		keep := int64(s.Guest.KeepLast)
		if keep < 1 {
			keep = 1
		}
		p.Existing += owned
		p.Planned += size * keep
		p.Services = append(p.Services, s.Name)
	}
	var out []DiskProjection
	for _, p := range acc {
		sort.Strings(p.Services)
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// proxmoxAccessAdvice turns an access-check outcome into a doctor
// status and a message naming the missing role or path.
func proxmoxAccessAdvice(ac proxmox.AccessCheck) (DoctorStatus, string) {
	switch {
	case ac.GuestsErr != nil:
		return DoctorFail, fmt.Sprintf("the token is accepted but listing guests failed: %v. The token needs the PVEAuditor role (VM.Audit) on path /vms. A privilege-separated token needs its own permission entry; it does not inherit its user's.", ac.GuestsErr)
	case ac.Guests == 0:
		return DoctorFail, "the token is accepted but cannot see any guests. Give the token the PVEAuditor role (VM.Audit) on path /vms, and if it is privilege-separated, add that permission to the token itself (it does not inherit its user's permissions)."
	case ac.StorageErr != nil:
		return DoctorWarn, fmt.Sprintf("the token sees %d guest(s) but could not read storage %q: %v. Give it Datastore.Audit (or Datastore.AllocateSpace) on path /storage/%s.", ac.Guests, ac.StorageName, ac.StorageErr, ac.StorageName)
	case ac.StorageRead:
		return DoctorOK, fmt.Sprintf("the token sees %d guest(s) and can read storage %q", ac.Guests, ac.StorageName)
	}
	return DoctorOK, fmt.Sprintf("the token sees %d guest(s)", ac.Guests)
}

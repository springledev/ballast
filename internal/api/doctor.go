package api

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/springledev/ballast/internal/buildinfo"
	"github.com/springledev/ballast/internal/heartbeat"
	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/store"
)

// DoctorStatus is the outcome of one doctor check.
type DoctorStatus string

// The three doctor outcomes, matching the traffic lights used elsewhere.
const (
	DoctorOK   DoctorStatus = "ok"
	DoctorWarn DoctorStatus = "warning"
	DoctorFail DoctorStatus = "failed"
)

// DoctorCheck is one line of a `ballast doctor` report.
type DoctorCheck struct {
	// Area groups checks: "restic", "docker-host", "proxmox",
	// "destination" or "disk".
	Area string `json:"area"`
	// Name is what was checked (a host, destination or tool name).
	Name   string       `json:"name"`
	Status DoctorStatus `json:"status"`
	// Detail is a plain-English explanation, with the fix when it failed.
	Detail string `json:"detail"`
}

// DoctorReport is the full result of Doctor.
type DoctorReport struct {
	Checks []DoctorCheck `json:"checks"`
}

// Healthy reports whether no check failed. Warnings do not count.
func (r DoctorReport) Healthy() bool {
	for _, c := range r.Checks {
		if c.Status == DoctorFail {
			return false
		}
	}
	return true
}

// TerminalNotice is the warning shown when `ballast serve` runs in a
// terminal instead of as a service.
const TerminalNotice = "Ballast is running in a terminal, not as a service, so scheduled backups stop when it closes. Fix: run `sudo ballast install` to run it in the background and start it at boot."

// Free space thresholds for verification drills, which restore a dump
// into a throwaway container next to the work directory.
const (
	doctorDiskFailBytes = 1 << 30 // 1 GiB
	doctorDiskWarnBytes = 5 << 30 // 5 GiB
)

// Doctor checks the usual causes of a broken setup: restic, every
// Docker host, Proxmox (when configured), every destination's
// credentials and the free disk space for verification drills. Every
// check runs regardless of earlier failures, so one problem never
// hides another (spec rule 3); a check that errors is a failed line,
// never a failed call.
func (o *Ops) Doctor(ctx context.Context) (DoctorReport, error) {
	var rep DoctorReport
	add := func(area, name string, st DoctorStatus, detail string) {
		rep.Checks = append(rep.Checks, DoctorCheck{Area: area, Name: name, Status: st, Detail: o.redact(detail)})
	}

	add("ballast", "version", DoctorOK, "Ballast "+o.version()+" is running.")

	if o.DB != nil {
		if v, err := store.SchemaVersionOf(o.DB); err != nil {
			add("ballast", "schema", DoctorWarn, "Could not read the database version: "+err.Error())
		} else {
			add("ballast", "schema", DoctorOK, fmt.Sprintf("The database is at version %d. Each upgrade moves it forward and it cannot be moved back, so keep a copy of ballast.db before upgrading (docs/upgrading.md).", v))
		}
	}

	pre := o.CheckPreflight(ctx)
	switch {
	case !pre.ResticInstalled:
		add("restic", "restic", DoctorFail, fmt.Sprintf("restic could not be run: %s. Install restic (version 0.17 or newer) and make sure it is on the PATH.", pre.ResticError))
	case pre.ResticError != "":
		add("restic", "restic", DoctorWarn, fmt.Sprintf("restic %s is installed but: %s", pre.ResticVersion, pre.ResticError))
	default:
		add("restic", "restic", DoctorOK, "restic "+pre.ResticVersion+" is installed")
	}

	hosts, err := o.ListDockerHosts(ctx)
	if err != nil {
		return rep, fmt.Errorf("doctor: listing Docker hosts: %w", err)
	}
	for _, h := range hosts {
		tr, err := o.TestDockerHost(ctx, h.Name)
		if err != nil {
			add("docker-host", h.Name, DoctorFail, err.Error())
			continue
		}
		add("docker-host", h.Name, DoctorOK, "Docker "+tr.ServerVersion+" is reachable")
	}

	switch tr, err := o.TestProxmoxConnection(ctx); {
	case errors.Is(err, inventory.ErrNotFound):
		// Proxmox is optional: nothing to check.
	case err != nil:
		add("proxmox", "proxmox", DoctorFail, err.Error())
	default:
		add("proxmox", "proxmox", DoctorOK, "Proxmox VE "+tr.Version+" accepted the API token")
		o.proxmoxDoctor(ctx, add)
	}

	dests, err := o.ListDestinations(ctx)
	if err != nil {
		return rep, fmt.Errorf("doctor: listing destinations: %w", err)
	}
	o.protonDoctor(ctx, dests, add)
	for _, d := range dests {
		if err := o.TestDestination(ctx, d.Name); err != nil {
			add("destination", d.Name, DoctorFail, err.Error())
			continue
		}
		if !d.Type.IsResticRepository() {
			add("destination", d.Name, DoctorOK, "signed in to Proton Drive and the folder can be read")
			continue
		}
		add("destination", d.Name, DoctorOK, "credentials work and the repository can be read")
	}

	if o.ListenAddr != "" && !ListenIsLoopback(o.ListenAddr) {
		add("network", o.ListenAddr, DoctorWarn, "Ballast is reachable from your network over plain HTTP. Put it behind a reverse proxy that provides HTTPS (or use a tailnet/VPN) so your access key is not sent in the clear.")
	}

	if o.InTerminal {
		add("runtime", "terminal", DoctorWarn, TerminalNotice)
	}

	for _, c := range o.scriptDoctorChecks() {
		add(c.Area, c.Name, c.Status, c.Detail)
	}

	o.heartbeatDoctor(add)

	o.diskProjectionDoctor(ctx, add)

	if o.WorkRoot != "" {
		st, detail := diskCheck(o.WorkRoot)
		add("disk", o.WorkRoot, st, detail)
	}
	return rep, nil
}

// diskCheck reports the free space on the filesystem holding dir (or
// its nearest existing parent, since the work directory is created on
// first use).
func diskCheck(dir string) (DoctorStatus, string) {
	probe := dir
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
	var fs syscall.Statfs_t
	if err := syscall.Statfs(probe, &fs); err != nil {
		return DoctorWarn, fmt.Sprintf("could not read free disk space at %s: %v", probe, err)
	}
	free := fs.Bavail * uint64(fs.Bsize)
	gib := float64(free) / float64(1<<30)
	switch {
	case free < doctorDiskFailBytes:
		return DoctorFail, fmt.Sprintf("only %.1f GiB free; verification drills restore each dump here and will fail. Free up space or move the data directory.", gib)
	case free < doctorDiskWarnBytes:
		return DoctorWarn, fmt.Sprintf("%.1f GiB free; large databases may not fit in a verification drill.", gib)
	}
	return DoctorOK, fmt.Sprintf("%.1f GiB free for verification drills", gib)
}

// proxmoxDoctor checks that the token can list guests and read a
// storage (a privilege-separated token can pass the version check yet
// see nothing), and adds the Proxmox storage projections.
func (o *Ops) proxmoxDoctor(ctx context.Context, add func(area, name string, st DoctorStatus, detail string)) {
	conn, err := inventory.GetProxmoxConnection(o.DB)
	if err != nil {
		return
	}
	cl, err := o.Proxmox.Client(conn)
	if err != nil {
		return
	}
	storage := ""
	if services, err := inventory.ListServices(o.DB); err == nil {
		for _, s := range services {
			if s.IsGuestJob() && s.Guest != nil && s.Guest.Storage != "" {
				storage = s.Guest.Storage
				break
			}
		}
	}
	st, detail := proxmoxAccessAdvice(cl.CheckAccess(ctx, storage))
	add("proxmox", "access", st, detail)
	if projs, err := o.ProxmoxStorageProjections(ctx, cl); err == nil {
		for _, p := range projs {
			add("disk", "proxmox storage "+p.Name, projStatus(p), p.Detail())
		}
	}
}

// diskProjectionDoctor adds one line per local destination disk.
func (o *Ops) diskProjectionDoctor(ctx context.Context, add func(area, name string, st DoctorStatus, detail string)) {
	projs, err := o.LocalDiskProjections(ctx)
	if err != nil {
		add("disk", "projection", DoctorWarn, "could not work out how full the backup disks will get: "+err.Error())
		return
	}
	for _, p := range projs {
		add("disk", p.Name, projStatus(p), p.Detail())
	}
}

func projStatus(p DiskProjection) DoctorStatus {
	if p.Warn() {
		return DoctorWarn
	}
	return DoctorOK
}

// version is the reported build version, "dev" when none was set.
func (o *Ops) version() string {
	if o.Version == "" {
		return buildinfo.Dev
	}
	return o.Version
}

// heartbeatDoctor reports whether an external heartbeat monitor is
// configured and when it last answered. It shows a time only, never the
// monitor address, which is a secret.
func (o *Ops) heartbeatDoctor(add func(area, name string, st DoctorStatus, detail string)) {
	configured := false
	for _, dir := range o.CredentialSearchDirs {
		if _, err := os.Lstat(filepath.Join(dir, heartbeat.CredentialName)); err == nil {
			configured = true
			break
		}
	}
	if !configured {
		add("heartbeat", "monitor", DoctorOK, "No heartbeat monitor is set up. This is optional: it lets an outside service alert you if Ballast or this machine stops (put its address in the "+heartbeat.CredentialName+" credential file).")
		return
	}
	if o.DB == nil {
		return
	}
	last, ok, err := heartbeat.LastOK(o.DB)
	switch {
	case err != nil:
		add("heartbeat", "monitor", DoctorWarn, err.Error())
	case !ok:
		add("heartbeat", "monitor", DoctorWarn, "A heartbeat monitor is set up but has not been reached yet. It is pinged while Ballast is running (`ballast serve`).")
	case time.Since(last) > 3*heartbeat.DefaultInterval:
		add("heartbeat", "monitor", DoctorWarn, "A heartbeat monitor is set up but the last successful ping was at "+last.UTC().Format(time.RFC3339)+". Check that Ballast is running and the monitor is reachable.")
	default:
		add("heartbeat", "monitor", DoctorOK, "A heartbeat monitor is set up; the last successful ping was at "+last.UTC().Format(time.RFC3339)+".")
	}
}

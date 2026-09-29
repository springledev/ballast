package api

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/springledev/ballast/internal/inventory"
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
	}

	dests, err := o.ListDestinations(ctx)
	if err != nil {
		return rep, fmt.Errorf("doctor: listing destinations: %w", err)
	}
	for _, d := range dests {
		if err := o.TestDestination(ctx, d.Name); err != nil {
			add("destination", d.Name, DoctorFail, err.Error())
			continue
		}
		add("destination", d.Name, DoctorOK, "credentials work and the repository can be read")
	}

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

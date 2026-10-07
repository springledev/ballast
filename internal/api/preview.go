package api

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/springledev/ballast/internal/catalog"
	"github.com/springledev/ballast/internal/engine"
	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/protondrive"
	"github.com/springledev/ballast/internal/strategy"
)

// PreviewPart is one piece of a backup: a database dump, a folder
// group, a volume or an archive.
type PreviewPart struct {
	// Name is what the part is called when choosing what goes where.
	Name string `json:"name"`
	// What says in plain English what the part contains.
	What string `json:"what"`
	// Goes lists the destinations that receive this part.
	Goes []string `json:"goes"`
}

// PreviewTarget is one destination in a preview.
type PreviewTarget struct {
	Destination string `json:"destination"`
	// Parts are the parts this destination receives (all of them when
	// the service names none).
	Parts []string `json:"parts"`
	// Secondary marks a report-only second copy (Proton Drive).
	Secondary bool `json:"secondary,omitempty"`
	// Keeps says how many backups are kept there, in plain English.
	Keeps string `json:"keeps,omitempty"`
	// Note adds a remark, such as the size limit of a second copy.
	Note string `json:"note,omitempty"`
}

// ServicePreview is everything a backup of one service would do,
// worked out without running anything (spec §19.9).
type ServicePreview struct {
	Service string          `json:"service"`
	Parts   []PreviewPart   `json:"parts"`
	Targets []PreviewTarget `json:"targets"`
	// EstimatedBytes is the size of the folders that live on the
	// Ballast machine, minus what is skipped; nil when it cannot be
	// known without running the backup.
	EstimatedBytes *int64 `json:"estimated_bytes,omitempty"`
	// StopContainers are the containers stopped during the backup, in
	// order. Empty when the service stays up.
	StopContainers []string `json:"stop_containers"`
	// StoppedSeconds is how long the last backup took to copy the data
	// while they were stopped; nil before the first run.
	StoppedSeconds *int64 `json:"stopped_seconds,omitempty"`
	// Excludes are the skip patterns that apply (the catalog entry's and
	// the service's own).
	Excludes []string `json:"excludes"`
	// Notes are plain-English remarks about what the preview cannot show.
	Notes []string `json:"notes,omitempty"`
}

// Preview works out what a backup of service name would do — parts and
// where each goes, the estimated size, which containers stop and for
// how long, which skip patterns apply and what reaches a second copy.
// Nothing runs, stops or is written.
func (o *Ops) Preview(ctx context.Context, name string) (*ServicePreview, error) {
	s, err := inventory.GetService(o.DB, name)
	if err != nil {
		return nil, wrapf("service "+name, err)
	}
	targets := s.EffectiveTargets()
	dests := map[string]inventory.Destination{}
	for _, t := range targets {
		d, err := inventory.GetDestination(o.DB, t.Destination)
		if err != nil {
			return nil, wrapf("service "+name, err)
		}
		dests[t.Destination] = d
	}
	if s.Kind != "" {
		return previewJob(s, targets, dests), nil
	}
	_, entry, err := o.resolvedEntry(ctx, name)
	if err != nil {
		return nil, err
	}
	entry.AddExcludes(s.Excludes)
	pv := buildPreview(s, entry, targets, dests)
	if len(entry.Backup.Files) > 0 {
		if ex, err := strategy.PreviewExcludes(entry); err == nil && len(entry.Backup.HostPaths) == 0 {
			b := ex.KeptBytes
			pv.EstimatedBytes = &b
		}
	}
	if len(pv.StopContainers) > 0 {
		if secs, ok := o.lastStoppedSeconds(name); ok {
			pv.StoppedSeconds = &secs
		}
	}
	return pv, nil
}

// lastStoppedSeconds is how long the newest finished run spent copying
// data (the DUMPING step, during which stop-start containers are down).
func (o *Ops) lastStoppedSeconds(service string) (int64, bool) {
	var ms int64
	err := o.DB.QueryRow(`
		SELECT t.duration_ms FROM run_transitions t
		JOIN runs r ON r.id = t.run_id JOIN jobs j ON j.id = r.job_id
		WHERE j.service = ? AND t.from_state = ? AND t.to_state = ?
		ORDER BY t.id DESC LIMIT 1`, service, string(engine.DUMPING), string(engine.UPLOADING)).Scan(&ms)
	if err != nil {
		return 0, false
	}
	return (ms + 999) / 1000, true
}

// previewJob previews a Proxmox job, which has no catalog entry.
func previewJob(s inventory.Service, targets []inventory.Target, dests map[string]inventory.Destination) *ServicePreview {
	pv := &ServicePreview{Service: s.Name, Parts: []PreviewPart{}, StopContainers: []string{}, Excludes: []string{}}
	if s.IsGuestJob() && s.Guest != nil {
		mode := s.Guest.Mode
		if mode == "" {
			mode = "snapshot"
		}
		pv.Parts = append(pv.Parts, PreviewPart{Name: "guest-dump", What: fmt.Sprintf("a Proxmox backup of the guest, written to storage %q in %s mode", s.Guest.Storage, mode)})
		pv.Notes = append(pv.Notes, "Proxmox makes the backup on its own storage; the size depends on the guest and is not known beforehand.")
	} else {
		pv.Parts = append(pv.Parts, PreviewPart{Name: "settings", What: "your Proxmox settings (firewall rules, guest configs, storage, pools, users) read through the API; passwords and keys are left out"})
	}
	pv.Targets = previewTargets(pv, targets, dests)
	return pv
}

// buildPreview is the pure part of Preview: it turns a resolved
// catalog entry and the service's targets into a preview.
func buildPreview(s inventory.Service, entry *catalog.Entry, targets []inventory.Target, dests map[string]inventory.Destination) *ServicePreview {
	pv := &ServicePreview{Service: s.Name, Parts: []PreviewPart{}, StopContainers: []string{}, Excludes: []string{}}
	seen := map[string]int{}
	add := func(name, what string) {
		if i, ok := seen[name]; ok {
			pv.Parts[i].What += "; " + what
			return
		}
		seen[name] = len(pv.Parts)
		pv.Parts = append(pv.Parts, PreviewPart{Name: name, What: what})
	}
	b := entry.Backup
	for _, d := range b.Dumps {
		add(d.Name, "a database or app dump")
	}
	excl := map[string]bool{}
	for _, f := range b.Files {
		n := f.Name
		if n == "" {
			n = strategy.DefaultPathPart
		}
		add(n, "the folder "+f.Path)
		for _, e := range f.Exclude {
			excl[e] = true
		}
	}
	for _, v := range b.Volumes {
		add("volume-"+v.Name, "the Docker volume "+v.Name)
	}
	for _, h := range b.HostPaths {
		where := h.Path
		if where == "" {
			where = h.Mount + " in " + h.Container
		}
		add("data", "the folder "+where+" on the Docker host")
		for _, e := range h.Exclude {
			excl[e] = true
		}
	}
	if b.StopStart != nil && len(b.Files) == 0 && len(b.HostPaths) == 0 {
		add("data", "the data folders of the stopped containers")
	}
	if b.Compose != nil {
		add("compose-manifest", "the list of Compose files captured")
		add("compose-config", "the Compose files, env files and .env of project "+b.Compose.Project)
		add("compose-labels", "the Compose labels of the running containers")
	}
	if b.Exec != nil {
		add("exec", "what your script "+b.Exec.Script+" produces")
	}
	for e := range excl {
		pv.Excludes = append(pv.Excludes, e)
	}
	sort.Strings(pv.Excludes)
	if b.StopStart != nil {
		pv.StopContainers = append(pv.StopContainers, b.StopStart.Containers...)
	}
	if len(b.Volumes) > 0 {
		pv.Notes = append(pv.Notes, "Containers that use the volumes may be stopped while they are copied.")
	}
	if len(b.HostPaths) > 0 {
		pv.Notes = append(pv.Notes, "Size and skipped files for folders on another Docker host are only known when the backup runs.")
	}
	if len(b.Dumps) > 0 || b.Exec != nil || len(b.Volumes) > 0 || b.Compose != nil {
		pv.Notes = append(pv.Notes, "The size of dumps and archives is only known once the backup has run.")
	}
	pv.Targets = previewTargets(pv, targets, dests)
	return pv
}

// previewTargets fills each part's destinations and returns the target
// list.
func previewTargets(pv *ServicePreview, targets []inventory.Target, dests map[string]inventory.Destination) []PreviewTarget {
	out := []PreviewTarget{}
	for _, t := range targets {
		d := dests[t.Destination]
		pt := PreviewTarget{Destination: t.Destination, Parts: []string{}, Secondary: !d.Type.IsResticRepository()}
		for i := range pv.Parts {
			if t.Wants(pv.Parts[i].Name) {
				pv.Parts[i].Goes = append(pv.Parts[i].Goes, t.Destination)
				pt.Parts = append(pt.Parts, pv.Parts[i].Name)
			}
		}
		if len(pt.Parts) == 0 {
			pt.Note = fmt.Sprintf("None of the parts chosen (%s) exist in this service, so nothing would be sent here.", strings.Join(t.Parts, ", "))
		}
		if pt.Secondary {
			pt.Keeps = fmt.Sprintf("the newest %d copies", d.KeepLast)
			pt.Note = strings.TrimSpace(pt.Note + fmt.Sprintf(" A second copy of verified backups only; any file over %d MB is skipped and listed. It never changes the service's light.", protondrive.DefaultMaxArtifactBytes>>20))
		} else {
			pt.Keeps = keepsText(t.EffectiveRetention(d))
		}
		out = append(out, pt)
	}
	for i := range pv.Parts {
		if pv.Parts[i].Goes == nil {
			pv.Parts[i].Goes = []string{}
		}
	}
	return out
}

// keepsText describes a retention policy in plain English.
func keepsText(r inventory.Retention) string {
	var bits []string
	if r.KeepLast > 0 {
		bits = append(bits, fmt.Sprintf("the last %d", r.KeepLast))
	}
	if r.KeepDaily > 0 {
		bits = append(bits, fmt.Sprintf("%d daily", r.KeepDaily))
	}
	if r.KeepWeekly > 0 {
		bits = append(bits, fmt.Sprintf("%d weekly", r.KeepWeekly))
	}
	if r.KeepMonthly > 0 {
		bits = append(bits, fmt.Sprintf("%d monthly", r.KeepMonthly))
	}
	if len(bits) == 0 {
		return "everything"
	}
	return strings.Join(bits, ", ")
}

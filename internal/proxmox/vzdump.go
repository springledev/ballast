package proxmox

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Backup modes vzdump understands (Proxmox docs, "Backup modes").
const (
	// ModeSnapshot backs up a running guest with no downtime.
	ModeSnapshot = "snapshot"
	// ModeSuspend briefly pauses the guest while it is copied.
	ModeSuspend = "suspend"
	// ModeStop shuts the guest down for the backup, then restarts it.
	ModeStop = "stop"
)

// DefaultPollInterval is how often a running Proxmox task is checked.
const DefaultPollInterval = 5 * time.Second

// GuestBackupOptions describes one guest backup: which guest, how, and
// how many dumps of it to keep.
type GuestBackupOptions struct {
	// Node and VMID name the guest (see Guest).
	Node string `json:"node"`
	VMID int    `json:"vmid"`
	// Mode is snapshot (default), suspend or stop.
	Mode string `json:"mode,omitempty"`
	// Storage is the Proxmox storage the dump is written to, e.g.
	// "local" or "backup-nas". Required.
	Storage string `json:"storage"`
	// KeepLast is how many of this guest's dumps to keep on Storage
	// after a successful backup; older ones are deleted. Zero means
	// "prune nothing" — a dump is never deleted unless the operator
	// asked for it.
	KeepLast int `json:"keep_last,omitempty"`
}

// Validate checks o and fills in the default mode.
func (o *GuestBackupOptions) Validate() error {
	if o.Node == "" || o.VMID <= 0 {
		return errors.New("a Proxmox node and guest VMID are required")
	}
	if strings.TrimSpace(o.Storage) == "" {
		return fmt.Errorf("guest %d: choose the Proxmox storage the backup should be written to (for example \"local\")", o.VMID)
	}
	switch o.Mode {
	case "":
		o.Mode = ModeSnapshot
	case ModeSnapshot, ModeSuspend, ModeStop:
	default:
		return fmt.Errorf("guest %d: unknown backup mode %q — use snapshot, suspend or stop", o.VMID, o.Mode)
	}
	if o.KeepLast < 0 {
		return fmt.Errorf("guest %d: keep-last must not be negative", o.VMID)
	}
	return nil
}

// GuestBackupResult is what one guest backup produced. A finished
// dump is NOT a verified backup: proving it restores is the
// verification drill's job (VerifyGuestConfig, spec §14 step 26), so callers must not
// treat this as SUCCESS on its own (AGENTS.md rule 1).
type GuestBackupResult struct {
	Node    string `json:"node"`
	VMID    int    `json:"vmid"`
	Mode    string `json:"mode"`
	Storage string `json:"storage"`
	// UPID is the Proxmox task that ran the dump.
	UPID string `json:"upid"`
	// Volume is the new dump's volume ID, e.g.
	// "local:backup/vzdump-lxc-100-2026_09_28-03_00_00.tar.zst".
	Volume string `json:"volume"`
	// Size is the dump's size in bytes (0 if Proxmox did not report it).
	Size int64 `json:"size"`
	// Pruned lists old dumps deleted to honour KeepLast.
	Pruned []string `json:"pruned,omitempty"`
	// PruneError is set when pruning failed. It is reported beside —
	// never instead of — the backup's own outcome: a full disk of old
	// dumps must not make a good backup look failed, nor hide behind it.
	PruneError string `json:"prune_error,omitempty"`
	// Verification is the verification drill's verdict (spec §17.4),
	// filled in by the caller after BackupGuest: nil means the dump
	// has not been drilled and must not be shown as verified.
	Verification *GuestVerification `json:"verification,omitempty"`
}

// TaskStatus is a Proxmox task's state.
type TaskStatus struct {
	// Status is "running" or "stopped".
	Status string `json:"status"`
	// ExitStatus is "OK" once a stopped task succeeded; anything else
	// is the failure message.
	ExitStatus string `json:"exitstatus"`
}

// DumpVolume is one backup file on a Proxmox storage.
type DumpVolume struct {
	Volume string `json:"volid"`
	CTime  int64  `json:"ctime"`
	Size   int64  `json:"size"`
	VMID   int    `json:"vmid"`
}

// StartVzdump asks Proxmox to back up one guest and returns the task's
// UPID.
func (cl *Client) StartVzdump(ctx context.Context, o GuestBackupOptions) (string, error) {
	form := url.Values{}
	form.Set("vmid", strconv.Itoa(o.VMID))
	form.Set("mode", o.Mode)
	form.Set("storage", o.Storage)
	form.Set("compress", "zstd")
	var upid string
	if err := cl.do(ctx, http.MethodPost, "/api2/json/nodes/"+url.PathEscape(o.Node)+"/vzdump", form, &upid); err != nil {
		return "", fmt.Errorf("starting the backup of guest %d: %w", o.VMID, err)
	}
	if upid == "" {
		return "", fmt.Errorf("guest %d: Proxmox accepted the backup but returned no task ID", o.VMID)
	}
	return upid, nil
}

// TaskStatus reads a task's current state.
func (cl *Client) TaskStatus(ctx context.Context, node, upid string) (TaskStatus, error) {
	var st TaskStatus
	path := "/api2/json/nodes/" + url.PathEscape(node) + "/tasks/" + url.PathEscape(upid) + "/status"
	if err := cl.do(ctx, http.MethodGet, path, nil, &st); err != nil {
		return TaskStatus{}, err
	}
	return st, nil
}

// WaitTask polls a task every interval until it stops, and returns an
// error unless it finished with exit status OK.
func (cl *Client) WaitTask(ctx context.Context, node, upid string, interval time.Duration) error {
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	for {
		st, err := cl.TaskStatus(ctx, node, upid)
		if err != nil {
			return fmt.Errorf("checking Proxmox task %s: %w", upid, err)
		}
		if st.Status == "stopped" {
			if st.ExitStatus != "OK" {
				return fmt.Errorf("Proxmox reported the backup task failed: %s", st.ExitStatus)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("stopped waiting for Proxmox task %s: %w", upid, ctx.Err())
		case <-time.After(interval):
		}
	}
}

// ListDumps lists a guest's backup files on a storage, newest first.
func (cl *Client) ListDumps(ctx context.Context, node, storage string, vmid int) ([]DumpVolume, error) {
	var out []DumpVolume
	path := fmt.Sprintf("/api2/json/nodes/%s/storage/%s/content?content=backup&vmid=%d", url.PathEscape(node), url.PathEscape(storage), vmid)
	if err := cl.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	kept := out[:0]
	for _, d := range out {
		if d.VMID == vmid {
			kept = append(kept, d)
		}
	}
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].CTime > kept[j].CTime })
	return kept, nil
}

// DeleteDump deletes one backup file from a storage and waits for
// Proxmox to finish removing it.
func (cl *Client) DeleteDump(ctx context.Context, node, storage, volume string, interval time.Duration) error {
	path := "/api2/json/nodes/" + url.PathEscape(node) + "/storage/" + url.PathEscape(storage) + "/content/" + url.PathEscape(volume)
	var upid string
	if err := cl.do(ctx, http.MethodDelete, path, nil, &upid); err != nil {
		return err
	}
	if upid == "" || !strings.HasPrefix(upid, "UPID:") {
		return nil // older Proxmox versions delete synchronously
	}
	return cl.WaitTask(ctx, node, upid, interval)
}

// BackupGuest runs one vzdump of a guest end to end: start, wait for
// the task, identify the new dump, then prune older dumps down to
// KeepLast. Pruning only ever touches this guest's dumps, never the
// one just made, and only after the new dump exists. The result is an
// unverified dump (see GuestBackupResult).
func (cl *Client) BackupGuest(ctx context.Context, o GuestBackupOptions, poll time.Duration) (GuestBackupResult, error) {
	res := GuestBackupResult{Node: o.Node, VMID: o.VMID, Mode: o.Mode, Storage: o.Storage}
	if err := o.Validate(); err != nil {
		return res, err
	}
	res.Mode = o.Mode

	before, err := cl.ListDumps(ctx, o.Node, o.Storage, o.VMID)
	if err != nil {
		return res, fmt.Errorf("guest %d: listing existing backups on %s: %w", o.VMID, o.Storage, err)
	}
	seen := make(map[string]bool, len(before))
	for _, d := range before {
		seen[d.Volume] = true
	}

	upid, err := cl.StartVzdump(ctx, o)
	if err != nil {
		return res, err
	}
	res.UPID = upid
	if err := cl.WaitTask(ctx, o.Node, upid, poll); err != nil {
		return res, fmt.Errorf("guest %d: %w", o.VMID, err)
	}

	after, err := cl.ListDumps(ctx, o.Node, o.Storage, o.VMID)
	if err != nil {
		return res, fmt.Errorf("guest %d: the backup finished but its file could not be listed on %s: %w", o.VMID, o.Storage, err)
	}
	found := false
	for _, d := range after {
		if !seen[d.Volume] {
			res.Volume, res.Size, found = d.Volume, d.Size, true
			break // newest first
		}
	}
	if !found {
		return res, fmt.Errorf("guest %d: Proxmox said the backup succeeded but no new backup file appeared on %s", o.VMID, o.Storage)
	}

	if o.KeepLast > 0 && len(after) > o.KeepLast {
		var errs []string
		for _, d := range after[o.KeepLast:] {
			if d.Volume == res.Volume {
				continue
			}
			if err := cl.DeleteDump(ctx, o.Node, o.Storage, d.Volume, poll); err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", d.Volume, err))
				continue
			}
			res.Pruned = append(res.Pruned, d.Volume)
		}
		res.PruneError = strings.Join(errs, "; ")
	}
	return res, nil
}

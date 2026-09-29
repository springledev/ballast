package proxmox

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// Layer names for GuestVerification, so the UI and CLI can show each
// check on its own (spec §17.4).
const (
	// LayerGuestConfig is the guest-config restore drill: the dump's
	// stored guest configuration can be read back out of the dump.
	LayerGuestConfig = "guest-config"
	// LayerInGuest is the in-guest layer: the DB/file services attached
	// to the guest each passed their own verification.
	LayerInGuest = "in-guest"
)

// LayerResult is one independent verification layer's verdict.
type LayerResult struct {
	// Layer is LayerGuestConfig, or LayerInGuest for a single attached
	// service.
	Layer string `json:"layer"`
	// Service names the attached service for in-guest results.
	Service string `json:"service,omitempty"`
	// Passed is the verdict.
	Passed bool `json:"passed"`
	// Detail is a plain-English summary.
	Detail string `json:"detail"`
}

// GuestVerification combines the guest-level and in-guest layers. The
// layers are judged independently and ALL must pass (spec §17.4, rule
// 3 of AGENTS.md): a good guest config never hides a failed in-guest
// restore, and vice versa.
type GuestVerification struct {
	Layers []LayerResult `json:"layers"`
	// Verified is true only when every layer passed and the guest
	// config layer ran.
	Verified bool `json:"verified"`
}

// Combine builds a GuestVerification from the guest-config layer and
// the in-guest layers. Every layer is kept in the output.
func Combine(config LayerResult, inGuest ...LayerResult) GuestVerification {
	config.Layer = LayerGuestConfig
	gv := GuestVerification{Layers: []LayerResult{config}, Verified: config.Passed}
	for _, r := range inGuest {
		r.Layer = LayerInGuest
		gv.Layers = append(gv.Layers, r)
		if !r.Passed {
			gv.Verified = false
		}
	}
	return gv
}

// ExtractConfig reads the guest configuration stored inside a dump, via
// Proxmox's extractconfig endpoint. It is the read-only restore check:
// if Proxmox cannot unpack the dump far enough to read its config, the
// dump is not restorable.
func (cl *Client) ExtractConfig(ctx context.Context, node, volume string) (string, error) {
	var conf string
	path := "/api2/json/nodes/" + url.PathEscape(node) + "/vzdump/extractconfig?volume=" + url.QueryEscape(volume)
	if err := cl.do(ctx, "GET", path, nil, &conf); err != nil {
		return "", err
	}
	return conf, nil
}

// diskKeys are the config keys that prove a VM config describes a disk.
var diskKeys = []string{"scsi0", "virtio0", "sata0", "ide0", "efidisk0", "nvme0"}

// VerifyGuestConfig is the guest-config restore drill for a finished
// dump: the dump must still be listed on its storage, and the guest
// config read back out of it must be the right kind of config for the
// right guest, with a root disk.
func (cl *Client) VerifyGuestConfig(ctx context.Context, res GuestBackupResult) LayerResult {
	fail := func(format string, a ...any) LayerResult {
		return LayerResult{Layer: LayerGuestConfig, Detail: fmt.Sprintf("guest %d: "+format, append([]any{res.VMID}, a...)...)}
	}
	if res.Volume == "" {
		return fail("there is no backup file to check")
	}
	dumps, err := cl.ListDumps(ctx, res.Node, res.Storage, res.VMID)
	if err != nil {
		return fail("could not list backups on %s: %v", res.Storage, err)
	}
	var found *DumpVolume
	for i := range dumps {
		if dumps[i].Volume == res.Volume {
			found = &dumps[i]
		}
	}
	if found == nil {
		return fail("the new backup %s is no longer on %s", res.Volume, res.Storage)
	}
	if found.Size <= 0 {
		return fail("the backup file %s is empty", res.Volume)
	}
	conf, err := cl.ExtractConfig(ctx, res.Node, res.Volume)
	if err != nil {
		return fail("the guest's settings could not be read back out of the backup: %v", err)
	}
	if err := checkGuestConfig(conf, res.Volume); err != nil {
		return fail("%v", err)
	}
	return LayerResult{Layer: LayerGuestConfig, Passed: true,
		Detail: fmt.Sprintf("guest %d: the backup is on %s and its settings read back correctly", res.VMID, res.Storage)}
}

// checkGuestConfig validates the extracted config text against the
// kind of guest the volume name says was dumped.
func checkGuestConfig(conf, volume string) error {
	keys := map[string]bool{}
	for _, line := range strings.Split(conf, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") { // snapshot sections follow the current config
			break
		}
		if k, _, ok := strings.Cut(line, ":"); ok {
			keys[strings.TrimSpace(k)] = true
		}
	}
	if len(keys) == 0 {
		return fmt.Errorf("the settings stored in the backup are empty")
	}
	switch {
	case strings.Contains(volume, "vzdump-lxc-"):
		if !keys["rootfs"] {
			return fmt.Errorf("the container settings in the backup have no root disk (rootfs)")
		}
	case strings.Contains(volume, "vzdump-qemu-"):
		for _, k := range diskKeys {
			if keys[k] {
				return nil
			}
		}
		return fmt.Errorf("the VM settings in the backup list no disks")
	default:
		return fmt.Errorf("the backup %s is not a container or VM dump", volume)
	}
	return nil
}

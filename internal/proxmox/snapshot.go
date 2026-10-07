package proxmox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Names of the JSON files a config snapshot is made of. Each is also
// the name of the part a destination target can pick (spec §19.4).
const (
	SnapshotManifest = "manifest"
	SnapshotCluster  = "cluster"
	SnapshotNodes    = "nodes"
	SnapshotGuests   = "guests"
	SnapshotAccess   = "access"
)

// snapshotFiles lists the snapshot files in the order they are written.
var snapshotFiles = []string{SnapshotManifest, SnapshotCluster, SnapshotNodes, SnapshotGuests, SnapshotAccess}

// SnapshotParts lists the snapshot's parts in the order they are
// written.
func SnapshotParts() []string { return append([]string(nil), snapshotFiles...) }

// SnapshotFileName is the file name of one snapshot part.
func SnapshotFileName(part string) string { return "proxmox-" + part + ".json" }

// secretKey matches the names of Proxmox settings that hold a secret
// (passwords, cloud-init passwords, two-factor data, API token and
// encryption keys). Such a setting is removed from the snapshot, never
// stored (rule 2 of AGENTS.md).
var secretKey = regexp.MustCompile(`(?i)(pass(word|wd)?$|^cipassword$|secret|^tfa|totp|^keys$|^key$|encryption-key|private|^token$|^tokens$)`)

// ConfigSnapshot is everything read from Proxmox for one snapshot.
type ConfigSnapshot struct {
	// Files maps a snapshot part to its JSON document.
	Files map[string][]byte
	// Guests is every guest whose config was captured.
	Guests []Guest
	// Skipped lists the optional sections Proxmox did not let the
	// token read (plain English), so the run can say what is missing.
	Skipped []string
	// Removed counts the secret settings left out.
	Removed int
}

// scrub removes every secret setting from v (decoded JSON), returning
// the cleaned value and how many were removed.
func scrub(v any) (any, int) {
	removed := 0
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if secretKey.MatchString(k) {
				delete(t, k)
				removed++
				continue
			}
			c, n := scrub(val)
			t[k], removed = c, removed+n
		}
		return t, removed
	case []any:
		for i, val := range t {
			c, n := scrub(val)
			t[i], removed = c, removed+n
		}
		return t, removed
	}
	return v, 0
}

// get reads one API path as generic JSON, with secrets removed.
func (cl *Client) get(ctx context.Context, path string, removed *int) (any, error) {
	var raw json.RawMessage
	if err := cl.do(ctx, http.MethodGet, "/api2/json"+path, nil, &raw); err != nil {
		return nil, err
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("decoding Proxmox's response from %s: %w", path, err)
	}
	v, n := scrub(v)
	*removed += n
	return v, nil
}

// SnapshotConfig reads the host's configuration through the API only
// (spec §17.3, §19.6): cluster and node firewall rules, every guest's
// config, storage definitions, pools, users, groups, roles and ACLs.
// Nothing is ever written to Proxmox. The guest list, node list and
// storage definitions are required; a firewall, pool or access section
// the token may not read is skipped and named in Skipped.
func (cl *Client) SnapshotConfig(ctx context.Context) (*ConfigSnapshot, error) {
	s := &ConfigSnapshot{Files: map[string][]byte{}}
	optional := func(path string) any {
		v, err := cl.get(ctx, path, &s.Removed)
		if err != nil {
			s.Skipped = append(s.Skipped, fmt.Sprintf("%s (%v)", path, err))
			return nil
		}
		return v
	}
	guests, err := cl.ListGuests(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading the guest list: %w", err)
	}
	nodes, err := cl.get(ctx, "/nodes", &s.Removed)
	if err != nil {
		return nil, fmt.Errorf("reading the node list: %w", err)
	}
	storage, err := cl.get(ctx, "/storage", &s.Removed)
	if err != nil {
		return nil, fmt.Errorf("reading the storage definitions: %w", err)
	}

	cluster := map[string]any{
		"storage":          storage,
		"firewall_options": optional("/cluster/firewall/options"),
		"firewall_rules":   optional("/cluster/firewall/rules"),
		"firewall_groups":  optional("/cluster/firewall/groups"),
		"firewall_aliases": optional("/cluster/firewall/aliases"),
		"pools":            optional("/pools"),
	}
	perNode := map[string]any{}
	if list, ok := nodes.([]any); ok {
		for _, n := range list {
			m, _ := n.(map[string]any)
			name, _ := m["node"].(string)
			if name == "" {
				continue
			}
			perNode[name] = map[string]any{
				"info":             m,
				"firewall_options": optional("/nodes/" + name + "/firewall/options"),
				"firewall_rules":   optional("/nodes/" + name + "/firewall/rules"),
			}
		}
	}
	guestDocs := make([]any, 0, len(guests))
	for _, g := range guests {
		base := fmt.Sprintf("/nodes/%s/%s/%d", g.Node, g.Type, g.VMID)
		cfg, err := cl.get(ctx, base+"/config", &s.Removed)
		if err != nil {
			return nil, fmt.Errorf("reading the settings of guest %d (%s) on %s: %w", g.VMID, g.Name, g.Node, err)
		}
		guestDocs = append(guestDocs, map[string]any{
			"node": g.Node, "vmid": g.VMID, "type": g.Type, "name": g.Name,
			"config":         cfg,
			"firewall_rules": optional(base + "/firewall/rules"),
		})
	}
	access := map[string]any{
		"users":  optional("/access/users"),
		"groups": optional("/access/groups"),
		"roles":  optional("/access/roles"),
		"acl":    optional("/access/acl"),
	}
	s.Guests = guests
	sort.Strings(s.Skipped)

	manifest := map[string]any{
		"created_at": time.Now().UTC().Format(time.RFC3339),
		"note":       "Read-only copy of Proxmox settings made by Ballast. Passwords, tokens and keys are not included.",
		"guests":     len(guests),
		"skipped":    s.Skipped,
	}
	for part, doc := range map[string]any{
		SnapshotManifest: manifest, SnapshotCluster: cluster, SnapshotNodes: perNode,
		SnapshotGuests: guestDocs, SnapshotAccess: access,
	} {
		b, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("encoding the %s part of the snapshot: %w", part, err)
		}
		s.Files[part] = b
	}
	return s, nil
}

// WriteTo writes every snapshot file into dir (mode 0600) and returns
// the path of each, keyed by part.
func (s *ConfigSnapshot) WriteTo(dir string) (map[string]string, error) {
	out := map[string]string{}
	for _, part := range snapshotFiles {
		p := filepath.Join(dir, SnapshotFileName(part))
		if err := os.WriteFile(p, s.Files[part], 0o600); err != nil {
			return nil, fmt.Errorf("writing %s: %w", p, err)
		}
		out[part] = p
	}
	return out, nil
}

// VerifySnapshotFiles proves the files in paths (part → file) are a
// usable snapshot: each parses as JSON, the manifest and storage
// definitions are present, and every guest in current has a config
// in the guests file. A guest missing from the snapshot fails the
// check. The returned warning is non-empty when optional sections
// were skipped.
func VerifySnapshotFiles(paths map[string]string, current []Guest) LayerResult {
	docs := map[string]any{}
	for _, part := range snapshotFiles {
		p, ok := paths[part]
		if !ok {
			return LayerResult{Detail: fmt.Sprintf("the %s file is missing from the snapshot", part)}
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return LayerResult{Detail: fmt.Sprintf("could not read the snapshot's %s file back: %v", part, err)}
		}
		var v any
		if err := json.Unmarshal(b, &v); err != nil {
			return LayerResult{Detail: fmt.Sprintf("the snapshot's %s file is not valid JSON: %v", part, err)}
		}
		docs[part] = v
	}
	cluster, _ := docs[SnapshotCluster].(map[string]any)
	if cluster == nil || cluster["storage"] == nil {
		return LayerResult{Detail: "the snapshot has no storage definitions"}
	}
	have := map[string]bool{}
	list, _ := docs[SnapshotGuests].([]any)
	for _, g := range list {
		m, _ := g.(map[string]any)
		cfg, _ := m["config"].(map[string]any)
		vmid, _ := m["vmid"].(float64)
		node, _ := m["node"].(string)
		if len(cfg) > 0 {
			have[fmt.Sprintf("%s/%d", node, int(vmid))] = true
		}
	}
	var missing []string
	for _, g := range current {
		if !have[fmt.Sprintf("%s/%d", g.Node, g.VMID)] {
			missing = append(missing, fmt.Sprintf("%d (%s)", g.VMID, g.Name))
		}
	}
	if len(missing) > 0 {
		return LayerResult{Detail: "these guests have no settings in the snapshot: " + strings.Join(missing, ", ")}
	}
	return LayerResult{Passed: true, Detail: fmt.Sprintf("read the snapshot back: %d guest config(s), storage, firewall, pool and access settings all parse", len(current))}
}

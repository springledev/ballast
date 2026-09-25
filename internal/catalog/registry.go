package catalog

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
)

// Registry is a loaded, merged set of catalog entries (spec §6):
// bundled entries embedded in the binary, overlaid with entries from
// user catalog directories (~/.config/ballast/catalog.d/ by default).
// A Registry is read-only after LoadRegistry returns.
type Registry struct {
	entries map[string]registryEntry
}

// registryEntry pairs a loaded entry with where it came from, so
// LoadRegistry can report conflicts by path and distinguish a
// bundled entry (which a single user file may override) from an
// already-overriding user entry (which a second user file may not
// also claim).
type registryEntry struct {
	entry    *Entry
	source   string
	fromUser bool
}

// LoadRegistry loads every *.yaml file in bundled first, then, for
// each directory in userDirs in order, every *.yaml file there. A
// user entry whose service matches a bundled entry overrides it. Two
// user files (in the same or different directories) declaring the
// same service is an error naming both paths. A missing user
// directory is not an error — it simply contributes nothing. Any
// invalid catalog file is a loud error naming its path (fail loudly,
// spec rule "catalog is data").
func LoadRegistry(bundled fs.FS, userDirs []string) (*Registry, error) {
	r := &Registry{entries: map[string]registryEntry{}}

	bundledFiles, err := fs.Glob(bundled, "*.yaml")
	if err != nil {
		return nil, fmt.Errorf("listing bundled catalog entries: %w", err)
	}
	sort.Strings(bundledFiles)
	for _, name := range bundledFiles {
		data, err := fs.ReadFile(bundled, name)
		if err != nil {
			return nil, fmt.Errorf("reading bundled catalog entry %s: %w", name, err)
		}
		entry, err := Load(data)
		if err != nil {
			return nil, fmt.Errorf("bundled catalog entry %s: %w", name, err)
		}
		r.entries[entry.Service] = registryEntry{entry: entry, source: "bundled:" + name}
	}

	for _, dir := range userDirs {
		if err := r.loadUserDir(dir); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// loadUserDir overlays every *.yaml file directly inside dir. A
// missing directory contributes nothing and is not an error:
// filepath.Glob reports no matches for a nonexistent directory rather
// than failing.
func (r *Registry) loadUserDir(dir string) error {
	matches, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return fmt.Errorf("listing catalog directory %s: %w", dir, err)
	}
	sort.Strings(matches)
	for _, path := range matches {
		entry, err := LoadFile(path)
		if err != nil {
			return fmt.Errorf("user catalog entry %s: %w", path, err)
		}
		if existing, ok := r.entries[entry.Service]; ok && existing.fromUser {
			return fmt.Errorf("catalog entry %q is defined in two user files: %s and %s", entry.Service, existing.source, path)
		}
		r.entries[entry.Service] = registryEntry{entry: entry, source: path, fromUser: true}
	}
	return nil
}

// Get returns a deep copy of the named entry, so callers may freely
// mutate it (e.g. via Entry.Resolve) without affecting the registry
// or other callers. ok is false when no entry with that service name
// is loaded.
func (r *Registry) Get(service string) (*Entry, bool) {
	e, ok := r.entries[service]
	if !ok {
		return nil, false
	}
	return e.entry.DeepCopy(), true
}

// List returns every loaded entry, sorted by service name, each a
// deep copy safe for the caller to mutate.
func (r *Registry) List() []*Entry {
	names := make([]string, 0, len(r.entries))
	for name := range r.entries {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]*Entry, 0, len(names))
	for _, name := range names {
		out = append(out, r.entries[name].entry.DeepCopy())
	}
	return out
}

// Package progress keeps the latest upload progress of running backups
// in memory so the status API and web UI can show a progress bar. It
// holds numbers and destination names only, and is deliberately not
// persisted: after a restart it is empty until the next report.
package progress

import "sync"

// Upload is the latest progress report for one destination of a
// running backup.
type Upload struct {
	// Destination is the destination's name.
	Destination string `json:"destination"`
	// Percent is 0..100.
	Percent float64 `json:"percent"`
	// BytesDone and TotalBytes are what has been uploaded and what is
	// expected in all.
	BytesDone  uint64 `json:"bytes_done"`
	TotalBytes uint64 `json:"total_bytes"`
	// SecondsRemaining is restic's rough estimate; 0 when it has none.
	SecondsRemaining uint64 `json:"seconds_remaining,omitempty"`
}

// Tracker holds the latest Upload per service and destination. The
// zero value is ready to use and safe for concurrent use; a nil
// *Tracker ignores every call.
type Tracker struct {
	mu sync.Mutex
	m  map[string]map[string]Upload
}

// Set records the latest progress for a service's destination.
func (t *Tracker) Set(service string, u Upload) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m == nil {
		t.m = map[string]map[string]Upload{}
	}
	if t.m[service] == nil {
		t.m[service] = map[string]Upload{}
	}
	t.m[service][u.Destination] = u
}

// Clear forgets a service's progress for one destination.
func (t *Tracker) Clear(service, destination string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.m[service], destination)
	if len(t.m[service]) == 0 {
		delete(t.m, service)
	}
}

// Get returns the latest progress for a service's destination.
func (t *Tracker) Get(service, destination string) (Upload, bool) {
	if t == nil {
		return Upload{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	u, ok := t.m[service][destination]
	return u, ok
}

// ForService returns every destination's progress for a service.
func (t *Tracker) ForService(service string) []Upload {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []Upload
	for _, u := range t.m[service] {
		out = append(out, u)
	}
	return out
}

package proxmox

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
)

// WarnUseFraction is the projected disk use above which Ballast warns
// (spec §20.3: about 80%).
const WarnUseFraction = 0.80

// StorageStatus is the space on one Proxmox storage, in bytes.
type StorageStatus struct {
	Total int64 `json:"total"`
	Used  int64 `json:"used"`
	Avail int64 `json:"avail"`
}

// StorageStatus reads how full a storage is. It needs Datastore.Audit
// (or Datastore.AllocateSpace) on /storage/<name>.
func (cl *Client) StorageStatus(ctx context.Context, node, storage string) (StorageStatus, error) {
	var out StorageStatus
	path := fmt.Sprintf("/api2/json/nodes/%s/storage/%s/status", url.PathEscape(node), url.PathEscape(storage))
	if err := cl.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return StorageStatus{}, err
	}
	return out, nil
}

// ProjectUse returns the share (0..1+) of total that would be in use
// after adding add bytes to the current used bytes. It returns 0 when
// total is unknown.
func ProjectUse(total, used, add int64) float64 {
	if total <= 0 {
		return 0
	}
	return float64(used+add) / float64(total)
}

// CheckStorageRoom is the check made before a vzdump starts: it fails
// when the last dump of this guest (lastSize) would not fit in the
// storage's free space, and returns a warning when the projected use
// passes WarnUseFraction. lastSize 0 (no earlier dump) skips it.
func CheckStorageRoom(st StorageStatus, storage string, lastSize int64) (warning string, err error) {
	if lastSize <= 0 || st.Total <= 0 {
		return "", nil
	}
	if lastSize > st.Avail {
		return "", fmt.Errorf("storage %q has %s free but the last backup of this guest took %s, so the new one would fill it. Free up space on the storage or lower how many backups are kept, then run again", storage, FormatBytes(st.Avail), FormatBytes(lastSize))
	}
	if p := ProjectUse(st.Total, st.Used, lastSize); p > WarnUseFraction {
		return fmt.Sprintf("storage %q would be %.0f%% full after this backup (%s free now, last backup was %s)", storage, p*100, FormatBytes(st.Avail), FormatBytes(lastSize)), nil
	}
	return "", nil
}

// FormatBytes renders a byte count in plain English (GiB, MiB).
func FormatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// AccessCheck is the result of CheckAccess: what the API token can
// actually see.
type AccessCheck struct {
	Guests       int
	GuestsErr    error
	StorageName  string
	StorageErr   error
	StorageRead  bool
	StorageSkips bool // no node/storage known to read
}

// CheckAccess lists guests and reads one storage (node and storage
// are taken from the first guest and the given storage name when
// known), because a privilege-separated token can pass the version
// check yet see nothing.
func (cl *Client) CheckAccess(ctx context.Context, storage string) AccessCheck {
	var ac AccessCheck
	guests, err := cl.ListGuests(ctx)
	ac.GuestsErr = err
	ac.Guests = len(guests)
	node := ""
	if len(guests) > 0 {
		node = guests[0].Node
	}
	if storage == "" || node == "" {
		ac.StorageSkips = true
		return ac
	}
	ac.StorageName = storage
	if _, err := cl.StorageStatus(ctx, node, storage); err != nil {
		ac.StorageErr = err
	} else {
		ac.StorageRead = true
	}
	return ac
}

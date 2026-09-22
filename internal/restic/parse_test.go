package restic

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixture reads a golden fixture: real output captured from restic
// PinnedVersion (spec §13: "restic output parsing (golden fixtures
// from real restic output)").
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return data
}

// TestParseSnapshotsGolden pins the snapshots --json schema: two
// snapshots with full IDs, times, path-sets and tags exactly as
// restic 0.17.3 reports them.
func TestParseSnapshotsGolden(t *testing.T) {
	snaps, err := parseSnapshots(fixture(t, "snapshots.json"))
	if err != nil {
		t.Fatalf("parseSnapshots: %v", err)
	}
	if len(snaps) != 2 {
		t.Fatalf("got %d snapshots, want 2", len(snaps))
	}

	first := snaps[0]
	if first.ID != "009f5ef27a74190a8b16d0eac076b20ca7b37c4bbd31553c701c807a88e9f8f5" {
		t.Errorf("snapshots[0].ID = %q", first.ID)
	}
	if first.ShortID != "009f5ef2" {
		t.Errorf("snapshots[0].ShortID = %q", first.ShortID)
	}
	if first.Hostname != "testhost" {
		t.Errorf("snapshots[0].Hostname = %q", first.Hostname)
	}
	if len(first.Tags) != 1 || first.Tags[0] != "nightly" {
		t.Errorf("snapshots[0].Tags = %v, want [nightly]", first.Tags)
	}
	// spec §8: path-set membership is what attributes a snapshot to
	// a service — the JSON parser must preserve Paths in full.
	if len(first.Paths) != 1 || first.Paths[0] != "/tmp/ballast-test/src2" {
		t.Errorf("snapshots[0].Paths = %v, want [/tmp/ballast-test/src2]", first.Paths)
	}
	if first.Time.IsZero() {
		t.Error("snapshots[0].Time must parse")
	}

	second := snaps[1]
	if second.ShortID != "a8a1f356" {
		t.Errorf("snapshots[1].ShortID = %q", second.ShortID)
	}
	if !second.Time.After(first.Time) {
		t.Errorf("snapshots must stay in restic's order: [1].Time %v not after [0].Time %v", second.Time, first.Time)
	}
}

// TestParseSnapshotsRejectsSurprises is the fail-safe table (spec
// §8): structural surprises must be ParseErrors, never empty results
// that look like "no backups".
func TestParseSnapshotsRejectsSurprises(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{name: "not json", data: "Fatal: repository does not exist", want: "unexpected restic snapshots output"},
		{name: "empty id", data: `[{"time":"2026-09-22T23:36:28Z","id":""}]`, want: "empty id"},
		{name: "bad time", data: `[{"time":"yesterday","id":"abc"}]`, want: "unparseable time"},
		{name: "object not array", data: `{"id":"abc"}`, want: "unexpected restic snapshots output"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseSnapshots([]byte(tt.data))
			if err == nil {
				t.Fatal("parseSnapshots = nil error, want ParseError")
			}
			var pe *ParseError
			if !errors.As(err, &pe) {
				t.Fatalf("error type = %T, want *ParseError", err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want substring %q", err, tt.want)
			}
			if !strings.Contains(err.Error(), PinnedVersion) {
				t.Errorf("error = %q, want it to name the pinned restic version", err)
			}
		})
	}
}

// TestParseBackupSummaryGolden pins the backup --json summary schema
// with a real summary line.
func TestParseBackupSummaryGolden(t *testing.T) {
	res, err := parseBackupSummary(fixture(t, "backup-summary.jsonl"))
	if err != nil {
		t.Fatalf("parseBackupSummary: %v", err)
	}
	if res.SnapshotID != "009f5ef27a74190a8b16d0eac076b20ca7b37c4bbd31553c701c807a88e9f8f5" {
		t.Errorf("SnapshotID = %q", res.SnapshotID)
	}
	if res.FilesNew != 1 || res.FilesChanged != 0 || res.FilesUnmodified != 0 {
		t.Errorf("files = new %d changed %d unmodified %d, want 1/0/0", res.FilesNew, res.FilesChanged, res.FilesUnmodified)
	}
	if res.DirsNew != 3 || res.DirsChanged != 0 || res.DirsUnmodified != 0 {
		t.Errorf("dirs = new %d changed %d unmodified %d, want 3/0/0", res.DirsNew, res.DirsChanged, res.DirsUnmodified)
	}
	if res.DataAdded != 1917 || res.DataAddedPacked != 1579 {
		t.Errorf("data added = %d packed %d, want 1917/1579", res.DataAdded, res.DataAddedPacked)
	}
	if res.TotalFilesProcessed != 1 || res.TotalBytesProcessed != 4 {
		t.Errorf("totals = %d files %d bytes, want 1/4", res.TotalFilesProcessed, res.TotalBytesProcessed)
	}
	if res.Duration <= 0 {
		t.Errorf("Duration = %v, want positive", res.Duration)
	}
}

// TestParseBackupSummarySkipsStatus: progress "status" events precede
// the summary in real runs and must not confuse the parser.
func TestParseBackupSummarySkipsStatus(t *testing.T) {
	res, err := parseBackupSummary(fixture(t, "backup-events.jsonl"))
	if err != nil {
		t.Fatalf("parseBackupSummary: %v", err)
	}
	if res.FilesNew != 2 || res.TotalFilesProcessed != 2 {
		t.Errorf("summary = %+v, want the second line's summary (files_new 2)", res)
	}
}

// TestParseBackupSummaryRejectsSurprises is the fail-safe table for
// backup output.
func TestParseBackupSummaryRejectsSurprises(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{name: "no summary", data: `{"message_type":"status","percent_done":0.5}`, want: "no summary event"},
		{name: "empty snapshot id", data: `{"message_type":"summary","snapshot_id":""}`, want: "empty snapshot_id"},
		{name: "two summaries", data: "{\"message_type\":\"summary\",\"snapshot_id\":\"a\"}\n{\"message_type\":\"summary\",\"snapshot_id\":\"b\"}", want: "second summary"},
		{name: "unknown type", data: `{"message_type":"teleport"}`, want: `unknown message_type "teleport"`},
		{name: "error item", data: `{"message_type":"error_item","item":"/srv/data/x","error":"permission denied"}`, want: "permission denied"},
		{name: "garbage line", data: "{\"message_type\":\"summary\",\"snapshot_id\":\"a\"}\nnot json", want: "unexpected restic backup output line 2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseBackupSummary([]byte(tt.data))
			if err == nil {
				t.Fatal("parseBackupSummary = nil error, want ParseError")
			}
			var pe *ParseError
			if !errors.As(err, &pe) {
				t.Fatalf("error type = %T, want *ParseError", err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want substring %q", err, tt.want)
			}
		})
	}
}

// TestParseLSGolden pins the ls --json stream: one snapshot header
// then node events with paths and types.
func TestParseLSGolden(t *testing.T) {
	entries, err := parseLS(fixture(t, "ls.jsonl"))
	if err != nil {
		t.Fatalf("parseLS: %v", err)
	}

	var files, dirs []string
	for _, e := range entries {
		switch e.Type {
		case "file":
			files = append(files, e.Path)
		case "dir":
			dirs = append(dirs, e.Path)
		}
	}
	if len(files) != 2 || files[0] != "/tmp/ballast-test/src2/one.txt" || files[1] != "/tmp/ballast-test/src2/two.txt" {
		t.Errorf("files = %v, want the two .txt entries in ls order", files)
	}
	if len(dirs) < 3 {
		t.Errorf("dirs = %v, want at least the path chain to src2", dirs)
	}

	var size int64
	for _, e := range entries {
		if e.Path == "/tmp/ballast-test/src2/one.txt" {
			size = e.Size
		}
	}
	if size != 4 { // "one\n"
		t.Errorf("one.txt size = %d, want 4", size)
	}
}

// TestParseLSRejectsSurprises is the fail-safe table for ls output.
func TestParseLSRejectsSurprises(t *testing.T) {
	good := `{"message_type":"snapshot","id":"abc","time":"2026-09-22T23:36:28Z","short_id":"abc"}`
	tests := []struct {
		name string
		data string
		want string
	}{
		{name: "node before header", data: `{"message_type":"node","path":"/a","type":"file"}`, want: "before the snapshot header"},
		{name: "no header", data: ``, want: "no snapshot header"},
		{name: "unknown type", data: good + "\n" + `{"message_type":"blob"}`, want: `unknown message_type "blob"`},
		{name: "node empty path", data: good + "\n" + `{"message_type":"node","path":"","type":"file"}`, want: "empty path"},
		{name: "node empty type", data: good + "\n" + `{"message_type":"node","path":"/a","type":""}`, want: "empty type"},
		{name: "bad header time", data: `{"message_type":"snapshot","id":"abc","time":"nope"}`, want: "unparseable time"},
		{name: "garbage", data: "Fatal: cannot open repository", want: "unexpected restic ls output"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseLS([]byte(tt.data))
			if err == nil {
				t.Fatal("parseLS = nil error, want ParseError")
			}
			var pe *ParseError
			if !errors.As(err, &pe) {
				t.Fatalf("error type = %T, want *ParseError", err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want substring %q", err, tt.want)
			}
		})
	}
}

// TestParseForgetGolden pins the forget --json schema: one group per
// host+path-set with keep and remove lists (spec §8 grouping).
func TestParseForgetGolden(t *testing.T) {
	res, err := parseForget(fixture(t, "forget-keep-remove.json"))
	if err != nil {
		t.Fatalf("parseForget: %v", err)
	}
	if len(res.Kept) != 1 {
		t.Fatalf("Kept = %v, want 1 snapshot", res.Kept)
	}
	if len(res.Removed) != 1 {
		t.Fatalf("Removed = %v, want 1 snapshot", res.Removed)
	}
	if !strings.HasPrefix(res.Kept[0], "a8a1f356") {
		t.Errorf("Kept[0] = %q, want the newer snapshot a8a1f356…", res.Kept[0])
	}
	if !strings.HasPrefix(res.Removed[0], "009f5ef2") {
		t.Errorf("Removed[0] = %q, want the older snapshot 009f5ef2…", res.Removed[0])
	}
}

// TestParseForgetNullLists: a policy that removes nothing reports
// "remove": null in real output — not an error, not a crash.
func TestParseForgetNullLists(t *testing.T) {
	res, err := parseForget([]byte(`[{"host":"h","paths":["/p"],"tags":null,"keep":[{"id":"aaa","time":"2026-09-22T23:36:28Z"}],"remove":null}]`))
	if err != nil {
		t.Fatalf("parseForget: %v", err)
	}
	if len(res.Kept) != 1 || len(res.Removed) != 0 {
		t.Errorf("result = %+v, want 1 kept and no removals", res)
	}
}

func TestParseForgetRejectsSurprises(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{name: "not json", data: "Fatal: no policy", want: "unexpected restic forget output"},
		{name: "keep without id", data: `[{"keep":[{"time":"2026-09-22T23:36:28Z"}]}]`, want: "keep[0]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseForget([]byte(tt.data))
			if err == nil {
				t.Fatal("parseForget = nil error, want ParseError")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want substring %q", err, tt.want)
			}
		})
	}
}

// TestParseVersionGolden pins the `restic version` text line.
func TestParseVersionGolden(t *testing.T) {
	v, err := parseVersion(fixture(t, "version.txt"))
	if err != nil {
		t.Fatalf("parseVersion: %v", err)
	}
	if v.Number != "0.17.3" {
		t.Errorf("Number = %q, want 0.17.3", v.Number)
	}
	if v.Raw != "restic 0.17.3 compiled with go1.23.3 on linux/amd64" {
		t.Errorf("Raw = %q", v.Raw)
	}
	if !v.Compatible() {
		t.Error("0.17.3 must be compatible with the pinned series")
	}
	if w := v.Warning(); w != "" {
		t.Errorf("Warning = %q, want empty for the pinned release", w)
	}
}

func TestVersionWarningOnMismatch(t *testing.T) {
	tests := []struct {
		number string
		warn   bool
	}{
		{number: "0.17.3", warn: false},
		{number: "0.17.0", warn: false}, // same series: JSON schema stable
		{number: "0.16.4", warn: true},
		{number: "0.18.0", warn: true},
		{number: "1.0.0", warn: true},
	}
	for _, tt := range tests {
		t.Run(tt.number, func(t *testing.T) {
			v := Version{Number: tt.number}
			w := v.Warning()
			if tt.warn && w == "" {
				t.Error("Warning = empty, want a loud mismatch warning (spec §4)")
			}
			if !tt.warn && w != "" {
				t.Errorf("Warning = %q, want empty inside the pinned series", w)
			}
			if tt.warn && !strings.Contains(w, PinnedVersion) {
				t.Errorf("Warning = %q, want it to name the pinned release", w)
			}
		})
	}
}

func TestParseVersionRejectsSurprises(t *testing.T) {
	for _, data := range []string{"", "golang 1.25.0", "restic compiled", "restic x.y.z compiled"} {
		if _, err := parseVersion([]byte(data)); err == nil {
			t.Errorf("parseVersion(%q) = nil error, want failure", data)
		}
	}
}

// TestParseSnapshotsEmptyArray: a repository with no snapshots lists
// zero of them — that is a legitimate empty answer, not a parse
// surprise (the fail-safe distinction the spec asks for).
func TestParseSnapshotsEmptyArray(t *testing.T) {
	snaps, err := parseSnapshots([]byte(`[]`))
	if err != nil {
		t.Fatalf("parseSnapshots: %v", err)
	}
	if len(snaps) != 0 {
		t.Errorf("got %d snapshots, want 0", len(snaps))
	}
}

// TestFirstFileUnder samples one regular file per path-set (spec
// §7.3) and skips empty directories.
func TestFirstFileUnder(t *testing.T) {
	entries := []FileEntry{
		{Path: "/srv", Type: "dir"},
		{Path: "/srv/data", Type: "dir"},
		{Path: "/srv/data/a.txt", Type: "file", Size: 1},
		{Path: "/srv/data/sub", Type: "dir"},
		{Path: "/srv/data/sub/b.txt", Type: "file", Size: 2},
		{Path: "/srv/empty", Type: "dir"},
		{Path: "/srv/empty/link", Type: "symlink"},
	}
	tests := []struct {
		prefix string
		want   string
	}{
		{prefix: "/srv/data", want: "/srv/data/a.txt"},
		{prefix: "/srv/data/sub", want: "/srv/data/sub/b.txt"},
		{prefix: "/srv/empty", want: ""},
		{prefix: "/srv/nope", want: ""},
	}
	for _, tt := range tests {
		if got := firstFileUnder(entries, tt.prefix); got != tt.want {
			t.Errorf("firstFileUnder(%q) = %q, want %q", tt.prefix, got, tt.want)
		}
	}
}

// TestSelectSnapshot resolves "latest" by time and exact/prefix IDs.
func TestSelectSnapshot(t *testing.T) {
	snaps := []Snapshot{
		{ID: "aaaa1111", ShortID: "aaaa1111", Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		{ID: "bbbb2222", ShortID: "bbbb2222", Time: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)},
	}
	s, err := selectSnapshot(snaps, "latest")
	if err != nil || s.ID != "bbbb2222" {
		t.Errorf("latest = %+v, %v; want bbbb2222", s, err)
	}
	s, err = selectSnapshot(snaps, "aaaa1111")
	if err != nil || s.ID != "aaaa1111" {
		t.Errorf("by id = %+v, %v; want aaaa1111", s, err)
	}
	s, err = selectSnapshot(snaps, "aaaa")
	if err != nil || s.ID != "aaaa1111" {
		t.Errorf("by prefix = %+v, %v; want aaaa1111", s, err)
	}
	if _, err := selectSnapshot(snaps, "cccc3333"); err == nil {
		t.Error("unknown id = nil error, want failure")
	}
	if _, err := selectSnapshot(nil, "latest"); err == nil {
		t.Error("empty repo latest = nil error, want failure")
	}
}

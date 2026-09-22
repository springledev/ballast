package restic

// restic output parsing (spec §8).
//
// Every parser here is pinned to the --json output of restic
// PinnedVersion and fails safe: a structural surprise (missing
// required field, unknown message type, unparseable timestamp) is a
// ParseError naming the command and the offending line — never a
// silently empty result that would look like "no backups". Unknown
// extra JSON fields are tolerated on purpose: restic may add fields
// within the series, and ignoring additions is safe while ignoring
// removals is not.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ParseError reports restic output that did not match the pinned
// format (spec §8: fail safe, not silently). Match it with
// errors.As(err, &ParseError{}).
type ParseError struct {
	Op   string // which command's output ("snapshots", "backup", ...)
	Line int    // 1-based line number; 0 for whole-document output
	Text string // short redacted snippet of the offending output
	Err  error  // the underlying decode or validation failure
}

// Error describes the parse surprise in plain English.
func (e *ParseError) Error() string {
	where := e.Op
	if e.Line > 0 {
		where = fmt.Sprintf("%s output line %d", e.Op, e.Line)
	}
	if e.Text == "" {
		return fmt.Sprintf("unexpected restic %s output (restic %s format is pinned): %v", where, PinnedVersion, e.Err)
	}
	return fmt.Sprintf("unexpected restic %s output (restic %s format is pinned): %v near %q", where, PinnedVersion, e.Err, e.Text)
}

// Unwrap returns the underlying decode or validation failure.
func (e *ParseError) Unwrap() error { return e.Err }

// snippet shortens raw output for inclusion in a ParseError.
func snippet(b []byte) string {
	const max = 120
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

// wireSnapshot is the restic --json snapshot object (snapshots --json
// array elements, ls's first line, forget's keep/remove lists).
type wireSnapshot struct {
	Time string   `json:"time"`
	ID   string   `json:"id"`
	ShortID  string   `json:"short_id"`
	Hostname string   `json:"hostname"`
	Tags     []string `json:"tags"`
	Paths    []string `json:"paths"`
}

// toSnapshot converts and validates one wire snapshot.
func (w wireSnapshot) toSnapshot() (Snapshot, error) {
	if w.ID == "" {
		return Snapshot{}, fmt.Errorf("snapshot has an empty id")
	}
	ts, err := time.Parse(time.RFC3339Nano, w.Time)
	if err != nil {
		return Snapshot{}, fmt.Errorf("snapshot %s has an unparseable time %q: %w", short(w.ID), w.Time, err)
	}
	return Snapshot{
		ID:       w.ID,
		ShortID:  w.ShortID,
		Time:     ts,
		Hostname: w.Hostname,
		Tags:     w.Tags,
		Paths:    w.Paths,
	}, nil
}

// short returns an 8-hex prefix of a snapshot ID for messages.
func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// parseSnapshots parses `restic snapshots --json` output: a JSON
// array of snapshot objects (pinned: restic 0.17.x).
func parseSnapshots(data []byte) ([]Snapshot, error) {
	var wire []wireSnapshot
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&wire); err != nil {
		return nil, &ParseError{Op: "snapshots", Text: snippet(data), Err: err}
	}
	out := make([]Snapshot, 0, len(wire))
	for i, w := range wire {
		s, err := w.toSnapshot()
		if err != nil {
			return nil, &ParseError{Op: "snapshots", Line: i + 1, Text: snippet(data), Err: err}
		}
		out = append(out, s)
	}
	return out, nil
}

// wireBackupEvent is one line of `restic backup --json` output: a
// stream of status/summary/error_item events (pinned: restic 0.17.x).
type wireBackupEvent struct {
	MessageType string `json:"message_type"`

	// Summary fields.
	FilesNew            int     `json:"files_new"`
	FilesChanged        int     `json:"files_changed"`
	FilesUnmodified     int     `json:"files_unmodified"`
	DirsNew             int     `json:"dirs_new"`
	DirsChanged         int     `json:"dirs_changed"`
	DirsUnmodified      int     `json:"dirs_unmodified"`
	DataAdded           uint64  `json:"data_added"`
	DataAddedPacked     uint64  `json:"data_added_packed"`
	TotalFilesProcessed int     `json:"total_files_processed"`
	TotalBytesProcessed uint64  `json:"total_bytes_processed"`
	TotalDuration       float64 `json:"total_duration"`
	SnapshotID          string  `json:"snapshot_id"`

	// error_item fields (a file restic could not read).
	Error string `json:"error"`
	Item  string `json:"item"`
}

// parseBackupSummary parses `restic backup --json` output and returns
// its single summary event. Progress "status" lines are skipped; an
// "error_item" line fails the parse (an unreadable file is a backup
// problem, never a detail to drop).
func parseBackupSummary(data []byte) (*BackupResult, error) {
	var summary *BackupResult
	lineNo := 0
	for _, line := range bytes.Split(data, []byte("\n")) {
		lineNo++
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			continue
		}
		var ev wireBackupEvent
		if err := json.Unmarshal(trimmed, &ev); err != nil {
			return nil, &ParseError{Op: "backup", Line: lineNo, Text: snippet(trimmed), Err: err}
		}
		switch ev.MessageType {
		case "status":
			// Progress only; nothing to record.
		case "summary":
			if summary != nil {
				return nil, &ParseError{Op: "backup", Line: lineNo, Text: snippet(trimmed), Err: fmt.Errorf("second summary event in one backup run")}
			}
			if ev.SnapshotID == "" {
				return nil, &ParseError{Op: "backup", Line: lineNo, Text: snippet(trimmed), Err: fmt.Errorf("summary has an empty snapshot_id")}
			}
			summary = &BackupResult{
				SnapshotID:          ev.SnapshotID,
				FilesNew:            ev.FilesNew,
				FilesChanged:        ev.FilesChanged,
				FilesUnmodified:     ev.FilesUnmodified,
				DirsNew:             ev.DirsNew,
				DirsChanged:         ev.DirsChanged,
				DirsUnmodified:      ev.DirsUnmodified,
				DataAdded:           ev.DataAdded,
				DataAddedPacked:     ev.DataAddedPacked,
				TotalFilesProcessed: ev.TotalFilesProcessed,
				TotalBytesProcessed: ev.TotalBytesProcessed,
				Duration:            time.Duration(ev.TotalDuration * float64(time.Second)),
			}
		case "error_item":
			return nil, &ParseError{Op: "backup", Line: lineNo, Text: snippet(trimmed),
				Err: fmt.Errorf("restic could not back up %q: %s", ev.Item, ev.Error)}
		default:
			return nil, &ParseError{Op: "backup", Line: lineNo, Text: snippet(trimmed),
				Err: fmt.Errorf("unknown message_type %q", ev.MessageType)}
		}
	}
	if summary == nil {
		return nil, &ParseError{Op: "backup", Text: snippet(data), Err: fmt.Errorf("no summary event (the backup did not report a snapshot)")}
	}
	return summary, nil
}

// wireLSEvent is one line of `restic ls --json` output: first a
// snapshot object, then node objects (pinned: restic 0.17.x).
type wireLSEvent struct {
	MessageType string `json:"message_type"`

	// Node fields.
	Name string `json:"name"`
	Type string `json:"type"`
	Path string `json:"path"`
	Size int64  `json:"size"`

	// Snapshot fields (message_type "snapshot").
	wireSnapshot
}

// parseLS parses `restic ls --json` output into file entries. The
// leading snapshot event is validated but not returned (callers that
// need the snapshot use Snapshots or ReadBack).
func parseLS(data []byte) ([]FileEntry, error) {
	var entries []FileEntry
	lineNo := 0
	sawSnapshot := false
	for _, line := range bytes.Split(data, []byte("\n")) {
		lineNo++
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			continue
		}
		var ev wireLSEvent
		if err := json.Unmarshal(trimmed, &ev); err != nil {
			return nil, &ParseError{Op: "ls", Line: lineNo, Text: snippet(trimmed), Err: err}
		}
		switch ev.MessageType {
		case "snapshot":
			if sawSnapshot {
				return nil, &ParseError{Op: "ls", Line: lineNo, Text: snippet(trimmed), Err: fmt.Errorf("second snapshot header in one ls run")}
			}
			if _, err := ev.wireSnapshot.toSnapshot(); err != nil {
				return nil, &ParseError{Op: "ls", Line: lineNo, Text: snippet(trimmed), Err: err}
			}
			sawSnapshot = true
		case "node":
			if !sawSnapshot {
				return nil, &ParseError{Op: "ls", Line: lineNo, Text: snippet(trimmed), Err: fmt.Errorf("node before the snapshot header")}
			}
			if ev.Path == "" {
				return nil, &ParseError{Op: "ls", Line: lineNo, Text: snippet(trimmed), Err: fmt.Errorf("node has an empty path")}
			}
			if ev.Type == "" {
				return nil, &ParseError{Op: "ls", Line: lineNo, Text: snippet(trimmed), Err: fmt.Errorf("node %q has an empty type", ev.Path)}
			}
			entries = append(entries, FileEntry{Path: ev.Path, Type: ev.Type, Size: ev.Size})
		default:
			return nil, &ParseError{Op: "ls", Line: lineNo, Text: snippet(trimmed),
				Err: fmt.Errorf("unknown message_type %q", ev.MessageType)}
		}
	}
	if !sawSnapshot {
		return nil, &ParseError{Op: "ls", Text: snippet(data), Err: fmt.Errorf("no snapshot header (expected message_type \"snapshot\" first)")}
	}
	return entries, nil
}

// wireForgetGroup is one group of `restic forget --json` output:
// snapshots are grouped by host and path-set (spec §8), and each
// group reports which snapshots the policy keeps and removes.
type wireForgetGroup struct {
	Host   string         `json:"host"`
	Paths  []string       `json:"paths"`
	Tags   []string       `json:"tags"`
	Keep   []wireSnapshot `json:"keep"`
	Remove []wireSnapshot `json:"remove"`
}

// parseForget parses `restic forget --json` output: a JSON array of
// keep/remove groups (pinned: restic 0.17.x).
func parseForget(data []byte) (*ForgetResult, error) {
	var groups []wireForgetGroup
	if err := json.Unmarshal(data, &groups); err != nil {
		return nil, &ParseError{Op: "forget", Text: snippet(data), Err: err}
	}
	out := &ForgetResult{}
	for i, g := range groups {
		for j, w := range g.Keep {
			s, err := w.toSnapshot()
			if err != nil {
				return nil, &ParseError{Op: "forget", Line: i + 1, Text: snippet(data), Err: fmt.Errorf("keep[%d]: %w", j, err)}
			}
			out.Kept = append(out.Kept, s.ID)
		}
		for j, w := range g.Remove {
			s, err := w.toSnapshot()
			if err != nil {
				return nil, &ParseError{Op: "forget", Line: i + 1, Text: snippet(data), Err: fmt.Errorf("remove[%d]: %w", j, err)}
			}
			out.Removed = append(out.Removed, s.ID)
		}
	}
	return out, nil
}

// parseVersion parses `restic version` text output, e.g.
// "restic 0.17.3 compiled with go1.23.3 on linux/amd64". The text
// form is stable across releases and has no --json variant.
func parseVersion(data []byte) (Version, error) {
	raw := strings.TrimSpace(string(data))
	if raw == "" {
		return Version{}, &ParseError{Op: "version", Text: snippet(data), Err: fmt.Errorf("empty output")}
	}
	first := strings.SplitN(raw, "\n", 2)[0]
	fields := strings.Fields(first)
	if len(fields) < 2 || fields[0] != "restic" {
		return Version{}, &ParseError{Op: "version", Text: snippet(data), Err: fmt.Errorf("want a line like \"restic 0.17.3 ...\"")}
	}
	num := fields[1]
	if _, err := strconv.Atoi(strings.SplitN(num, ".", 2)[0]); err != nil {
		return Version{}, &ParseError{Op: "version", Text: snippet(data), Err: fmt.Errorf("unparseable version number %q", num)}
	}
	return Version{Raw: first, Number: num}, nil
}

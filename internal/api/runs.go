package api

import (
	"errors"
	"fmt"
	"time"

	"github.com/springledev/ballast/internal/engine"
	"github.com/springledev/ballast/internal/inventory"
)

const (
	// DefaultRunsLimit is how many runs a history page holds when the
	// caller does not say.
	DefaultRunsLimit = 20
	// MaxRunsLimit is the most runs one history page may hold.
	MaxRunsLimit = 100
	// MaxLogLines caps the log tail returned for one run.
	MaxLogLines = 200
	// MaxLogBytes caps the total size of the log tail returned for one run.
	MaxLogBytes = 64 * 1024
)

// VerificationSummary counts a run's verification checks.
type VerificationSummary struct {
	Checks   int `json:"checks"`
	Passed   int `json:"passed"`
	Failed   int `json:"failed"`
	Warnings int `json:"warnings"`
}

// RunTarget is one destination's result within a run. Destinations are
// always reported separately so one cannot hide another.
type RunTarget struct {
	Destination string `json:"destination"`
	State       string `json:"state"`
	Light       string `json:"light"`
	SnapshotID  string `json:"snapshot_id,omitempty"`
	Detail      string `json:"detail,omitempty"`
	Secondary   bool   `json:"secondary,omitempty"`
}

// RunSummary is one past run as listed in a service's history. State is
// shown exactly as stored; Light uses the same vocabulary as the
// status view.
type RunSummary struct {
	ID              int64               `json:"id"`
	Service         string              `json:"service"`
	State           string              `json:"state"`
	Light           string              `json:"light"`
	StartedAt       time.Time           `json:"started_at"`
	FinishedAt      *time.Time          `json:"finished_at,omitempty"`
	DurationSeconds *int64              `json:"duration_seconds,omitempty"`
	SnapshotID      string              `json:"snapshot_id,omitempty"`
	Error           string              `json:"error,omitempty"`
	Verification    VerificationSummary `json:"verification"`
	Targets         []RunTarget         `json:"targets"`
}

// RunPage is one page of a service's run history, newest first.
// NextBefore is the cursor for the following page (pass it as
// "before"); it is 0 when there are no older runs.
type RunPage struct {
	Runs       []RunSummary `json:"runs"`
	NextBefore int64        `json:"next_before,omitempty"`
}

// LogLine is one line of a run's log: when it happened and what.
type LogLine struct {
	At   time.Time `json:"at"`
	Text string    `json:"text"`
}

// RunDetail is one run in full: the summary, every verification
// result and the end of its log. Sizes are not recorded per run, so
// none is reported.
type RunDetail struct {
	RunSummary
	VerificationResults []VerificationRecord `json:"verification_results"`
	LogTail             []LogLine            `json:"log_tail"`
	LogTruncated        bool                 `json:"log_truncated,omitempty"`
}

// ListRuns returns a page of service's past runs, newest first. limit
// 0 means DefaultRunsLimit and is capped at MaxRunsLimit; before is
// the run ID to page from (0 = newest). Log tails are not included.
func (o *Ops) ListRuns(service string, limit int, before int64) (RunPage, error) {
	if limit < 0 || before < 0 {
		return RunPage{}, fmt.Errorf("service %s: %w: limit and before must not be negative", service, ErrValidation)
	}
	if limit == 0 {
		limit = DefaultRunsLimit
	}
	if limit > MaxRunsLimit {
		limit = MaxRunsLimit
	}
	if _, err := inventory.GetService(o.DB, service); err != nil {
		return RunPage{}, wrapf("service "+service, err)
	}
	runs, err := engine.ListServiceRuns(o.DB, service, before, limit+1)
	if err != nil {
		return RunPage{}, wrapf("service "+service, err)
	}
	page := RunPage{Runs: []RunSummary{}}
	if len(runs) > limit {
		runs = runs[:limit]
		page.NextBefore = runs[limit-1].ID
	}
	for _, r := range runs {
		s, _, err := o.runSummary(r)
		if err != nil {
			return RunPage{}, wrapf("service "+service, err)
		}
		page.Runs = append(page.Runs, s)
	}
	return page, nil
}

// GetRun returns run id in full, with verification detail and a log
// tail capped at MaxLogLines lines and MaxLogBytes bytes. Unknown IDs
// wrap inventory.ErrNotFound.
func (o *Ops) GetRun(id int64) (RunDetail, error) {
	run, err := engine.LoadVisibleRun(o.DB, id)
	if err != nil {
		if isRunNotFound(err) {
			return RunDetail{}, fmt.Errorf("run %d: %w", id, inventory.ErrNotFound)
		}
		return RunDetail{}, err
	}
	sum, recs, err := o.runSummary(run)
	if err != nil {
		return RunDetail{}, wrapf("service "+run.Service, err)
	}
	d := RunDetail{RunSummary: sum, VerificationResults: []VerificationRecord{}, LogTail: []LogLine{}}
	for _, r := range recs {
		d.VerificationResults = append(d.VerificationResults, VerificationRecord{
			Target: r.Target, Method: r.Method, Passed: r.Passed,
			Warning: o.redact(r.Warning), Detail: o.redact(r.Detail), Output: o.redact(r.Output),
			DurationMS: r.Duration.Milliseconds(), RecordedAt: r.RecordedAt,
		})
	}
	ts, err := engine.RecentTransitions(o.DB, id, MaxLogLines+1)
	if err != nil {
		return RunDetail{}, wrapf("service "+run.Service, err)
	}
	if len(ts) > MaxLogLines {
		ts, d.LogTruncated = ts[len(ts)-MaxLogLines:], true
	}
	lines := make([]LogLine, len(ts))
	for i, t := range ts {
		text := fmt.Sprintf("%s -> %s", t.From, t.To)
		if t.Detail != "" {
			text += ": " + t.Detail
		}
		lines[i] = LogLine{At: t.At.UTC(), Text: o.redact(text)}
	}
	total, start := 0, len(lines)
	for start > 0 && total+len(lines[start-1].Text)+1 <= MaxLogBytes {
		start--
		total += len(lines[start].Text) + 1
	}
	if start > 0 {
		d.LogTruncated = true
	}
	d.LogTail = lines[start:]
	return d, nil
}

func isRunNotFound(err error) bool { return errors.Is(err, engine.ErrRunNotFound) }

// runSummary builds the list item for run, also returning its raw
// verification records for callers that need them.
func (o *Ops) runSummary(r engine.Run) (RunSummary, []engine.VerificationRecord, error) {
	recs, err := engine.VerificationResults(o.DB, r.ID)
	if err != nil {
		return RunSummary{}, nil, err
	}
	targets, err := engine.RunTargets(o.DB, r.ID)
	if err != nil {
		return RunSummary{}, nil, err
	}
	inProgress := r.FinishedAt == nil
	s := RunSummary{
		ID: r.ID, Service: r.Service, State: string(r.State), Light: lightFor(r.State, inProgress),
		StartedAt: r.StartedAt, FinishedAt: r.FinishedAt, SnapshotID: r.SnapshotID,
		Error: o.redact(r.Error), Targets: []RunTarget{},
	}
	if r.FinishedAt != nil {
		d := int64(r.FinishedAt.Sub(r.StartedAt).Seconds())
		s.DurationSeconds = &d
	}
	for _, v := range recs {
		s.Verification.Checks++
		if v.Passed {
			s.Verification.Passed++
		} else {
			s.Verification.Failed++
		}
		if v.Warning != "" {
			s.Verification.Warnings++
		}
	}
	for _, t := range targets {
		s.Targets = append(s.Targets, RunTarget{
			Destination: t.Destination, State: string(t.State), Light: lightFor(t.State, false),
			SnapshotID: t.SnapshotID, Detail: o.redact(t.Detail), Secondary: t.Secondary,
		})
	}
	return s, recs, nil
}

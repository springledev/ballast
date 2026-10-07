package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/springledev/ballast/internal/repohealth"
)

// errNoHealth is returned when repository health was never wired in.
var errNoHealth = errors.New("repository health checks are not set up (internal wiring error)")

// ListRepoHealth returns every destination's repository health.
func (o *Ops) ListRepoHealth(ctx context.Context) ([]repohealth.Status, error) {
	if o.RepoHealth == nil {
		return nil, errNoHealth
	}
	return o.RepoHealth.List()
}

// GetRepoHealth returns one destination's repository health.
func (o *Ops) GetRepoHealth(ctx context.Context, name string) (repohealth.Status, error) {
	if o.RepoHealth == nil {
		return repohealth.Status{}, errNoHealth
	}
	return o.RepoHealth.Get(name)
}

// RepoHealthSettings is how often a destination is checked.
type RepoHealthSettings struct {
	// Enabled switches scheduled checks on or off.
	Enabled bool `json:"enabled"`
	// IntervalHours is the time between checks.
	IntervalHours int `json:"interval_hours"`
	// Subset is the share of data read back ("5%", "1/10", "500M");
	// empty checks the repository structure only.
	Subset string `json:"subset"`
}

// ConfigureRepoHealth changes a destination's check schedule.
func (o *Ops) ConfigureRepoHealth(ctx context.Context, name string, s RepoHealthSettings) (repohealth.Status, error) {
	if o.RepoHealth == nil {
		return repohealth.Status{}, errNoHealth
	}
	if err := o.RepoHealth.Configure(name, s.Enabled, s.IntervalHours, s.Subset); err != nil {
		return repohealth.Status{}, fmt.Errorf("%w: %v", ErrValidation, o.redactErr(err))
	}
	return o.RepoHealth.Get(name)
}

// CheckRepoHealth runs a repository check on a destination now.
func (o *Ops) CheckRepoHealth(ctx context.Context, name string) (repohealth.Status, error) {
	if o.RepoHealth == nil {
		return repohealth.Status{}, errNoHealth
	}
	st, err := o.RepoHealth.Check(ctx, name)
	if err != nil {
		return st, o.redactErr(err)
	}
	return st, nil
}

func (h *handler) listRepoHealth(w http.ResponseWriter, r *http.Request) {
	list, err := h.ops.ListRepoHealth(r.Context())
	if err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (h *handler) getRepoHealth(w http.ResponseWriter, r *http.Request) {
	st, err := h.ops.GetRepoHealth(r.Context(), pathName(r))
	if err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (h *handler) putRepoHealth(w http.ResponseWriter, r *http.Request) {
	var body RepoHealthSettings
	if err := decodeJSON(r, &body); err != nil {
		h.writeErr(w, err, http.StatusBadRequest)
		return
	}
	st, err := h.ops.ConfigureRepoHealth(r.Context(), pathName(r), body)
	if err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (h *handler) checkRepoHealth(w http.ResponseWriter, r *http.Request) {
	st, err := h.ops.CheckRepoHealth(r.Context(), pathName(r))
	if err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

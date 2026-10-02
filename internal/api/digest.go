package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/springledev/ballast/internal/notify"
)

// WeeklyDigest builds the weekly summary event: each service's primary
// traffic light and last verified restore age, plus the free disk space
// for verification drills.
func (o *Ops) WeeklyDigest(ctx context.Context) (notify.Event, error) {
	statuses, err := o.Status(ctx, "", false)
	if err != nil {
		return notify.Event{}, fmt.Errorf("weekly digest: %w", err)
	}
	services := make([]notify.DigestService, 0, len(statuses))
	for _, s := range statuses {
		ds := notify.DigestService{Name: s.Service, Light: "none"}
		for _, t := range s.Tiers {
			if t.Tier != notify.TierPrimary {
				continue
			}
			ds.Light = t.Light
			if t.VerifiedAgeSeconds != nil {
				age := time.Duration(*t.VerifiedAgeSeconds) * time.Second
				ds.VerifiedAge = &age
			}
		}
		services = append(services, ds)
	}
	disk := ""
	if o.WorkRoot != "" {
		_, disk = diskCheck(o.WorkRoot)
	}
	return notify.FormatWeeklyDigest(services, disk), nil
}

// DigestResult reports one weekly digest send.
type DigestResult struct {
	// Sent names the channels that delivered the digest.
	Sent []string `json:"sent"`
	// Message is the digest's one-line summary.
	Message string `json:"message"`
}

// ErrNoDigestChannels is returned by SendDigest when no channel has
// opted into the weekly digest.
var ErrNoDigestChannels = errors.New("no notification channel has the weekly digest turned on; turn it on for a channel in Settings (or `ballast notify channel edit <name> --digest`)")

// SendDigest sends the weekly digest to every channel that opted in.
// Each channel is tried regardless of the others; failures are returned
// joined and labelled with the channel name, alongside the channels
// that did deliver.
func (o *Ops) SendDigest(ctx context.Context) (DigestResult, error) {
	var res DigestResult
	chans, err := notify.ListChannels(o.DB)
	if err != nil {
		return res, fmt.Errorf("weekly digest: %w", err)
	}
	var targets []notify.ChannelConfig
	for _, c := range chans {
		if c.Digest {
			targets = append(targets, c)
		}
	}
	if len(targets) == 0 {
		return res, ErrNoDigestChannels
	}
	ev, err := o.WeeklyDigest(ctx)
	if err != nil {
		return res, err
	}
	res.Message = ev.Message
	res.Sent = []string{}
	var errs []error
	for _, c := range targets {
		ch, err := c.Build(o.credDirs(), o.secretSet())
		if err == nil {
			err = ch.Send(ctx, ev)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("channel %s: %w", c.Name, err))
			continue
		}
		res.Sent = append(res.Sent, c.Name)
	}
	return res, o.redactErr(errors.Join(errs...))
}

func (h *handler) sendDigest(w http.ResponseWriter, r *http.Request) {
	res, err := h.ops.SendDigest(r.Context())
	switch {
	case errors.Is(err, ErrNoDigestChannels):
		writeErrorMsg(w, http.StatusBadRequest, err.Error())
		return
	case err != nil && len(res.Sent) == 0:
		h.writeErr(w, err, http.StatusBadGateway)
		return
	case err != nil:
		writeJSON(w, http.StatusOK, struct {
			DigestResult
			Error string `json:"error"`
		}{res, err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

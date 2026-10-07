package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/springledev/keel/dockerhost"
	"net/http"
	"strconv"
	"time"

	"github.com/springledev/ballast/internal/backend"
	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/notify"
	"github.com/springledev/ballast/internal/proxmox"
	"github.com/springledev/ballast/internal/scheduler"
)

// NewHandler builds Ballast's REST API (spec §9: "All operations are
// also full REST API endpoints ... the GUI uses exactly this API").
// Handlers hold no logic beyond parsing the request and calling Ops;
// every real decision lives in internal/api.Ops so the CLI and the
// API can never drift apart.
//
// Every route requires `Authorization: Bearer <apiToken>` except
// POST .../trigger, which uses its own per-service token instead (see
// auth.go). That route is registered directly on the top-level mux,
// unwrapped by withAuth, rather than detected by a path-string
// heuristic in middleware: net/http's ServeMux picks the more
// specific of two matching patterns, so the exact
// "POST .../services/{name}/trigger" pattern here always wins over
// the "/api/v1/" subtree pattern below, by the same routing rules
// every other route relies on — not a heuristic that could drift out
// of sync with the route table.
func NewHandler(ops *Ops, apiToken string) http.Handler {
	return NewAuthedHandler(ops, NewAuth(apiToken, nil, ""))
}

// NewAuthedHandler is NewHandler with an explicit Auth, so the server
// can rotate the key and report where it listens. Besides the routes
// below, it serves GET /api/v1/health, POST /api/v1/auth/login and
// POST /api/v1/auth/logout without credentials (login is
// rate-limited); every other route needs the key or a session.
func NewAuthedHandler(ops *Ops, auth *Auth) http.Handler {
	h := &handler{ops: ops}
	protected := http.NewServeMux()

	protected.HandleFunc("GET /api/v1/status", h.status)
	protected.HandleFunc("GET /api/v1/catalog", h.catalog)
	protected.HandleFunc("GET /api/v1/detect", h.detect)
	protected.HandleFunc("GET /api/v1/preflight", h.preflight)
	protected.HandleFunc("GET /api/v1/doctor", h.doctor)
	protected.HandleFunc("POST /api/v1/verify", h.verifyAll)
	protected.HandleFunc("POST /api/v1/self-backup", h.selfBackup)
	protected.HandleFunc("GET /api/v1/ballast-health", h.ballastHealth)
	protected.HandleFunc("POST /api/v1/digest/send", h.sendDigest)

	protected.HandleFunc("GET /api/v1/services", h.listServices)
	protected.HandleFunc("POST /api/v1/services", h.createService)
	protected.HandleFunc("GET /api/v1/services/{name}", h.getService)
	protected.HandleFunc("PUT /api/v1/services/{name}", h.putService)
	protected.HandleFunc("DELETE /api/v1/services/{name}", h.deleteService)
	protected.HandleFunc("POST /api/v1/services/{name}/excludes/preview", h.previewExcludes)
	protected.HandleFunc("POST /api/v1/services/{name}/backup", h.startBackup)
	protected.HandleFunc("GET /api/v1/services/{name}/snapshots", h.snapshots)
	protected.HandleFunc("POST /api/v1/services/{name}/verify", h.verifyService)
	protected.HandleFunc("POST /api/v1/services/{name}/restore", h.restore)
	protected.HandleFunc("GET /api/v1/services/{name}/schedule", h.getSchedule)
	protected.HandleFunc("PUT /api/v1/services/{name}/schedule", h.putSchedule)
	protected.HandleFunc("PUT /api/v1/services/{name}/schedule/enabled", h.putScheduleEnabled)
	protected.HandleFunc("POST /api/v1/services/{name}/trigger-token", h.generateTriggerToken)
	protected.HandleFunc("GET /api/v1/services/{name}/notifications", h.getNotificationSettings)
	protected.HandleFunc("PUT /api/v1/services/{name}/notifications", h.putNotificationSettings)
	protected.HandleFunc("PUT /api/v1/services/{name}/secrets/{secret}", h.putSecret)
	protected.HandleFunc("PUT /api/v1/services/{name}/secrets/{secret}/env-file", h.putEnvSecret)
	protected.HandleFunc("POST /api/v1/env-sources/test", h.testEnvSource)
	protected.HandleFunc("POST /api/v1/catalog/{name}/env-source", h.readEnvSource)

	protected.HandleFunc("GET /api/v1/notification-channels", h.listChannels)
	protected.HandleFunc("POST /api/v1/notification-channels", h.putChannel)
	protected.HandleFunc("PUT /api/v1/notification-channels/{name}", h.putChannel)
	protected.HandleFunc("DELETE /api/v1/notification-channels/{name}", h.deleteChannel)
	protected.HandleFunc("POST /api/v1/notification-channels/{name}/test", h.testChannel)

	protected.HandleFunc("GET /api/v1/destinations", h.listDestinations)
	protected.HandleFunc("POST /api/v1/destinations", h.createDestination)
	protected.HandleFunc("DELETE /api/v1/destinations/{name}", h.deleteDestination)
	protected.HandleFunc("POST /api/v1/destinations/{name}/test", h.testDestination)
	protected.HandleFunc("POST /api/v1/destinations/{name}/init", h.initDestination)

	protected.HandleFunc("GET /api/v1/docker-hosts", h.listDockerHosts)
	protected.HandleFunc("POST /api/v1/docker-hosts", h.createDockerHost)
	protected.HandleFunc("GET /api/v1/docker-hosts/{name}", h.getDockerHost)
	protected.HandleFunc("DELETE /api/v1/docker-hosts/{name}", h.deleteDockerHost)
	protected.HandleFunc("POST /api/v1/docker-hosts/{name}/test", h.testDockerHost)
	protected.HandleFunc("GET /api/v1/docker-hosts/{name}/volumes", h.listDockerHostVolumes)
	protected.HandleFunc("GET /api/v1/docker-hosts/{name}/containers/{container}/mounts", h.listContainerMounts)
	protected.HandleFunc("GET /api/v1/docker-hosts/{name}/compose-projects", h.listDockerHostComposeProjects)
	protected.HandleFunc("GET /api/v1/docker-hosts/{name}/compose-dirs", h.listComposeDirs)
	protected.HandleFunc("POST /api/v1/docker-hosts/{name}/compose-dirs", h.addComposeDir)
	protected.HandleFunc("DELETE /api/v1/docker-hosts/{name}/compose-dirs", h.removeComposeDir)

	protected.HandleFunc("GET /api/v1/proxmox", h.getProxmoxConnection)
	protected.HandleFunc("PUT /api/v1/proxmox", h.putProxmoxConnection)
	protected.HandleFunc("DELETE /api/v1/proxmox", h.deleteProxmoxConnection)
	protected.HandleFunc("POST /api/v1/proxmox/test", h.testProxmoxConnection)
	protected.HandleFunc("GET /api/v1/proxmox/guests", h.listProxmoxGuests)
	protected.HandleFunc("POST /api/v1/proxmox/backup", h.backupProxmoxGuest)
	protected.HandleFunc("GET /api/v1/proxmox/storage-check", h.checkProxmoxStorage)
	protected.HandleFunc("GET /api/v1/proxmox/config", h.viewProxmoxConfig)

	protected.HandleFunc("GET /api/v1/auth/session", auth.session)
	protected.HandleFunc("POST /api/v1/auth/rotate", auth.rotate)
	protected.HandleFunc("POST /api/v1/auth/sign-out-everywhere", auth.signOutEverywhere)

	protected.HandleFunc("GET /api/v1/projects", h.listProjects)
	protected.HandleFunc("POST /api/v1/projects/{project}/restore", h.restoreProject)

	mux := http.NewServeMux()
	mux.Handle("/api/v1/", auth.middleware(protected))
	mux.HandleFunc("GET /api/v1/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/v1/auth/login", auth.login)
	mux.HandleFunc("POST /api/v1/auth/logout", auth.logout)
	mux.HandleFunc("POST /api/v1/services/{name}/trigger", h.trigger)
	return mux
}

type handler struct{ ops *Ops }

// --- response/request helpers -----------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErrorMsg(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// writeErr maps err to a status code (spec §14 step 11 D3) and writes
// a JSON error body, with defaultStatus used when err matches none of
// the known sentinels. The message is redacted (spec rule 2) as a
// last line of defence before it reaches an API response.
func (h *handler) writeErr(w http.ResponseWriter, err error, defaultStatus int) {
	status := defaultStatus
	switch {
	case errors.Is(err, inventory.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, inventory.ErrInUse), errors.Is(err, scheduler.ErrAlreadyRunning):
		status = http.StatusConflict
	case errors.Is(err, ErrValidation):
		status = http.StatusBadRequest
	}
	writeErrorMsg(w, status, h.ops.redact(err.Error()))
}

// decodeJSON reads and validates a JSON request body, rejecting
// unknown fields so a client typo is a loud 400, not a silently
// ignored field.
func decodeJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: decoding request body: %v", ErrValidation, err)
	}
	return nil
}

func pathName(r *http.Request) string { return r.PathValue("name") }

// --- status & catalog ----------------------------------------------------

func (h *handler) status(w http.ResponseWriter, r *http.Request) {
	service := r.URL.Query().Get("service")
	withVerify := r.URL.Query().Get("verify") == "1"
	st, err := h.ops.Status(r.Context(), service, withVerify)
	if err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (h *handler) catalog(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.ops.ListCatalog())
}

func (h *handler) detect(w http.ResponseWriter, r *http.Request) {
	res, err := h.ops.DetectServices(r.Context(), r.URL.Query().Get("host"))
	if err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// preflight never itself fails the request (spec: a best-effort tool
// check, not a hard gate) — CheckPreflight always returns a result,
// with any failure recorded per-tool inside it instead of as an HTTP
// error, so the wizard can render "restic: not found" without special
// casing a 500.
func (h *handler) preflight(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.ops.CheckPreflight(r.Context()))
}

// --- services --------------------------------------------------------

func (h *handler) listServices(w http.ResponseWriter, r *http.Request) {
	list, err := h.ops.ListServices(r.Context())
	if err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (h *handler) createService(w http.ResponseWriter, r *http.Request) {
	var s inventory.Service
	if err := decodeJSON(r, &s); err != nil {
		h.writeErr(w, err, http.StatusBadRequest)
		return
	}
	if s.DockerHost == "" {
		s.DockerHost = dockerhost.LocalName
	}
	if err := h.ops.PutService(r.Context(), s); err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, s)
}

func (h *handler) getService(w http.ResponseWriter, r *http.Request) {
	s, err := h.ops.GetService(r.Context(), pathName(r))
	if err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, s)
}

func (h *handler) putService(w http.ResponseWriter, r *http.Request) {
	var s inventory.Service
	if err := decodeJSON(r, &s); err != nil {
		h.writeErr(w, err, http.StatusBadRequest)
		return
	}
	s.Name = pathName(r)
	if s.DockerHost == "" {
		s.DockerHost = dockerhost.LocalName
	}
	if err := h.ops.PutService(r.Context(), s); err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, s)
}

func (h *handler) deleteService(w http.ResponseWriter, r *http.Request) {
	if err := h.ops.DeleteService(r.Context(), pathName(r)); err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) previewExcludes(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Excludes []string `json:"excludes"`
	}
	if err := decodeJSON(r, &body); err != nil {
		h.writeErr(w, err, http.StatusBadRequest)
		return
	}
	res, err := h.ops.PreviewExcludes(r.Context(), pathName(r), body.Excludes)
	if err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// --- backups, snapshots, restore ----------------------------------------

func (h *handler) startBackup(w http.ResponseWriter, r *http.Request) {
	interrupted, err := h.ops.StartBackup(r.Context(), pathName(r))
	if err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"started":                  true,
		"previous_run_interrupted": interrupted,
	})
}

func (h *handler) snapshots(w http.ResponseWriter, r *http.Request) {
	snaps, err := h.ops.Snapshots(r.Context(), pathName(r))
	if err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, snaps)
}

type restoreRequestBody struct {
	Snapshot string   `json:"snapshot"`
	Paths    []string `json:"paths"`
	To       string   `json:"to"`
}

func (h *handler) restore(w http.ResponseWriter, r *http.Request) {
	var body restoreRequestBody
	if err := decodeJSON(r, &body); err != nil {
		h.writeErr(w, err, http.StatusBadRequest)
		return
	}
	err := h.ops.Restore(r.Context(), RestoreRequest{
		Service: pathName(r), Snapshot: body.Snapshot, Paths: body.Paths, To: body.To,
	})
	if err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"restored": true})
}

func (h *handler) listProjects(w http.ResponseWriter, r *http.Request) {
	projects, err := h.ops.ListProjects(r.Context())
	if err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, projects)
}

type restoreProjectRequestBody struct {
	AsOf string `json:"as_of"`
	To   string `json:"to"`
}

// restoreProject mirrors verifyAll's status-code shape: a report is
// returned with 200 whenever RestoreProject managed to produce one
// (even when some of its services failed — each carries its own
// error), and only a request that never got that far (bad project
// name, invalid as_of, no target directory) is a 4xx/5xx.
func (h *handler) restoreProject(w http.ResponseWriter, r *http.Request) {
	var body restoreProjectRequestBody
	if err := decodeJSON(r, &body); err != nil {
		h.writeErr(w, err, http.StatusBadRequest)
		return
	}
	var asOf *time.Time
	if body.AsOf != "" {
		t, err := time.Parse(time.RFC3339, body.AsOf)
		if err != nil {
			h.writeErr(w, fmt.Errorf("%w: as_of must be an RFC3339 timestamp (e.g. 2026-01-02T15:04:05Z): %v", ErrValidation, err), http.StatusBadRequest)
			return
		}
		asOf = &t
	}
	rep, err := h.ops.RestoreProject(r.Context(), RestoreProjectRequest{
		Project: r.PathValue("project"), AsOf: asOf, To: body.To,
	})
	if err != nil && rep.Results == nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

func (h *handler) verifyService(w http.ResponseWriter, r *http.Request) {
	rep, err := h.ops.VerifyService(r.Context(), pathName(r))
	if err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// verifyAll always returns 200 with one report per configured service
// (each carrying its own light/error) unless VerifyAllServices could
// not even list the configured services; a service that came back
// red or amber is not itself an API-level failure.
func (h *handler) verifyAll(w http.ResponseWriter, r *http.Request) {
	reports, err := h.ops.VerifyAllServices(r.Context())
	if err != nil && reports == nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, reports)
}

// doctor always returns 200 with a report of system checks (each carrying
// its own status) unless Doctor could not even start; a check that failed
// is not itself an API-level failure.
func (h *handler) doctor(w http.ResponseWriter, r *http.Request) {
	report, err := h.ops.Doctor(r.Context())
	if err != nil && !report.Healthy() {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

type selfBackupRequestBody struct {
	Destination string `json:"destination"`
}

// selfBackup backs up Ballast's own state and returns the result as JSON,
// or an error if the backup failed or could not be verified.
func (h *handler) selfBackup(w http.ResponseWriter, r *http.Request) {
	var body selfBackupRequestBody
	if err := decodeJSON(r, &body); err != nil {
		h.writeErr(w, err, http.StatusBadRequest)
		return
	}
	result, err := h.ops.SelfBackup(r.Context(), body.Destination)
	if err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// --- schedules -------------------------------------------------------

func (h *handler) getSchedule(w http.ResponseWriter, r *http.Request) {
	s, err := h.ops.GetSchedule(r.Context(), pathName(r))
	if err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, s)
}

type scheduleRequestBody struct {
	Cron string `json:"cron"`
}

func (h *handler) putSchedule(w http.ResponseWriter, r *http.Request) {
	var body scheduleRequestBody
	if err := decodeJSON(r, &body); err != nil {
		h.writeErr(w, err, http.StatusBadRequest)
		return
	}
	name := pathName(r)
	if err := h.ops.SetSchedule(r.Context(), name, body.Cron); err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, scheduler.ScheduledJob{Service: name, Cron: body.Cron, Enabled: true})
}

type scheduleEnabledBody struct {
	Enabled bool `json:"enabled"`
}

func (h *handler) putScheduleEnabled(w http.ResponseWriter, r *http.Request) {
	var body scheduleEnabledBody
	if err := decodeJSON(r, &body); err != nil {
		h.writeErr(w, err, http.StatusBadRequest)
		return
	}
	if err := h.ops.SetScheduleEnabled(r.Context(), pathName(r), body.Enabled); err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"enabled": body.Enabled})
}

// --- notification settings -----------------------------------------------

func (h *handler) getNotificationSettings(w http.ResponseWriter, r *http.Request) {
	s, err := h.ops.GetNotificationSettings(r.Context(), pathName(r))
	if err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, s)
}

// notificationSettingsBody is a full replacement of a service's
// notification settings. Enabled is required: a client that forgets
// it must get a 400, not silently switch off the alerts that tell the
// user a backup has gone missing.
type notificationSettingsBody struct {
	Enabled          *bool `json:"enabled"`
	MaxAgeHours      *int  `json:"max_age_hours"`
	HeartbeatEnabled bool  `json:"heartbeat_enabled"`
}

func (h *handler) putNotificationSettings(w http.ResponseWriter, r *http.Request) {
	var body notificationSettingsBody
	if err := decodeJSON(r, &body); err != nil {
		h.writeErr(w, err, http.StatusBadRequest)
		return
	}
	if body.Enabled == nil {
		writeErrorMsg(w, http.StatusBadRequest, `"enabled" is required (true or false)`)
		return
	}
	name := pathName(r)
	s := notify.Settings{
		Service: name, Enabled: *body.Enabled, MaxAgeHours: body.MaxAgeHours, HeartbeatEnabled: body.HeartbeatEnabled,
	}
	if err := h.ops.SetNotificationSettings(r.Context(), name, s); err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, s)
}

// --- secrets -----------------------------------------------------------

type secretBody struct {
	Value string `json:"value"`
}

// putSecret never returns the stored value in its response (spec rule
// 2: shown/entered once, like a trigger token — but unlike a trigger
// token this one is user-supplied, so there is nothing to show back).
func (h *handler) putSecret(w http.ResponseWriter, r *http.Request) {
	var body secretBody
	if err := decodeJSON(r, &body); err != nil {
		h.writeErr(w, err, http.StatusBadRequest)
		return
	}
	name := pathName(r)
	secretName := r.PathValue("secret")
	if err := h.ops.SetSecret(r.Context(), name, secretName, body.Value); err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]bool{"set": true})
}

// envSecretBody names a key inside an app's .env file; it carries no value.
type envSecretBody struct {
	Path string `json:"path"`
	Key  string `json:"key"`
}

func (h *handler) putEnvSecret(w http.ResponseWriter, r *http.Request) {
	var body envSecretBody
	if err := decodeJSON(r, &body); err != nil {
		h.writeErr(w, err, http.StatusBadRequest)
		return
	}
	if err := h.ops.SetEnvSecret(r.Context(), pathName(r), r.PathValue("secret"), body.Path, body.Key); err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"set": true})
}

// testEnvSource lists the key names (never values) in a .env file.
func (h *handler) testEnvSource(w http.ResponseWriter, r *http.Request) {
	var body envSecretBody
	if err := decodeJSON(r, &body); err != nil {
		h.writeErr(w, err, http.StatusBadRequest)
		return
	}
	keys, err := h.ops.TestEnvSource(r.Context(), body.Path)
	if err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string][]string{"keys": keys})
}

// readEnvSource proposes setup values from the .env in an app's folder.
func (h *handler) readEnvSource(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Dir string `json:"dir"`
	}
	if err := decodeJSON(r, &body); err != nil {
		h.writeErr(w, err, http.StatusBadRequest)
		return
	}
	prop, err := h.ops.ReadEnvSource(r.Context(), r.PathValue("name"), body.Dir)
	if err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, prop)
}

// --- trigger tokens & trigger --------------------------------------------

func (h *handler) generateTriggerToken(w http.ResponseWriter, r *http.Request) {
	token, err := h.ops.GenerateTriggerToken(r.Context(), pathName(r))
	if err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	// Shown exactly once (spec rule 2): never logged, never returned
	// by any other endpoint, and never cached.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, map[string]string{"token": token})
}

// trigger is authenticated by its own per-service token, not the
// ordinary API bearer token — see NewHandler's routing, which
// registers this route unwrapped by withAuth. Like startBackup, it
// starts the run in the background and returns 202 immediately: it
// must not hold the request open for an hours-long backup, nor let
// the request's own cancellation or timeout kill a run that outlives
// it (net/http cancels r.Context() the instant this handler returns).
func (h *handler) trigger(w http.ResponseWriter, r *http.Request) {
	name := pathName(r)
	token := r.Header.Get("X-Ballast-Trigger-Token")
	interrupted, err := h.ops.StartTrigger(r.Context(), name, token)
	switch {
	case err == nil:
		writeJSON(w, http.StatusAccepted, map[string]any{
			"started":                  true,
			"previous_run_interrupted": interrupted,
		})
	case errors.Is(err, scheduler.ErrInvalidTriggerToken):
		writeErrorMsg(w, http.StatusUnauthorized, h.ops.redact(err.Error()))
	default:
		h.writeErr(w, err, http.StatusInternalServerError)
	}
}

// --- destinations ------------------------------------------------------

func (h *handler) listDestinations(w http.ResponseWriter, r *http.Request) {
	list, err := h.ops.ListDestinations(r.Context())
	if err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// createDestinationBody is POST /api/v1/destinations' request shape:
// the destination, write-only backend credentials, and whether to set
// the repository up now.
type createDestinationBody struct {
	inventory.Destination
	// Credentials are saved to a root-only credential file and never
	// echoed back (spec rule 2).
	Credentials backend.Credentials `json:"credentials"`
	Init        bool                `json:"init"`
}

func (h *handler) createDestination(w http.ResponseWriter, r *http.Request) {
	var body createDestinationBody
	if err := decodeJSON(r, &body); err != nil {
		h.writeErr(w, err, http.StatusBadRequest)
		return
	}
	req := DestinationSetup{Destination: body.Destination, Credentials: body.Credentials, Init: body.Init}
	if err := h.ops.SetupDestination(r.Context(), req); err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	// Respond with what was saved (generated location, default
	// credential file names), never the request body: that would echo
	// the credentials straight back.
	saved, err := h.ops.GetDestination(r.Context(), body.Name)
	if err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, saved)
}

func (h *handler) deleteDestination(w http.ResponseWriter, r *http.Request) {
	if err := h.ops.DeleteDestination(r.Context(), pathName(r)); err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) testDestination(w http.ResponseWriter, r *http.Request) {
	if err := h.ops.TestDestination(r.Context(), pathName(r)); err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// initDestination makes sure a destination's repository exists,
// without requiring the caller to resend the whole destination (spec
// §14 step 19: "restic init per destination", available on demand for
// a destination added without --init or whose storage was emptied).
func (h *handler) initDestination(w http.ResponseWriter, r *http.Request) {
	if err := h.ops.InitDestination(r.Context(), pathName(r)); err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"initialized": true})
}

// --- docker hosts --------------------------------------------------------

func (h *handler) listDockerHosts(w http.ResponseWriter, r *http.Request) {
	list, err := h.ops.ListDockerHosts(r.Context())
	if err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (h *handler) getDockerHost(w http.ResponseWriter, r *http.Request) {
	host, err := h.ops.GetDockerHost(r.Context(), pathName(r))
	if err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, host)
}

// createDockerHostBody is POST /api/v1/docker-hosts' request shape:
// the host, write-only credentials, and whether to skip the
// connection test.
type createDockerHostBody struct {
	dockerhost.Host
	// Credentials are saved to root-only credential file(s) and never
	// echoed back (spec rule 2).
	Credentials dockerhost.Credentials `json:"credentials"`
	SkipTest    bool                   `json:"skip_test"`
}

func (h *handler) createDockerHost(w http.ResponseWriter, r *http.Request) {
	var body createDockerHostBody
	if err := decodeJSON(r, &body); err != nil {
		h.writeErr(w, err, http.StatusBadRequest)
		return
	}
	req := DockerHostSetup{Host: body.Host, Credentials: body.Credentials, SkipTest: body.SkipTest}
	// Respond with what SetupDockerHost actually saved and found,
	// never the request body: that would echo the credentials
	// straight back.
	res, err := h.ops.SetupDockerHost(r.Context(), req)
	if err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

func (h *handler) deleteDockerHost(w http.ResponseWriter, r *http.Request) {
	if err := h.ops.DeleteDockerHost(r.Context(), pathName(r)); err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) testDockerHost(w http.ResponseWriter, r *http.Request) {
	tr, err := h.ops.TestDockerHost(r.Context(), pathName(r))
	if err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, tr)
}

func (h *handler) listDockerHostVolumes(w http.ResponseWriter, r *http.Request) {
	list, err := h.ops.ListDockerHostVolumes(r.Context(), pathName(r))
	if err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (h *handler) listContainerMounts(w http.ResponseWriter, r *http.Request) {
	list, err := h.ops.ListContainerMounts(r.Context(), pathName(r), r.PathValue("container"))
	if err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (h *handler) listDockerHostComposeProjects(w http.ResponseWriter, r *http.Request) {
	list, err := h.ops.ListDockerHostComposeProjects(r.Context(), pathName(r))
	if err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (h *handler) listComposeDirs(w http.ResponseWriter, r *http.Request) {
	list, err := h.ops.ListComposeDirs(r.Context(), pathName(r))
	if err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// composeDirBody is the request shape of POST/DELETE
// /api/v1/docker-hosts/{name}/compose-dirs: the folder on the host and,
// when adding, whether it is one stack's folder ("project") or a parent
// to scan ("scan").
type composeDirBody struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
}

func (h *handler) addComposeDir(w http.ResponseWriter, r *http.Request) {
	var body composeDirBody
	if err := decodeJSON(r, &body); err != nil {
		h.writeErr(w, err, http.StatusBadRequest)
		return
	}
	saved, err := h.ops.AddComposeDir(r.Context(), inventory.ComposeDir{DockerHost: pathName(r), Path: body.Path, Kind: body.Kind})
	if err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, saved)
}

func (h *handler) removeComposeDir(w http.ResponseWriter, r *http.Request) {
	var body composeDirBody
	if err := decodeJSON(r, &body); err != nil {
		h.writeErr(w, err, http.StatusBadRequest)
		return
	}
	if err := h.ops.RemoveComposeDir(r.Context(), pathName(r), body.Path); err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) getProxmoxConnection(w http.ResponseWriter, r *http.Request) {
	c, err := h.ops.GetProxmoxConnection(r.Context())
	if err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// putProxmoxConnectionBody is PUT /api/v1/proxmox's request shape: the
// connection, write-only credentials, and whether to skip the
// connection test.
type putProxmoxConnectionBody struct {
	proxmox.Connection
	// Credentials are saved to a root-only credential file and never
	// echoed back (spec rule 2).
	Credentials proxmox.Credentials `json:"credentials"`
	SkipTest    bool                `json:"skip_test"`
}

func (h *handler) putProxmoxConnection(w http.ResponseWriter, r *http.Request) {
	var body putProxmoxConnectionBody
	if err := decodeJSON(r, &body); err != nil {
		h.writeErr(w, err, http.StatusBadRequest)
		return
	}
	req := ProxmoxConnectionSetup{Connection: body.Connection, Credentials: body.Credentials, SkipTest: body.SkipTest}
	// Respond with what SetupProxmoxConnection actually saved and
	// found, never the request body: that would echo the token secret
	// straight back.
	res, err := h.ops.SetupProxmoxConnection(r.Context(), req)
	if err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *handler) deleteProxmoxConnection(w http.ResponseWriter, r *http.Request) {
	if err := h.ops.DeleteProxmoxConnection(r.Context()); err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) testProxmoxConnection(w http.ResponseWriter, r *http.Request) {
	tr, err := h.ops.TestProxmoxConnection(r.Context())
	if err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, tr)
}

func (h *handler) listProxmoxGuests(w http.ResponseWriter, r *http.Request) {
	list, err := h.ops.ListProxmoxGuests(r.Context())
	if err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// backupProxmoxGuest handles POST /api/v1/proxmox/backup: one vzdump of
// one guest. The response is the (unverified) dump, or the error;
// a prune problem is reported inside a successful result.
func (h *handler) backupProxmoxGuest(w http.ResponseWriter, r *http.Request) {
	var body proxmox.GuestBackupOptions
	if err := decodeJSON(r, &body); err != nil {
		h.writeErr(w, err, http.StatusBadRequest)
		return
	}
	res, err := h.ops.BackupProxmoxGuest(r.Context(), body)
	if err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// checkProxmoxStorage handles GET /api/v1/proxmox/storage-check?node=&storage=&vmid=&service=:
// how many dumps of the guest on that storage Ballast did not make.
func (h *handler) checkProxmoxStorage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	vmid, err := strconv.Atoi(q.Get("vmid"))
	if err != nil {
		h.writeErr(w, fmt.Errorf("%w: vmid must be a number", ErrValidation), http.StatusBadRequest)
		return
	}
	res, err := h.ops.CheckProxmoxStorage(r.Context(), q.Get("service"), q.Get("node"), q.Get("storage"), vmid)
	if err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// viewProxmoxConfig handles GET /api/v1/proxmox/config: the host's
// settings read live, for viewing or download (read-only).
func (h *handler) viewProxmoxConfig(w http.ResponseWriter, r *http.Request) {
	v, err := h.ops.ViewProxmoxConfig(r.Context())
	if err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

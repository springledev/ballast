package api

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/springledev/ballast/internal/engine"
	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/proxmox"
	"github.com/springledev/keel/dockerhost"
)

// GetProxmoxConnection returns Ballast's configured Proxmox
// connection, or inventory.ErrNotFound when none is set up yet.
func (o *Ops) GetProxmoxConnection(ctx context.Context) (proxmox.Connection, error) {
	c, err := inventory.GetProxmoxConnection(o.DB)
	if err != nil {
		return proxmox.Connection{}, wrapf("Proxmox connection", err)
	}
	return c, nil
}

// ProxmoxConnectionSetup is one "save the Proxmox connection" request:
// the non-secret connection settings plus a write-only token secret.
type ProxmoxConnectionSetup struct {
	Connection proxmox.Connection
	// Credentials, when not empty, are written to the connection's
	// credential file (replacing an earlier one). Empty keeps the
	// token secret an existing connection already has.
	Credentials proxmox.Credentials
	// SkipTest saves without connecting first — for a host that is
	// offline right now. By default the connection is only saved once
	// a connection test passes.
	SkipTest bool
}

// ProxmoxConnectionResult is what SetupProxmoxConnection saved, and
// the connection test's findings (empty when the test was skipped).
type ProxmoxConnectionResult struct {
	Connection proxmox.Connection  `json:"connection"`
	Test       *proxmox.TestResult `json:"test,omitempty"`
}

// SetupProxmoxConnection validates and saves Ballast's Proxmox
// connection. Unless SkipTest is set it first connects with the new
// settings and saves nothing if that fails: a new credential file is
// removed (or the previous one restored), so a typo never leaves a
// half-configured connection behind.
func (o *Ops) SetupProxmoxConnection(ctx context.Context, req ProxmoxConnectionSetup) (res ProxmoxConnectionResult, err error) {
	c := req.Connection
	prev, prevErr := inventory.GetProxmoxConnection(o.DB)
	existed := prevErr == nil
	if prevErr != nil && !errors.Is(prevErr, inventory.ErrNotFound) {
		return res, prevErr
	}

	if !req.Credentials.Empty() {
		c.Credential = proxmox.DefaultCredentialName
	} else if existed && c.Credential == "" {
		c.Credential = prev.Credential
	}
	if err := c.Normalize(); err != nil {
		return res, fmt.Errorf("%w: %w", ErrValidation, err)
	}
	if c.Credential == "" {
		return res, fmt.Errorf("%w: Proxmox connection: an API token secret is required", ErrValidation)
	}

	if !req.Credentials.Empty() {
		// rerr/serr, not err — see SetupDockerHost's identical comment:
		// this must not shadow the named return err, or the deferred
		// rollback below would never fire.
		data, secrets, rerr := proxmox.RenderCredentials(req.Credentials)
		if rerr != nil {
			return res, fmt.Errorf("%w: Proxmox connection: %w", ErrValidation, rerr)
		}
		o.Redact.Add(secrets...)
		files := []dockerhost.File{{Name: c.Credential, Data: data}}
		undo, serr := o.savePrivateFiles(files)
		if serr != nil {
			return res, fmt.Errorf("the Proxmox connection: %w", serr)
		}
		defer func() {
			if err != nil {
				if uerr := undo(); uerr != nil {
					err = fmt.Errorf("%w (and cleaning up: %v)", err, uerr)
				}
			}
		}()
	}

	if !req.SkipTest {
		tr, err := o.testProxmox(ctx, c)
		if err != nil {
			return res, err
		}
		res.Test = &tr
	}
	if err := inventory.PutProxmoxConnection(o.DB, c); err != nil {
		return res, fmt.Errorf("%w: %w", ErrValidation, err)
	}
	res.Connection = c
	return res, nil
}

// TestProxmoxConnection connects to the saved Proxmox connection and
// reports its API version: it checks the address, TLS trust and the
// token, not just reachability.
func (o *Ops) TestProxmoxConnection(ctx context.Context) (proxmox.TestResult, error) {
	c, err := inventory.GetProxmoxConnection(o.DB)
	if err != nil {
		return proxmox.TestResult{}, wrapf("Proxmox connection", err)
	}
	return o.testProxmox(ctx, c)
}

func (o *Ops) testProxmox(ctx context.Context, c proxmox.Connection) (proxmox.TestResult, error) {
	cl, err := o.Proxmox.Client(c)
	if err != nil {
		return proxmox.TestResult{}, o.redactErr(err)
	}
	tr, err := cl.Test(ctx)
	if err != nil {
		return proxmox.TestResult{}, o.redactErr(err)
	}
	return tr, nil
}

// ListProxmoxGuests lists every VM and LXC container on the configured
// Proxmox host, for the guest picker in a service's setup form.
func (o *Ops) ListProxmoxGuests(ctx context.Context) ([]proxmox.Guest, error) {
	c, err := inventory.GetProxmoxConnection(o.DB)
	if err != nil {
		return nil, wrapf("Proxmox connection", err)
	}
	cl, err := o.Proxmox.Client(c)
	if err != nil {
		return nil, o.redactErr(err)
	}
	guests, err := cl.ListGuests(ctx)
	if err != nil {
		return nil, o.redactErr(err)
	}
	return guests, nil
}

// DeleteProxmoxConnection removes Ballast's Proxmox connection, along
// with the credential file Ballast saved for it. A file the operator
// provided (another credential directory) is never touched.
func (o *Ops) DeleteProxmoxConnection(ctx context.Context) error {
	c, err := inventory.GetProxmoxConnection(o.DB)
	if err != nil {
		return wrapf("Proxmox connection", err)
	}
	if err := inventory.DeleteProxmoxConnection(o.DB); err != nil {
		return wrapf("Proxmox connection", err)
	}
	if o.CredentialsDir == "" || c.Credential != proxmox.DefaultCredentialName {
		return nil
	}
	if err := os.Remove(filepath.Join(o.CredentialsDir, c.Credential)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("the Proxmox connection was removed, but its credential file %s could not be: %w", c.Credential, err)
	}
	return nil
}

// BackupProxmoxGuest runs a vzdump of one guest on the configured
// Proxmox host, waits for it, and prunes that guest's older dumps down
// to o.KeepLast. The result is an unverified dump — it is never
// reported as SUCCESS here; the verification drill (spec §14 step 26)
// is what proves it restores.
func (o *Ops) BackupProxmoxGuest(ctx context.Context, opts proxmox.GuestBackupOptions) (proxmox.GuestBackupResult, error) {
	if err := opts.Validate(); err != nil {
		return proxmox.GuestBackupResult{}, fmt.Errorf("%w: %w", ErrValidation, err)
	}
	c, err := inventory.GetProxmoxConnection(o.DB)
	if err != nil {
		return proxmox.GuestBackupResult{}, wrapf("Proxmox connection", err)
	}
	cl, err := o.Proxmox.Client(c)
	if err != nil {
		return proxmox.GuestBackupResult{}, o.redactErr(err)
	}
	res, err := cl.BackupGuest(ctx, opts, proxmox.DefaultPollInterval)
	if err != nil {
		return res, o.redactErr(err)
	}

	// Drill both layers independently (spec §17.4): the guest's stored
	// config, and every service attached to the guest via its own latest
	// verified run. A failure in one never hides the other.
	config := cl.VerifyGuestConfig(ctx, res)
	config.Detail = o.redact(config.Detail)
	inGuest, err := o.inGuestLayers(opts.Node, opts.VMID)
	if err != nil {
		return res, err
	}
	gv := proxmox.Combine(config, inGuest...)
	res.Verification = &gv
	return res, nil
}

// inGuestLayers reports, per service attached to the guest, whether its
// latest run passed verification (state SUCCESS). A service that has
// never run, or whose latest run is amber or failed, fails its layer.
func (o *Ops) inGuestLayers(node string, vmid int) ([]proxmox.LayerResult, error) {
	services, err := inventory.ListServices(o.DB)
	if err != nil {
		return nil, wrapf("services", err)
	}
	var out []proxmox.LayerResult
	for _, s := range services {
		if s.ProxmoxVMID != vmid || s.ProxmoxNode != node {
			continue
		}
		r := proxmox.LayerResult{Service: s.Name}
		runs, err := engine.RecentRuns(o.DB, s.Name, 1)
		switch {
		case err != nil:
			return nil, fmt.Errorf("service %s: %w", s.Name, err)
		case len(runs) == 0:
			r.Detail = fmt.Sprintf("%s has never been backed up, so its in-guest restore is unproven", s.Name)
		case runs[0].State != engine.SUCCESS:
			r.Detail = fmt.Sprintf("the latest backup of %s ended as %s, so its in-guest restore is not verified", s.Name, runs[0].State)
		default:
			r.Passed = true
			r.Detail = fmt.Sprintf("the latest backup of %s passed its restore check", s.Name)
		}
		out = append(out, r)
	}
	return out, nil
}

// putGuestJob saves a Proxmox guest backup job (a service of kind
// proxmox-guest). Proxmox must be connected, and an optional copy
// destination must exist.
func (o *Ops) putGuestJob(ctx context.Context, s inventory.Service) error {
	if _, err := inventory.GetProxmoxConnection(o.DB); err != nil {
		return fmt.Errorf("service %s: %w: connect Ballast to Proxmox first (ballast proxmox connect)", s.Name, ErrValidation)
	}
	for _, t := range s.EffectiveTargets() {
		if _, err := inventory.GetDestination(o.DB, t.Destination); err != nil {
			return wrapf("service "+s.Name, err)
		}
	}
	if s.DockerHost == "" {
		s.DockerHost = dockerhost.LocalName
	}
	if err := inventory.PutService(o.DB, s); err != nil {
		return fmt.Errorf("%w: %w", ErrValidation, wrapf("service "+s.Name, err))
	}
	return nil
}

// StorageCheck says what a Proxmox storage already holds of one guest.
type StorageCheck struct {
	// Total is how many dumps of the guest are on the storage.
	Total int `json:"total"`
	// Foreign is how many of them Ballast did not make. Ballast never
	// deletes these.
	Foreign int `json:"foreign"`
	// Warning is plain-English advice, empty when there is nothing to say.
	Warning string `json:"warning,omitempty"`
}

// CheckProxmoxStorage counts the guest's dumps on storage that service
// did not make, so the UI can say that other backups share the storage
// (Ballast leaves them alone) and recommend a dedicated one.
func (o *Ops) CheckProxmoxStorage(ctx context.Context, service, node, storage string, vmid int) (StorageCheck, error) {
	c, err := inventory.GetProxmoxConnection(o.DB)
	if err != nil {
		return StorageCheck{}, wrapf("Proxmox connection", err)
	}
	cl, err := o.Proxmox.Client(c)
	if err != nil {
		return StorageCheck{}, o.redactErr(err)
	}
	dumps, err := cl.ListDumps(ctx, node, storage, vmid)
	if err != nil {
		return StorageCheck{}, o.redactErr(err)
	}
	owned, err := inventory.OwnedProxmoxDumps(o.DB, service, node, storage)
	if err != nil {
		return StorageCheck{}, err
	}
	mine := map[string]bool{}
	for _, v := range owned {
		mine[v] = true
	}
	res := StorageCheck{Total: len(dumps)}
	for _, d := range dumps {
		if !mine[d.Volume] {
			res.Foreign++
		}
	}
	if res.Foreign > 0 {
		res.Warning = fmt.Sprintf("%s already holds %d backup(s) of this guest that Ballast did not make. Ballast will never delete them, so they will not count toward \"keep last\". A storage used only by Ballast keeps things tidy.", storage, res.Foreign)
	}
	return res, nil
}

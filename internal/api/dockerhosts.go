package api

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/springledev/ballast/internal/dockerhost"
	"github.com/springledev/ballast/internal/inventory"
)

// dockerFor resolves the named Docker host ("" = the local host) into
// the handle its Docker commands run through.
func (o *Ops) dockerFor(name string) (dockerhost.Docker, error) {
	if name == "" {
		name = dockerhost.LocalName
	}
	h, err := inventory.GetDockerHost(o.DB, name)
	if err != nil {
		if errors.Is(err, inventory.ErrNotFound) {
			return dockerhost.Docker{}, fmt.Errorf("%w: %w", ErrValidation, err)
		}
		return dockerhost.Docker{}, err
	}
	d, err := o.DockerHosts.Docker(h)
	if err != nil {
		return dockerhost.Docker{}, o.redactErr(err)
	}
	return d, nil
}

// ListDockerHosts lists every Docker host, the built-in local host
// first.
func (o *Ops) ListDockerHosts(ctx context.Context) ([]dockerhost.Host, error) {
	return inventory.ListDockerHosts(o.DB)
}

// GetDockerHost returns one Docker host.
func (o *Ops) GetDockerHost(ctx context.Context, name string) (dockerhost.Host, error) {
	h, err := inventory.GetDockerHost(o.DB, name)
	if err != nil {
		return dockerhost.Host{}, wrapf("docker host "+name, err)
	}
	return h, nil
}

// DockerHostSetup is one "save Docker host" request: the non-secret
// host settings plus write-only credentials.
type DockerHostSetup struct {
	Host dockerhost.Host
	// Credentials, when not empty, are written to the host's
	// credential files (replacing earlier ones). Empty keeps the files
	// an existing host of the same kind already has.
	Credentials dockerhost.Credentials
	// SkipTest saves without connecting first — for a host that is
	// offline right now. By default a host is only saved once a
	// connection test passes.
	SkipTest bool
}

// DockerHostResult is what SetupDockerHost saved, and the connection
// test's findings (empty when the test was skipped).
type DockerHostResult struct {
	Host dockerhost.Host        `json:"host"`
	Test *dockerhost.TestResult `json:"test,omitempty"`
}

// SetupDockerHost validates and saves a Docker host. Unless SkipTest
// is set it first connects with the new settings and saves nothing if
// that fails: new credential files are removed (or the previous ones
// restored), so a typo never leaves a half-configured host behind.
func (o *Ops) SetupDockerHost(ctx context.Context, req DockerHostSetup) (res DockerHostResult, err error) {
	h := req.Host
	if err := inventory.ValidateName(h.Name); err != nil {
		return res, fmt.Errorf("%w: docker host: %w", ErrValidation, err)
	}
	prev, prevErr := inventory.GetDockerHost(o.DB, h.Name)
	existed := prevErr == nil
	if prevErr != nil && !errors.Is(prevErr, inventory.ErrNotFound) {
		return res, prevErr
	}

	if !req.Credentials.Empty() {
		if h.Kind == dockerhost.Local {
			return res, fmt.Errorf("%w: docker host %s: a local host does not use credentials", ErrValidation, h.Name)
		}
		h.Credential = dockerhost.DefaultCredentialName(h.Name)
	} else if h.Kind != dockerhost.Local && existed && prev.Kind == h.Kind && h.Credential == "" {
		h.Credential = prev.Credential
	}
	if h.Kind == dockerhost.RemoteTLS && h.Credential == "" {
		return res, fmt.Errorf("%w: docker host %s: a TLS host needs its CA certificate, client certificate and client key; paste them into the form (or use --tls-ca/--tls-cert/--tls-key on the command line)", ErrValidation, h.Name)
	}
	if h.Name == dockerhost.LocalName && h.Kind != dockerhost.Local {
		return res, fmt.Errorf("%w: docker host %s: the built-in local host must stay a local host; give a remote host its own name", ErrValidation, h.Name)
	}
	if err := h.Normalize(); err != nil {
		return res, fmt.Errorf("%w: %w", ErrValidation, err)
	}

	if !req.Credentials.Empty() {
		// rerr/serr, not err: this block must not shadow the named
		// return err, or the deferred rollback below would close over
		// a local variable that's never set again instead of the
		// function's real outcome, and would silently never fire.
		files, secrets, rerr := dockerhost.RenderCredentials(h.Kind, h.Credential, req.Credentials)
		if rerr != nil {
			return res, fmt.Errorf("%w: docker host %s: %w", ErrValidation, h.Name, rerr)
		}
		o.Redact.Add(secrets...)
		undo, serr := o.savePrivateFiles(files)
		if serr != nil {
			return res, fmt.Errorf("docker host %s: %w", h.Name, serr)
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
		tr, err := o.testHost(ctx, h)
		if err != nil {
			return res, err
		}
		res.Test = &tr
	}
	saved, err := inventory.PutDockerHost(o.DB, h)
	if err != nil {
		return res, fmt.Errorf("%w: %w", ErrValidation, err)
	}
	res.Host = saved
	return res, nil
}

// TestDockerHost connects to a saved Docker host and reports the
// daemon's version: it checks the address, the credentials and the
// user's permission to use Docker there, not just reachability.
func (o *Ops) TestDockerHost(ctx context.Context, name string) (dockerhost.TestResult, error) {
	h, err := inventory.GetDockerHost(o.DB, name)
	if err != nil {
		return dockerhost.TestResult{}, wrapf("docker host "+name, err)
	}
	return o.testHost(ctx, h)
}

func (o *Ops) testHost(ctx context.Context, h dockerhost.Host) (dockerhost.TestResult, error) {
	if o.Runner == nil {
		return dockerhost.TestResult{}, fmt.Errorf("docker host %s: connection tests are not configured (internal wiring error)", h.Name)
	}
	d, err := o.DockerHosts.Docker(h)
	if err != nil {
		return dockerhost.TestResult{}, o.redactErr(err)
	}
	tr, err := dockerhost.Test(ctx, o.Runner, d)
	if err != nil {
		return dockerhost.TestResult{}, o.redactErr(err)
	}
	return tr, nil
}

// DeleteDockerHost removes a Docker host no service uses, along with
// the credential files Ballast saved for it. Files an operator
// provided (another credential directory) are never touched.
func (o *Ops) DeleteDockerHost(ctx context.Context, name string) error {
	h, err := inventory.GetDockerHost(o.DB, name)
	if err != nil {
		return wrapf("docker host "+name, err)
	}
	if err := inventory.DeleteDockerHost(o.DB, name); err != nil {
		if !errors.Is(err, inventory.ErrInUse) && !errors.Is(err, inventory.ErrNotFound) {
			err = fmt.Errorf("%w: %w", ErrValidation, err)
		}
		return wrapf("docker host "+name, err)
	}
	if o.CredentialsDir == "" || h.Credential != dockerhost.DefaultCredentialName(name) {
		return nil
	}
	ca, cert, key := dockerhost.TLSFiles(h.Credential)
	for _, f := range []string{h.Credential, ca, cert, key} {
		if err := os.Remove(filepath.Join(o.CredentialsDir, f)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("docker host %s was removed, but its credential file %s could not be: %w", name, f, err)
		}
	}
	return nil
}

// savePrivateFiles writes each file (0600, atomically) into the
// writable credential directory, refusing when an operator-provided
// file of the same name would shadow it. It returns a func undoing
// every write: new files removed, replaced files restored.
func (o *Ops) savePrivateFiles(files []dockerhost.File) (func() error, error) {
	if o.CredentialsDir == "" {
		return nil, fmt.Errorf("%w: no writable credential directory is configured", ErrValidation)
	}
	for _, f := range files {
		for _, dir := range o.CredentialSearchDirs {
			if dir == o.CredentialsDir {
				continue
			}
			if _, err := os.Lstat(filepath.Join(dir, f.Name)); err == nil {
				return nil, fmt.Errorf("%w: a credential file named %q already exists in %s (likely provided by the operator); it takes precedence over anything Ballast saves itself, so edit or replace that file directly instead", ErrValidation, f.Name, dir)
			}
		}
	}
	if err := os.MkdirAll(o.CredentialsDir, 0o700); err != nil {
		return nil, fmt.Errorf("creating credential directory %s: %w", o.CredentialsDir, err)
	}
	type saved struct {
		path    string
		prev    []byte
		existed bool
	}
	var done []saved
	undo := func() error {
		var errs []error
		for i := len(done) - 1; i >= 0; i-- {
			s := done[i]
			if s.existed {
				if err := writePrivateFile(s.path, s.prev); err != nil {
					errs = append(errs, fmt.Errorf("restoring the previous credential file %s: %w", s.path, err))
				}
			} else if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
				errs = append(errs, fmt.Errorf("removing the unused credential file %s: %w", s.path, err))
			}
		}
		return errors.Join(errs...)
	}
	for _, f := range files {
		path := filepath.Join(o.CredentialsDir, f.Name)
		prev, readErr := os.ReadFile(path)
		if readErr != nil && !os.IsNotExist(readErr) {
			_ = undo()
			return nil, fmt.Errorf("reading existing credential file %s: %w", path, readErr)
		}
		if err := writePrivateFile(path, f.Data); err != nil {
			_ = undo()
			return nil, fmt.Errorf("saving credentials to %s: %w", path, err)
		}
		done = append(done, saved{path: path, prev: prev, existed: readErr == nil})
	}
	return undo, nil
}

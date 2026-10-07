package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/springledev/ballast/internal/backend"
	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/protondrive"
)

// protonClient builds the Proton Drive client for a destination. Its
// session folder is a 0700 folder under the credential directory.
func (o *Ops) protonClient(dest inventory.Destination) *protondrive.Client {
	return &protondrive.Client{Bin: o.ProtonBin, StateDir: protondrive.StateDirFor(o.CredentialsDir, dest.Name), Runner: o.Runner}
}

// protonDestination returns the named destination, which must be a
// Proton Drive copy.
func (o *Ops) protonDestination(name string) (inventory.Destination, error) {
	dest, err := inventory.GetDestination(o.DB, name)
	if err != nil {
		return inventory.Destination{}, wrapf("destination "+name, err)
	}
	if dest.Type != backend.ProtonDrive {
		return inventory.Destination{}, fmt.Errorf("destination %s: %w: this is a %s destination, not a Proton Drive copy", name, ErrValidation, dest.Type)
	}
	return dest, nil
}

// ProtonSignIn starts Proton Drive's browser sign-in for a Proton Drive
// destination and returns at once; poll ProtonSignInStatus for the link
// to show. Starting again abandons an earlier unfinished sign-in. The
// Proton password and 2FA code are typed on Proton's page, never into
// Ballast.
func (o *Ops) ProtonSignIn(ctx context.Context, name string) (protondrive.LoginInfo, error) {
	dest, err := o.protonDestination(name)
	if err != nil {
		return protondrive.LoginInfo{}, err
	}
	l, err := o.protonClient(dest).StartLogin(protondrive.FolderFor(dest.Settings.Path))
	if err != nil {
		return protondrive.LoginInfo{}, o.redactErr(wrapf("destination "+name, err))
	}
	o.protonMu.Lock()
	if old := o.protonLogins[name]; old != nil {
		old.Cancel()
	}
	if o.protonLogins == nil {
		o.protonLogins = map[string]*protondrive.Login{}
	}
	o.protonLogins[name] = l
	o.protonMu.Unlock()
	return l.Info(), nil
}

// ProtonSignInStatus reports the sign-in started by ProtonSignIn.
func (o *Ops) ProtonSignInStatus(ctx context.Context, name string) (protondrive.LoginInfo, error) {
	if _, err := o.protonDestination(name); err != nil {
		return protondrive.LoginInfo{}, err
	}
	o.protonMu.Lock()
	l := o.protonLogins[name]
	o.protonMu.Unlock()
	if l == nil {
		return protondrive.LoginInfo{}, fmt.Errorf("destination %s: %w: no sign-in is in progress; start one first", name, inventory.ErrNotFound)
	}
	return l.Info(), nil
}

// testProton checks that a Proton Drive destination can be reached with
// the saved login.
func (o *Ops) testProton(ctx context.Context, dest inventory.Destination) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	err := o.protonClient(dest).TestConnection(ctx, protondrive.FolderFor(dest.Settings.Path))
	switch {
	case err == nil:
		return nil
	case errors.Is(err, protondrive.ErrSignedOut):
		return o.redactErr(fmt.Errorf("destination %s: Proton Drive needs you to %s (Destinations page, \"Sign in to Proton Drive\", or `ballast destination sign-in %s`): %w", dest.Name, protondrive.SignInAgainText, dest.Name, err))
	}
	return o.redactErr(fmt.Errorf("could not use destination %s: %w", dest.Name, err))
}

// protonDoctor reports on the Proton Drive command-line tool when any
// destination uses it.
func (o *Ops) protonDoctor(ctx context.Context, dests []inventory.Destination, add func(area, name string, st DoctorStatus, detail string)) {
	var first *inventory.Destination
	for i := range dests {
		if dests[i].Type == backend.ProtonDrive {
			first = &dests[i]
			break
		}
	}
	if first == nil {
		return
	}
	c := o.protonClient(*first)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	v, tested, err := c.Version(ctx)
	switch {
	case errors.Is(err, protondrive.ErrNotInstalled):
		add("proton-drive", "proton-drive", DoctorFail, fmt.Sprintf("The Proton Drive command-line tool is not installed, so the Proton Drive copy cannot run. Download it from %s and put it on the PATH.", protondrive.DownloadURL))
	case err != nil:
		add("proton-drive", "proton-drive", DoctorFail, "The Proton Drive command-line tool could not be run: "+o.redact(err.Error()))
	case !tested:
		add("proton-drive", "proton-drive", DoctorWarn, fmt.Sprintf("Proton Drive tool %s is installed. Ballast was tested with the %s series, so copies may behave differently.", v, protondrive.TestedVersions))
	default:
		add("proton-drive", "proton-drive", DoctorOK, "Proton Drive tool "+v+" is installed")
	}
}

// --- HTTP --------------------------------------------------------------

func (h *handler) protonSignIn(w http.ResponseWriter, r *http.Request) {
	info, err := h.ops.ProtonSignIn(r.Context(), pathName(r))
	if err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusAccepted, info)
}

func (h *handler) protonSignInStatus(w http.ResponseWriter, r *http.Request) {
	info, err := h.ops.ProtonSignInStatus(r.Context(), pathName(r))
	if err != nil {
		h.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

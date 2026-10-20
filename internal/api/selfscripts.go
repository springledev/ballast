package api

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// scriptRef is one local script a catalog entry runs.
type scriptRef struct {
	service string
	path    string
	// real is the path with symlinks resolved; empty when missing.
	real string
	// problem is empty when the script can be saved, else a plain
	// English reason it cannot.
	problem string
	// credential is true when the script was left out because it lives
	// in (or points into) a credentials folder.
	credential bool
}

// referencedScripts lists the local scripts that exec strategies and
// exec-script verifiers in the loaded catalog point to, one per distinct
// path (sorted). Templated or relative-to-PATH names that cannot be
// found are reported as missing.
func (o *Ops) referencedScripts() []scriptRef {
	if o.Catalog == nil {
		return nil
	}
	seen := map[string]*scriptRef{}
	for _, e := range o.Catalog.List() {
		if e.Backup.Exec == nil {
			continue
		}
		cands := []string{e.Backup.Exec.Script}
		if e.Verify.Method == "exec-script" || containsStr(e.Verify.MethodList(), "exec-script") {
			cands = append(cands, e.Backup.Exec.VerifyScript)
		}
		for _, c := range cands {
			c = strings.TrimSpace(c)
			if c == "" || strings.Contains(c, "{{") {
				continue
			}
			if !strings.Contains(c, "/") {
				if lp, err := exec.LookPath(c); err == nil {
					c = lp
				}
			}
			c = filepath.Clean(c)
			if !filepath.IsAbs(c) {
				if abs, err := filepath.Abs(c); err == nil {
					c = abs
				}
			}
			if _, ok := seen[c]; ok {
				continue
			}
			seen[c] = o.checkScript(e.Service, c)
		}
	}
	out := make([]scriptRef, 0, len(seen))
	for _, r := range seen {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out
}

func containsStr(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// checkScript decides whether path can go into a self-backup.
func (o *Ops) checkScript(service, path string) *scriptRef {
	r := &scriptRef{service: service, path: path}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		r.problem = "is missing"
		return r
	}
	r.real = real
	if o.inCredentialArea(path) || o.inCredentialArea(real) {
		r.credential, r.problem = true, "is in a credentials folder, so it is never saved (secrets are not backed up)"
		return r
	}
	fi, err := os.Stat(real)
	if err != nil || !fi.Mode().IsRegular() {
		r.problem = "is not a regular file"
	}
	return r
}

// inCredentialArea reports whether p is inside a credentials folder.
func (o *Ops) inCredentialArea(p string) bool {
	for _, d := range o.credDirs() {
		if d == "" {
			continue
		}
		for _, base := range []string{d, evalOr(d)} {
			rel, err := filepath.Rel(base, p)
			if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return true
			}
		}
	}
	return false
}

func evalOr(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// scriptDoctorChecks reports, for each script a catalog entry runs,
// whether the self-backup will cover it.
func (o *Ops) scriptDoctorChecks() []DoctorCheck {
	dests, _ := o.selfDestinations()
	var out []DoctorCheck
	for _, r := range o.referencedScripts() {
		c := DoctorCheck{Area: "scripts", Name: r.path}
		switch {
		case r.problem != "":
			c.Status = DoctorWarn
			c.Detail = fmt.Sprintf("%s runs %s, which %s. A restore would not bring it back.", r.service, r.path, r.problem)
		case len(dests) == 0:
			c.Status = DoctorWarn
			c.Detail = fmt.Sprintf("%s runs %s, but no destination exists to back Ballast's own settings up to, so no backup covers it.", r.service, r.path)
		default:
			c.Status = DoctorOK
			c.Detail = fmt.Sprintf("%s runs %s; it is saved with Ballast's own settings backup.", r.service, r.path)
		}
		c.Detail = o.redact(c.Detail)
		out = append(out, c)
	}
	return out
}

// stageScripts copies every savable script under scripts/ in dir,
// keeping its full path so a restore knows where it belongs.
func (o *Ops) stageScripts(dir string) error {
	for _, r := range o.referencedScripts() {
		if r.problem != "" {
			o.logf("%s: not saving script %s: it %s", SelfBackupService, r.path, r.problem)
			continue
		}
		dst := filepath.Join(dir, "scripts", r.path)
		if err := copyFile(r.real, dst); err != nil {
			return fmt.Errorf("self-backup: copying script %s: %w", r.path, err)
		}
		if fi, err := os.Stat(r.real); err == nil {
			_ = os.Chmod(dst, fi.Mode().Perm()&0o700|0o600)
		}
	}
	return nil
}

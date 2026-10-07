package verify

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/springledev/ballast/internal/catalog"
	"github.com/springledev/keel/runner"
)

// MethodExecScript is the catalog name of the exec-script verification
// method (spec §19.9).
const MethodExecScript = "exec-script"

// artifactDirEnv tells the script where the backup's artifacts are. It
// is the same variable the exec backup script receives.
const artifactDirEnv = "BALLAST_ARTIFACT_DIR"

// ExecScript implements the exec-script verification method: it runs
// the entry's exec.verifyScript with BALLAST_ARTIFACT_DIR pointing at
// the backup's artifacts, and exit code 0 means "verified". An entry
// with no verifyScript cannot be proven restorable, so the checker
// reports the result as unverifiable (amber) instead of passing it.
type ExecScript struct {
	service string
	script  string
	opts    Options
}

func newExecScript(entry *catalog.Entry, opts Options) (Checker, error) {
	if entry.Backup.Exec == nil {
		return nil, fmt.Errorf("service %s: exec-script verification needs the exec backup strategy", entry.Service)
	}
	if opts.Runner == nil {
		return nil, fmt.Errorf("service %s: exec-script verification needs a command runner", entry.Service)
	}
	return &ExecScript{service: entry.Service, script: entry.Backup.Exec.VerifyScript, opts: opts}, nil
}

// Method implements Checker.
func (*ExecScript) Method() string { return MethodExecScript }

// Check implements Checker.
func (c *ExecScript) Check(ctx context.Context, req Request) Result {
	start := time.Now()
	res := Result{Method: c.Method()}
	done := func(detail string) Result {
		res.Detail = detail
		res.Duration = time.Since(start)
		return res
	}
	if c.script == "" {
		res.Unverifiable = true
		return done(fmt.Sprintf("service %s: this service has no verify script, so Ballast cannot prove the backup can be restored. Add a verifyScript to its catalog entry to turn this green.", req.Service))
	}
	if len(req.Artifacts) == 0 {
		return done(fmt.Sprintf("service %s: the backup left no files for the verify script to check", req.Service))
	}
	dir := filepath.Dir(req.Artifacts[0].Path)
	env := append(append([]string(nil), req.Env...), artifactDirEnv+"="+dir)
	out, err := c.opts.Runner.Run(ctx, nil, nil, runner.Command{Name: c.script, Env: env})
	res.Output = strings.TrimSpace(string(out.Stderr))
	if err != nil {
		return done(fmt.Sprintf("service %s: verify script %s did not succeed: %v", req.Service, c.script, err))
	}
	res.Passed = true
	return done(fmt.Sprintf("verify script %s exited 0 for the backup files in %s", c.script, dir))
}

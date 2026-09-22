package strategy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/springledev/ballast/internal/catalog"
	"github.com/springledev/ballast/internal/runner"
)

// artifactDirEnv is the environment variable telling an exec script
// where to leave its backup artifacts.
const artifactDirEnv = "BALLAST_ARTIFACT_DIR"

// Exec implements the exec strategy (spec §4.2), the reserved
// Phase-2 external contract: a user-supplied script runs with
// BALLAST_ARTIFACT_DIR pointing at the work directory; exit 0 means
// the script produced its dump artifact(s) there. Verification is the
// paired verifyScript of the catalog entry, run by the verification
// phase — not here. v0.1 ships no per-service exec logic; the
// contract is tested with stub scripts only (spec §12).
type Exec struct {
	entry *catalog.Entry
	opts  Options
}

// Backup runs the entry's script and collects every top-level entry
// it left in the work directory as an artifact. A script that exits 0
// but leaves nothing violates the contract and fails loudly: silently
// uploading nothing is how backups lie.
func (s *Exec) Backup(ctx context.Context, req Request) (*Result, error) {
	if err := ensureWorkDir(req.WorkDir); err != nil {
		return nil, fmt.Errorf("service %s: %w", s.entry.Service, err)
	}

	script := s.entry.Backup.Exec.Script
	env := append(append([]string(nil), req.env()...), artifactDirEnv+"="+req.WorkDir)
	cmd := runner.Command{Name: script, Args: s.entry.Backup.Exec.Args, Env: env}
	if _, err := s.opts.runnerFor(req).Run(ctx, nil, nil, cmd); err != nil {
		return nil, fmt.Errorf("service %s: exec script %s: %w", s.entry.Service, script, err)
	}

	entries, err := os.ReadDir(req.WorkDir)
	if err != nil {
		return nil, fmt.Errorf("service %s: reading work directory after exec script: %w", s.entry.Service, err)
	}
	res := &Result{}
	for _, e := range entries {
		res.Artifacts = append(res.Artifacts, Artifact{
			Name: e.Name(),
			Path: filepath.Join(req.WorkDir, e.Name()),
		})
	}
	if len(res.Artifacts) == 0 {
		return nil, fmt.Errorf("service %s: exec script %s exited 0 but left no artifacts in the work directory", s.entry.Service, script)
	}
	return res, nil
}

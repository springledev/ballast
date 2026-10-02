package verify

import (
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/springledev/ballast/internal/catalog"
)

// ComposeConfig implements the compose-config verification method
// (spec §18.4): the captured compose snapshot is unpacked into a
// private temp folder and `docker compose config -q` parses it. Every
// file named in the snapshot's manifest must be in the archive, and
// Compose must be able to resolve the whole stack from just those
// files — a missing env_file target or a broken override fails here,
// not on the day of a restore. `config` never contacts a Docker
// daemon and never starts anything.
type ComposeConfig struct {
	opts Options
}

func newComposeConfig(entry *catalog.Entry, opts Options) (Checker, error) {
	if opts.Runner == nil {
		return nil, fmt.Errorf("service %s: compose-config verification needs a command runner", entry.Service)
	}
	return &ComposeConfig{opts: opts}, nil
}

// Method implements Checker.
func (*ComposeConfig) Method() string { return MethodComposeConfig }

// Check implements Checker.
func (c *ComposeConfig) Check(ctx context.Context, req Request) Result {
	start := time.Now()
	res := Result{Method: c.Method()}
	fail := func(format string, a ...any) Result {
		res.Detail = fmt.Sprintf("service %s: ", req.Service) + fmt.Sprintf(format, a...)
		res.Duration = time.Since(start)
		return res
	}

	var tarPath, manifestPath string
	for _, a := range req.Artifacts {
		switch a.Name {
		case "compose-config":
			tarPath = a.Path
		case "compose-manifest":
			manifestPath = a.Path
		}
	}
	if tarPath == "" || manifestPath == "" {
		return fail("the backup has no compose snapshot to check")
	}
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return fail("reading the snapshot manifest: %v", err)
	}
	var m struct {
		ConfigFiles []string `json:"config_files_merge_order"`
		Captured    []string `json:"captured_files"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return fail("the snapshot manifest is unreadable: %v", err)
	}

	dir, err := os.MkdirTemp("", "ballast-compose-verify-")
	if err != nil {
		return fail("creating a temporary folder: %v", err)
	}
	defer os.RemoveAll(dir)
	if err := untarFlat(tarPath, dir); err != nil {
		return fail("unpacking the snapshot: %v", err)
	}
	for _, name := range m.Captured {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(name))); err != nil {
			return fail("%s is listed in the snapshot but is missing from the archive", name)
		}
	}
	if len(m.ConfigFiles) == 0 {
		return fail("the snapshot lists no compose files")
	}

	args := []string{"compose", "--project-directory", dir}
	for _, f := range m.ConfigFiles {
		args = append(args, "-f", filepath.Join(dir, f))
	}
	args = append(args, "config", "-q")
	var errBuf strings.Builder
	if _, err := c.opts.Runner.Run(ctx, nil, &errBuf, c.opts.Docker.Command(nil, args...)); err != nil {
		res.Output = errBuf.String()
		return fail("`docker compose config` could not read the captured files, so this snapshot may not rebuild the stack: %v (is the Docker Compose plugin installed?)", err)
	}
	res.Passed = true
	res.Detail = fmt.Sprintf("service %s: Docker Compose read all %d captured files (merge order: %s)", req.Service, len(m.Captured), strings.Join(m.ConfigFiles, " → "))
	res.Duration = time.Since(start)
	return res
}

// untarFlat extracts regular files from a tar into dir, refusing any
// member whose cleaned path would leave dir.
func untarFlat(tarPath, dir string) error {
	f, err := os.Open(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		name := path.Clean(h.Name)
		if path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("archive member %q escapes the folder", h.Name)
		}
		dst := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		_, cpErr := io.Copy(out, tr)
		if closeErr := out.Close(); cpErr == nil {
			cpErr = closeErr
		}
		if cpErr != nil {
			return cpErr
		}
	}
}

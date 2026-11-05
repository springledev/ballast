package strategy

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/springledev/ballast/internal/catalog"
	"github.com/springledev/keel/runner"
)

// stopStartArtifact is the name of the single tar artifact the
// stop-start strategy produces from the entry's data directories.
const stopStartArtifact = "data"

// restartTimeout bounds the deferred container restart so a hung
// docker daemon cannot block cleanup forever.
const restartTimeout = 2 * time.Minute

// stopTimeout bounds one `docker stop`, which a cancel does not cut
// short: Docker's default grace period is 10 seconds and a container
// may ask for more, so this leaves a generous margin.
const stopTimeout = 2 * time.Minute

// restartAttempts and restartRetryDelay control how often a restart is
// retried until the container is confirmed running.
const restartAttempts = 10

var restartRetryDelay = time.Second

// StopStart implements the stop-start strategy (spec §4.2): stop the
// catalog's containers, archive their data directories into one tar
// artifact, and start the containers again. The restart is guaranteed
// by defer — it runs on success, on failure, and on cancellation —
// because a backup must never leave a service down (spec §4.1 cleanup
// obligation).
type StopStart struct {
	entry *catalog.Entry
	opts  Options
}

// Backup stops the containers in catalog order, tars every
// backup.files path into <workdir>/data.tar while they are down, and
// starts the containers again in catalog order. Stopping is
// all-or-nothing: if a stop fails, the containers already stopped are
// started again before the error returns.
func (s *StopStart) Backup(ctx context.Context, req Request) (res *Result, retErr error) {
	if err := ensureWorkDir(req.WorkDir); err != nil {
		return nil, fmt.Errorf("service %s: %w", s.entry.Service, err)
	}
	containers := s.entry.Backup.StopStart.Containers

	// Folders on the Docker host are looked up and checked first: a
	// wrong path must fail before anything is stopped.
	var hostSrcs []hostSource
	if len(s.entry.Backup.HostPaths) > 0 {
		var err error
		if hostSrcs, err = s.opts.prepareHostPaths(ctx, s.entry.Service, s.entry.Backup.HostPaths); err != nil {
			return nil, err
		}
	}
	if err := s.stopAll(ctx, containers); err != nil {
		return nil, err
	}

	// Guaranteed restart (spec §4.2). It runs with a cancellation-free
	// context: when the run was cancelled, the cancelled context would
	// otherwise make the restart fail instantly and leave the service
	// down. A restart failure never masks the original error — both
	// are reported.
	defer func() {
		if err := s.startAll(containers); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("service %s: starting containers again: %w", s.entry.Service, err))
		}
	}()

	if hostSrcs != nil {
		a, err := s.opts.archiveHostPaths(ctx, req.WorkDir, hostSrcs)
		if err != nil {
			return nil, fmt.Errorf("service %s: archiving host folders: %w", s.entry.Service, err)
		}
		return &Result{Artifacts: []Artifact{a}}, nil
	}
	tarPath := filepath.Join(req.WorkDir, stopStartArtifact+".tar")
	skipped, err := s.tarPaths(ctx, tarPath)
	if err != nil {
		return nil, fmt.Errorf("service %s: archiving data directories: %w", s.entry.Service, err)
	}
	return &Result{
		Artifacts: []Artifact{{Name: stopStartArtifact, Path: tarPath, Format: "tar"}},
		Skipped:   skipped,
	}, nil
}

// Preflight checks, for hostPaths entries, that every folder exists on
// the service's Docker host, before anything is stopped.
func (s *StopStart) Preflight(ctx context.Context) error {
	if len(s.entry.Backup.HostPaths) == 0 {
		return nil
	}
	_, err := s.opts.prepareHostPaths(ctx, s.entry.Service, s.entry.Backup.HostPaths)
	return err
}

// stopAll stops the containers in order. Stopping is all-or-nothing:
// if a stop fails or the run is stopped meanwhile, the containers
// already stopped are started again before the error returns, and so
// is the one whose stop was under way. The caller only needs to arrange
// the restart once stopAll succeeded.
//
// Each `docker stop` runs on its own bounded context that a cancel
// cannot end: killing the docker client part-way does not stop the
// daemon from finishing the stop, so a restart issued meanwhile would
// be a no-op and the container would end up down. The cancel is acted
// on as soon as the stop under way has finished.
func (s *StopStart) stopAll(ctx context.Context, containers []string) error {
	for i, c := range containers {
		if err := ctx.Err(); err != nil {
			// Not stopped yet: only the ones before it need starting.
			return s.undoStops(containers[:i], fmt.Errorf("service %s: stopping containers: %w", s.entry.Service, err))
		}
		stopCtx, cancel := context.WithTimeout(context.Background(), stopTimeout)
		_, err := s.opts.Runner.Run(stopCtx, nil, nil, s.dockerCmd("stop", c))
		cancel()
		if err != nil {
			// A failed `docker stop` may still have stopped it.
			return s.undoStops(containers[:i+1], fmt.Errorf("service %s: stopping container %s: %w", s.entry.Service, c, err))
		}
		if err := ctx.Err(); err != nil {
			return s.undoStops(containers[:i+1], fmt.Errorf("service %s: stopping containers: %w", s.entry.Service, err))
		}
	}
	return nil
}

// undoStops starts containers again after stopAll gave up, joining any
// restart failure to cause.
func (s *StopStart) undoStops(containers []string, cause error) error {
	if rerr := s.startAll(containers); rerr != nil {
		return errors.Join(cause, rerr)
	}
	return cause
}

// dockerCmd builds a docker CLI invocation.
func (s *StopStart) dockerCmd(args ...string) runner.Command {
	return s.opts.Docker.Command(nil, args...)
}

// startAll starts every container and checks each is really running,
// attempting each even when others fail — leaving a container down is
// the worst outcome — and joins the failures. It runs on a fresh,
// bounded context so cancellation of the backup run cannot prevent the
// restart.
func (s *StopStart) startAll(containers []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), restartTimeout)
	defer cancel()
	var errs []error
	for _, c := range containers {
		if err := s.startRunning(ctx, c); err != nil {
			errs = append(errs, fmt.Errorf("starting container %s: %w", c, err))
		}
	}
	return errors.Join(errs...)
}

// startRunning starts container c and confirms with `docker inspect`
// that it is running, trying again a few times: a start issued while
// the daemon is still finishing a stop succeeds but does nothing. A
// start Docker refuses is reported at once.
func (s *StopStart) startRunning(ctx context.Context, c string) error {
	var last error
	for attempt := 0; attempt < restartAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return errors.Join(last, ctx.Err())
			case <-time.After(restartRetryDelay):
			}
		}
		if _, err := s.opts.Runner.Run(ctx, nil, nil, s.dockerCmd("start", c)); err != nil {
			return err // Docker refused outright; trying again will not help
		}
		res, err := s.opts.Runner.Run(ctx, nil, nil, s.dockerCmd("inspect", "--format", "{{.State.Running}}", c))
		if err != nil {
			last = fmt.Errorf("checking it is running: %w", err)
			continue
		}
		if strings.TrimSpace(string(res.Stdout)) == "true" {
			return nil
		}
		last = errors.New("it is not running after being started; start it yourself with `docker start " + c + "`")
	}
	return last
}

// tarPaths archives every backup.files path into one tar at dest and
// returns the paths it deliberately skipped (sockets, devices, …).
// Each path is stored under its base name, mirroring
// `tar -C <parent> <base>`. The archive is created 0600: it holds
// service data.
func (s *StopStart) tarPaths(ctx context.Context, dest string) ([]string, error) {
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("creating archive file: %w", err)
	}
	tw := tar.NewWriter(f)

	var skipped []string
	for _, spec := range s.entry.Backup.Files {
		if err := ctx.Err(); err != nil {
			tw.Close()
			f.Close()
			return nil, err
		}
		root := filepath.Clean(spec.Path)
		if !filepath.IsAbs(root) {
			tw.Close()
			f.Close()
			return nil, fmt.Errorf("backup path %q must be absolute (e.g. /srv/data), not relative", spec.Path)
		}
		if err := tarOne(ctx, tw, root, spec.Exclude, &skipped); err != nil {
			tw.Close()
			f.Close()
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		f.Close()
		return nil, fmt.Errorf("finishing archive: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("writing archive file: %w", err)
	}
	return skipped, nil
}

// tarOne walks root and writes every entry to tw, rooted at root's
// base name. Regular files, directories and symlinks are archived;
// anything else (sockets, devices, fifos) cannot be restored
// meaningfully and is skipped — recorded in skipped so the run log
// can name it.
func tarOne(ctx context.Context, tw *tar.Writer, root string, excludes []string, skipped *[]string) error {
	err := filepath.Walk(root, func(path string, info fs.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}

		if len(excludes) > 0 && path != root {
			if inner, relErr := filepath.Rel(root, path); relErr == nil && excluded(excludes, filepath.ToSlash(inner)) {
				if info.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		rel, err := filepath.Rel(filepath.Dir(root), path)
		if err != nil {
			return fmt.Errorf("computing archive name for %s: %w", path, err)
		}
		name := filepath.ToSlash(rel)

		switch {
		case info.IsDir():
			hdr, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return fmt.Errorf("archiving %s: %w", path, err)
			}
			hdr.Name = name + "/"
			if err := tw.WriteHeader(hdr); err != nil {
				return fmt.Errorf("archiving %s: %w", path, err)
			}
		case info.Mode().IsRegular():
			hdr, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return fmt.Errorf("archiving %s: %w", path, err)
			}
			hdr.Name = name
			if err := tw.WriteHeader(hdr); err != nil {
				return fmt.Errorf("archiving %s: %w", path, err)
			}
			src, err := os.Open(path)
			if err != nil {
				return fmt.Errorf("archiving %s: %w", path, err)
			}
			_, copyErr := io.Copy(tw, src)
			closeErr := src.Close()
			if copyErr != nil {
				return fmt.Errorf("archiving %s: %w", path, copyErr)
			}
			if closeErr != nil {
				return fmt.Errorf("archiving %s: %w", path, closeErr)
			}
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return fmt.Errorf("archiving symlink %s: %w", path, err)
			}
			hdr, err := tar.FileInfoHeader(info, target)
			if err != nil {
				return fmt.Errorf("archiving symlink %s: %w", path, err)
			}
			hdr.Name = name
			if err := tw.WriteHeader(hdr); err != nil {
				return fmt.Errorf("archiving symlink %s: %w", path, err)
			}
		default:
			*skipped = append(*skipped, path)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("archiving %s: %w", root, err)
	}
	return nil
}

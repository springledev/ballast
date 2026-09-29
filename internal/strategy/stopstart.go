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

// stopAll stops the containers in order. Stopping is all-or-nothing:
// if a stop fails, the containers already stopped are started again
// before the error returns, so the caller only needs to arrange the
// restart once stopAll succeeded.
func (s *StopStart) stopAll(ctx context.Context, containers []string) error {
	for i, c := range containers {
		if _, err := s.opts.Runner.Run(ctx, nil, nil, s.dockerCmd("stop", c)); err != nil {
			retErr := fmt.Errorf("service %s: stopping container %s: %w", s.entry.Service, c, err)
			if rerr := s.startAll(containers[:i]); rerr != nil {
				retErr = errors.Join(retErr, rerr)
			}
			return retErr
		}
	}
	return nil
}

// dockerCmd builds a docker CLI invocation.
func (s *StopStart) dockerCmd(args ...string) runner.Command {
	return s.opts.Docker.Command(nil, args...)
}

// startAll starts every container, attempting each even when others
// fail — leaving a container down is the worst outcome — and joins
// the failures. It runs on a fresh, bounded context so cancellation
// of the backup run cannot prevent the restart.
func (s *StopStart) startAll(containers []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), restartTimeout)
	defer cancel()
	var errs []error
	for _, c := range containers {
		if _, err := s.opts.Runner.Run(ctx, nil, nil, s.dockerCmd("start", c)); err != nil {
			errs = append(errs, fmt.Errorf("starting container %s: %w", c, err))
		}
	}
	return errors.Join(errs...)
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
		if err := tarOne(ctx, tw, root, &skipped); err != nil {
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
func tarOne(ctx context.Context, tw *tar.Writer, root string, skipped *[]string) error {
	err := filepath.Walk(root, func(path string, info fs.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
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

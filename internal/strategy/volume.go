package strategy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/springledev/ballast/internal/catalog"
)

// DefaultVolumeHelperImage is the small image whose tar reads and
// writes named volumes. It is pulled on the service's Docker host the
// first time it is needed, like any other image.
const DefaultVolumeHelperImage = "alpine:3.20"

// volumeArtifactPrefix prefixes every volume artifact's name, so a
// volume called "data" cannot collide with other artifact names.
const volumeArtifactPrefix = "volume-"

// helperCleanupTimeout bounds the best-effort removal of a helper
// container left behind by a failed or cancelled run.
const helperCleanupTimeout = 30 * time.Second

// volumeNamePattern is Docker's own rule for volume names. Enforcing it
// is a safety check, not a nicety: in `-v NAME:/data` a value holding a
// slash would be taken as a host path and bind-mount it instead.
var volumeNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]+$`)

// Volume implements the volume strategy (spec §17.5): every named
// Docker volume in backup.volumes is archived into its own tar artifact
// by a throwaway helper container (`docker run --rm -v VOL:/data:ro
// alpine tar`) on the service's Docker host. Nothing is read from the
// host filesystem, so local and remote hosts behave identically.
//
// Consistency is per service: when backup.stopStart.containers is set,
// those containers are stopped for the duration of the archiving and
// always started again (stop→tar→start, the same guarantee as the
// stop-start strategy); without it the volume is archived live, which
// suits data that is never half-written (media, static config).
type Volume struct {
	entry *catalog.Entry
	opts  Options
}

// validateVolumeEntry checks a volume-strategy entry: at least one
// volume, every name a real Docker volume name, no duplicates, and no
// fields the strategy would otherwise silently ignore.
func validateVolumeEntry(entry *catalog.Entry) error {
	b := entry.Backup
	if len(b.Volumes) == 0 {
		return fmt.Errorf("service %s: volume strategy needs at least one backup.volumes entry", entry.Service)
	}
	if len(b.Files) != 0 || len(b.Dumps) != 0 {
		return fmt.Errorf("service %s: volume strategy archives Docker volumes only; backup.files and backup.dumps belong to other strategies", entry.Service)
	}
	if len(b.HostPaths) != 0 {
		return fmt.Errorf("service %s: volume strategy archives Docker volumes only; folders on the Docker host go in backup.hostPaths of a files or stop-start service (a path is never read as a volume name)", entry.Service)
	}
	if b.StopStart != nil && len(b.StopStart.Containers) == 0 {
		return fmt.Errorf("service %s: backup.stopStart is set but lists no containers (remove it to back the volumes up live)", entry.Service)
	}
	seen := map[string]bool{}
	for _, v := range b.Volumes {
		if err := validVolumeName(v.Name); err != nil {
			return fmt.Errorf("service %s: %w", entry.Service, err)
		}
		if seen[v.Name] {
			return fmt.Errorf("service %s: volume %q is listed twice", entry.Service, v.Name)
		}
		seen[v.Name] = true
	}
	return nil
}

// validVolumeName rejects anything that is not a Docker volume name —
// in particular host paths, which `-v` would bind-mount.
func validVolumeName(name string) error {
	if !volumeNamePattern.MatchString(name) {
		return fmt.Errorf("%q is not a Docker volume name (letters, digits, '_', '.', '-'; at least two characters; no slashes)", name)
	}
	return nil
}

// Backup checks that every volume exists, stops the containers when
// the entry asks for it, and archives each volume into
// <workdir>/volume-<name>.tar. The restart is guaranteed exactly as in
// the stop-start strategy.
func (s *Volume) Backup(ctx context.Context, req Request) (res *Result, retErr error) {
	if err := ensureWorkDir(req.WorkDir); err != nil {
		return nil, fmt.Errorf("service %s: %w", s.entry.Service, err)
	}

	// Existence check first, before any container is stopped: `docker
	// run -v NAME:...` would quietly create a missing volume, and an
	// empty new volume would then be "backed up" successfully.
	for _, v := range s.entry.Backup.Volumes {
		if _, err := s.opts.Runner.Run(ctx, nil, nil, s.opts.Docker.Command(nil, "volume", "inspect", v.Name)); err != nil {
			return nil, fmt.Errorf("service %s: volume %s not found on Docker host %s (check the name with `docker volume ls`): %w",
				s.entry.Service, v.Name, s.opts.Docker.Name(), err)
		}
	}

	if ss := s.entry.Backup.StopStart; ss != nil {
		stopper := &StopStart{entry: s.entry, opts: s.opts}
		if err := stopper.stopAll(ctx, ss.Containers); err != nil {
			return nil, err
		}
		defer func() {
			if err := stopper.startAll(ss.Containers); err != nil {
				retErr = errors.Join(retErr, fmt.Errorf("service %s: starting containers again: %w", s.entry.Service, err))
			}
		}()
	}

	res = &Result{}
	for _, v := range s.entry.Backup.Volumes {
		a, err := s.archive(ctx, req.WorkDir, v.Name)
		if err != nil {
			return nil, fmt.Errorf("service %s: archiving volume %s: %w", s.entry.Service, v.Name, err)
		}
		res.Artifacts = append(res.Artifacts, a)
	}
	return res, nil
}

// archive streams one volume's tar from the helper container into its
// artifact file (created 0600: it holds service data). The volume is
// mounted read-only and the helper has no network.
func (s *Volume) archive(ctx context.Context, workDir, volume string) (Artifact, error) {
	name := volumeArtifactPrefix + volume
	path := filepath.Join(workDir, name+".tar")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return Artifact{}, fmt.Errorf("creating archive file: %w", err)
	}
	helper := helperName()
	cmd := s.opts.Docker.Command(nil, "run", "--rm", "--name", helper, "--network", "none",
		"-v", volume+":/data:ro", s.opts.volumeHelperImage(),
		"tar", "-cf", "-", "-C", "/data", ".")
	_, runErr := s.opts.Runner.Run(ctx, nil, f, cmd)
	closeErr := f.Close()
	if runErr != nil {
		s.opts.removeHelper(helper)
		return Artifact{}, runErr
	}
	if closeErr != nil {
		return Artifact{}, fmt.Errorf("writing archive file: %w", closeErr)
	}
	return Artifact{Name: name, Path: path, Format: "tar"}, nil
}

// RestoreVolume extracts a tar archive produced by the volume strategy
// into the named volume on opts.Docker's host, creating the volume when
// it does not exist. Files already in the volume that the archive also
// holds are overwritten; others are left alone, so restoring into an
// empty (or freshly created) volume gives an exact copy. Stop the
// containers using the volume first.
func RestoreVolume(ctx context.Context, opts Options, volume string, archive io.Reader) error {
	if opts.Runner == nil {
		return fmt.Errorf("restoring volume %s: no command runner configured", volume)
	}
	if err := validVolumeName(volume); err != nil {
		return fmt.Errorf("restoring volume: %w", err)
	}
	helper := helperName()
	cmd := opts.Docker.Command(nil, "run", "--rm", "-i", "--name", helper, "--network", "none",
		"-v", volume+":/data", opts.volumeHelperImage(),
		"tar", "-xpf", "-", "-C", "/data")
	if _, err := opts.Runner.Run(ctx, archive, nil, cmd); err != nil {
		opts.removeHelper(helper)
		return fmt.Errorf("restoring volume %s on Docker host %s: %w", volume, opts.Docker.Name(), err)
	}
	return nil
}

// volumeHelperImage returns the configured helper image.
func (o Options) volumeHelperImage() string {
	if o.VolumeHelperImage != "" {
		return o.VolumeHelperImage
	}
	return DefaultVolumeHelperImage
}

// removeHelper force-removes a helper container after a failure. When
// the run was cancelled, killing the docker CLI does not stop the
// container it started, so it would otherwise keep running (and keep
// the volume mounted). --volumes matters because removing the container
// here bypasses --rm's own volume cleanup, which would leak an anonymous
// volume if a configured helper image declares one. Best effort, on a
// fresh bounded context, so the caller still reports the original
// failure: a failed removal is logged, and "no such container" (when
// --rm already removed it) is not a failure, as docker rm -f exits 0.
func (o Options) removeHelper(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), helperCleanupTimeout)
	defer cancel()
	if _, err := o.Runner.Run(ctx, nil, nil, o.Docker.Command(nil, "rm", "-f", "--volumes", name)); err != nil {
		log.Printf("ballast: helper container %s on Docker host %s could not be removed (the backup or restore result is unaffected); to remove it yourself, run: docker rm -f --volumes %s: %v", name, o.Docker.Name(), name, err)
	}
}

// helperName returns a unique helper container name, so a failed run
// can remove exactly the container it started.
func helperName() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return "ballast-volume-" + hex.EncodeToString(b[:])
}

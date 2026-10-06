package strategy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/springledev/ballast/internal/catalog"
)

// hostPathsArtifact is the name of the single tar artifact a
// hostPaths backup produces (the same name the stop-start strategy
// gives its local archive).
const hostPathsArtifact = "data"

// hostMountRoot is where the helper container mounts each folder,
// read-only, under its own base name.
const hostMountRoot = "/src"

// hostSource is one resolved folder on the service's Docker host.
type hostSource struct {
	// Source is the absolute path on the Docker host.
	Source string
	// Name is the folder's base name, which is also its name inside
	// the archive.
	Name string
}

// ContainerMount is one bind mount of a container, as `docker inspect`
// reports it. It feeds the "pick a folder" auto-fill in the setup form.
type ContainerMount struct {
	// Source is the folder on the Docker host.
	Source string `json:"source"`
	// Destination is where the container sees it (e.g. /data).
	Destination string `json:"destination"`
	// ReadOnly is true when the container mounts it read-only.
	ReadOnly bool `json:"readOnly"`
}

// inspectedMount mirrors the fields of `docker inspect`'s Mounts list
// that Ballast uses.
type inspectedMount struct {
	Type        string `json:"Type"`
	Name        string `json:"Name"`
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
	RW          bool   `json:"RW"`
}

// inspectMounts returns every mount of container on opts.Docker's host.
func (o Options) inspectMounts(ctx context.Context, container string) ([]inspectedMount, error) {
	if container == "" {
		return nil, fmt.Errorf("no container name given")
	}
	var buf bytes.Buffer
	cmd := o.Docker.Command(nil, "inspect", "--format", "{{json .Mounts}}", container)
	if _, err := o.Runner.Run(ctx, nil, &buf, cmd); err != nil {
		return nil, fmt.Errorf("looking up container %s on Docker host %s (is the name right?): %w", container, o.Docker.Name(), err)
	}
	var mounts []inspectedMount
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &mounts); err != nil {
		return nil, fmt.Errorf("reading the mounts of container %s: %w", container, err)
	}
	return mounts, nil
}

// ContainerBindMounts lists the folders of the Docker host that
// container has bind-mounted, so the setup form can offer them instead
// of asking the user to type a path. Named volumes are left out: they
// belong to the volume strategy.
func ContainerBindMounts(ctx context.Context, opts Options, container string) ([]ContainerMount, error) {
	if opts.Runner == nil {
		return nil, fmt.Errorf("container %s: no command runner configured", container)
	}
	mounts, err := opts.inspectMounts(ctx, container)
	if err != nil {
		return nil, err
	}
	var out []ContainerMount
	for _, m := range mounts {
		if m.Type == "bind" {
			out = append(out, ContainerMount{Source: m.Source, Destination: m.Destination, ReadOnly: !m.RW})
		}
	}
	return out, nil
}

// validHostPath checks one folder path: absolute, not the whole disk,
// and free of characters that could change how docker reads --mount.
// A host path is never a volume name; this is the only place it is
// interpreted.
func validHostPath(p string) error {
	if p == "" {
		return fmt.Errorf("a hostPaths entry has an empty path")
	}
	if !path.IsAbs(p) {
		return fmt.Errorf("host path %q must be absolute (e.g. /srv/data), not relative", p)
	}
	if strings.ContainsAny(p, ",\"\n\r\x00") {
		return fmt.Errorf("host path %q must not contain commas, quotes or line breaks", p)
	}
	if path.Clean(p) == "/" {
		return fmt.Errorf("host path %q is the whole disk; choose the folder that holds the service's data", p)
	}
	return nil
}

// validateHostPathSpecs checks the static shape of hostPaths entries
// (no Docker needed): path xor mount+container, absolute paths.
func validateHostPathSpecs(service string, specs []catalog.HostPathSpec) error {
	for i, h := range specs {
		switch {
		case h.Path != "" && (h.Mount != "" || h.Container != ""):
			return fmt.Errorf("service %s: hostPaths entry %d sets both a path and a container mount; use one or the other", service, i+1)
		case h.Path != "":
			if err := validHostPath(h.Path); err != nil {
				return fmt.Errorf("service %s: %w", service, err)
			}
		case h.Mount != "" && h.Container != "":
			if !path.IsAbs(h.Mount) {
				return fmt.Errorf("service %s: hostPaths mount %q must be an absolute path inside the container (e.g. /data)", service, h.Mount)
			}
		default:
			return fmt.Errorf("service %s: hostPaths entry %d needs either a path, or a container together with its mount point", service, i+1)
		}
	}
	return nil
}

// resolveHostPaths turns the entry's hostPaths into absolute folders on
// the Docker host: container mount points are looked up with `docker
// inspect` now, so a folder moved since the last run is followed.
func (o Options) resolveHostPaths(ctx context.Context, service string, specs []catalog.HostPathSpec) ([]hostSource, error) {
	var out []hostSource
	seen := map[string]string{}
	for _, h := range specs {
		src := h.Path
		if src == "" {
			resolved, err := o.resolveMount(ctx, h.Container, h.Mount)
			if err != nil {
				return nil, fmt.Errorf("service %s: %w", service, err)
			}
			src = resolved
		}
		if err := validHostPath(src); err != nil {
			return nil, fmt.Errorf("service %s: %w", service, err)
		}
		src = path.Clean(src)
		name := path.Base(src)
		if prev, dup := seen[name]; dup {
			return nil, fmt.Errorf("service %s: host paths %s and %s share the folder name %q, which would collide in the archive; back them up as separate services", service, prev, src, name)
		}
		seen[name] = src
		out = append(out, hostSource{Source: src, Name: name})
	}
	return out, nil
}

// resolveMount finds the host folder behind container's mount point.
func (o Options) resolveMount(ctx context.Context, container, mount string) (string, error) {
	mounts, err := o.inspectMounts(ctx, container)
	if err != nil {
		return "", err
	}
	want := path.Clean(mount)
	var have []string
	for _, m := range mounts {
		have = append(have, m.Destination)
		if path.Clean(m.Destination) != want {
			continue
		}
		if m.Type != "bind" {
			return "", fmt.Errorf("%s in container %s is a Docker volume, not a folder on the host; back it up with the volume strategy instead", mount, container)
		}
		return m.Source, nil
	}
	sort.Strings(have)
	return "", fmt.Errorf("container %s has nothing mounted at %s on Docker host %s (it mounts: %s)", container, mount, o.Docker.Name(), strings.Join(have, ", "))
}

// checkHostPath proves one folder exists on the Docker host. It uses
// --mount (not -v) on purpose: a missing source is an error, where -v
// would quietly create an empty root-owned folder on the host.
func (o Options) checkHostPath(ctx context.Context, service string, src hostSource) error {
	helper := helperName()
	target := hostMountRoot + "/" + src.Name
	cmd := o.Docker.Command(nil, "run", "--rm", "--name", helper, "--network", "none",
		"--mount", "type=bind,source="+src.Source+",target="+target+",readonly",
		o.volumeHelperImage(), "test", "-e", target)
	if _, err := o.Runner.Run(ctx, nil, nil, cmd); err != nil {
		o.removeHelper(helper)
		return fmt.Errorf("service %s: folder %s was not found on Docker host %s (check the path on that machine; nothing was stopped): %w", service, src.Source, o.Docker.Name(), err)
	}
	return nil
}

// prepareHostPaths resolves and checks every folder on the service's
// own Docker host. It runs before any container is stopped, so a wrong
// path can never leave a service down for nothing.
func (o Options) prepareHostPaths(ctx context.Context, service string, specs []catalog.HostPathSpec) ([]hostSource, error) {
	srcs, err := o.resolveHostPaths(ctx, service, specs)
	if err != nil {
		return nil, err
	}
	for _, s := range srcs {
		if err := o.checkHostPath(ctx, service, s); err != nil {
			return nil, err
		}
	}
	return srcs, nil
}

// archiveHostPaths streams every folder, mounted read-only into one
// throwaway helper container with no network, into <workDir>/data.tar
// (created 0600: it holds service data). Each folder is stored under
// its base name.
func (o Options) archiveHostPaths(ctx context.Context, workDir string, srcs []hostSource) (Artifact, error) {
	dest := filepath.Join(workDir, hostPathsArtifact+".tar")
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return Artifact{}, fmt.Errorf("creating archive file: %w", err)
	}
	helper := helperName()
	args := []string{"run", "--rm", "--name", helper, "--network", "none"}
	for _, s := range srcs {
		args = append(args, "--mount", "type=bind,source="+s.Source+",target="+hostMountRoot+"/"+s.Name+",readonly")
	}
	args = append(args, o.volumeHelperImage(), "tar", "-cf", "-", "-C", hostMountRoot, ".")
	_, runErr := o.Runner.Run(ctx, nil, f, o.Docker.Command(nil, args...))
	closeErr := f.Close()
	if runErr != nil {
		o.removeHelper(helper)
		return Artifact{}, runErr
	}
	if closeErr != nil {
		return Artifact{}, fmt.Errorf("writing archive file: %w", closeErr)
	}
	return Artifact{Name: hostPathsArtifact, Path: dest, Format: "tar"}, nil
}

// RestoreHostPath extracts a tar archive made from hostPaths into dest
// on opts.Docker's host, through the helper container. By default dest
// must not exist yet, so a restore can never overwrite live data; pass
// overwrite only after the user explicitly confirmed it (files that the
// archive also holds are then replaced, others are left alone). The
// folders inside the archive land under dest, one per original folder.
func RestoreHostPath(ctx context.Context, opts Options, dest string, archive io.Reader, overwrite bool) error {
	if opts.Runner == nil {
		return fmt.Errorf("restoring to %s: no command runner configured", dest)
	}
	if err := validHostPath(dest); err != nil {
		return fmt.Errorf("restoring to host folder: %w", err)
	}
	dest = path.Clean(dest)
	probe := helperName()
	exists := true
	probeCmd := opts.Docker.Command(nil, "run", "--rm", "--name", probe, "--network", "none",
		"--mount", "type=bind,source="+dest+",target=/restore,readonly", opts.volumeHelperImage(), "test", "-d", "/restore")
	if _, err := opts.Runner.Run(ctx, nil, nil, probeCmd); err != nil {
		opts.removeHelper(probe)
		exists = false
	}
	if exists && !overwrite {
		return fmt.Errorf("restore folder %s already exists on Docker host %s; choose a new folder, or confirm that overwriting is intended", dest, opts.Docker.Name())
	}
	helper := helperName()
	cmd := opts.Docker.Command(nil, "run", "--rm", "-i", "--name", helper, "--network", "none",
		"-v", dest+":/restore", opts.volumeHelperImage(),
		"tar", "-xpf", "-", "-C", "/restore")
	if _, err := opts.Runner.Run(ctx, archive, nil, cmd); err != nil {
		opts.removeHelper(helper)
		return fmt.Errorf("restoring to %s on Docker host %s: %w", dest, opts.Docker.Name(), err)
	}
	return nil
}

package verify

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/springledev/ballast/internal/runner"
	"github.com/springledev/ballast/internal/strategy"
)

// defaultReadyTimeout is how long the throwaway container gets to
// accept client connections before verification fails. Container
// image pulls and first-boot initialization normally finish well
// inside this.
const defaultReadyTimeout = 60 * time.Second

// ContainerRestore implements the throwaway-container restore of
// spec §7.2: a database dump is fed into a fresh container of the
// entry's verify.image, stderr is filtered whitelist-style (only the
// catalog's filterStderr patterns are benign), and the container is
// destroyed afterwards — even when the check fails or is cancelled.
//
// The spec §7.2 normative MUSTs, each enforced by a test:
//
//   - The container runs with an explicit --entrypoint override,
//     because e.g. the postgres entrypoint only starts a server when
//     the command is `postgres`.
//   - The container runs with --network none: it holds an
//     empty-password / trust-local server for the check window, so
//     it must not be reachable from other containers on the docker
//     network. Every interaction is via docker exec, which needs no
//     network.
//   - A live source container whose image differs from verify.image
//     produces a Warning on the result (drift = restore verdict means
//     less, e.g. an Immich restore into a postgres build without
//     pgvector/vchord).
//   - Cleanup uses a detached context so cancellation of the job can
//     never leave a throwaway container behind.
//   - The stderr filter is whitelist-style: patterns match by
//     substring, and anything not whitelisted fails the check.
type ContainerRestore struct {
	// service is the catalog entry's service name; it backs the
	// per-service error details when Request.Service is empty.
	service string
	image   string
	filter  []string
	opts    Options

	// ReadyTimeout bounds the wait-for-startup loop. Zero uses a
	// 60-second default; tests shorten it to fail fast.
	ReadyTimeout time.Duration
}

// Method implements Checker.
func (*ContainerRestore) Method() string { return "container-restore" }

// Check restores every SQL artifact of the run into one new throwaway
// container and reports the aggregate verdict.
func (c *ContainerRestore) Check(ctx context.Context, req Request) (res Result) {
	start := time.Now()
	res = Result{Method: c.Method()}

	// Per-service attribution (gate: every failure names its
	// service): Request.Service is the caller's name for the service;
	// fall back to the catalog entry's so a caller forgetting it
	// never degrades details to "service : ...".
	service := req.Service
	if service == "" {
		service = c.service
	}

	dumps := sqlArtifacts(req.Artifacts)
	if len(dumps) == 0 {
		res.Detail = "service " + service + ": the backup produced no SQL dump artifact to restore"
		res.Duration = time.Since(start)
		return res
	}

	dialect := dialectFor(c.image)
	name := "ballast-verify-" + randomSuffix()

	// spec §7.2 MUST: warn when the live container's image differs
	// from the image we restore into. Comparison is best-effort: an
	// inspect failure or an empty SourceContainer only adds a
	// warning, never fails the check itself.
	if warning := c.imageDriftWarning(ctx, req.SourceContainer, req.Env); warning != "" {
		res.Warning = warning
	}

	// Guaranteed cleanup (spec §7.2): every exit path destroys the
	// container. It runs on a detached context so job cancellation
	// cannot skip it, and the deferred mutation of the named return
	// attaches any cleanup failure to the result's warnings.
	defer func() {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := c.removeContainer(cleanCtx, name, req.Env); err != nil {
			res.Warning = joinWarning(res.Warning,
				"throwaway container "+name+" could not be removed: "+err.Error())
		}
	}()

	if err := c.startContainer(ctx, name, dialect, req.Env); err != nil {
		res.Detail = "service " + service + ": could not start throwaway container " + name + ": " + err.Error()
		res.Duration = time.Since(start)
		return res
	}

	if err := c.waitReady(ctx, name, dialect, req.Env); err != nil {
		res.Detail = "service " + service + ": throwaway container " + name + " did not come up: " + err.Error()
		res.Duration = time.Since(start)
		return res
	}

	var output []string
	for _, a := range dumps {
		benign, leftover, warn, err := c.restoreOne(ctx, name, dialect, a, req.Env)
		output = append(output, benign...)
		if len(leftover) > 0 {
			output = append(output, leftover...)
			res.Detail = fmt.Sprintf("service %s: artifact %q (%s) failed to restore cleanly; stderr lines not on the catalog's benign whitelist: %s",
				service, a.Name, a.Path, strings.Join(leftover, "; "))
			res.Output = strings.Join(output, "\n")
			res.Duration = time.Since(start)
			return res
		}
		if err != nil {
			res.Detail = fmt.Sprintf("service %s: artifact %q (%s): %v", service, a.Name, a.Path, err)
			res.Output = strings.Join(output, "\n")
			res.Duration = time.Since(start)
			return res
		}
		res.Warning = joinWarning(res.Warning, warn)
	}

	res.Passed = true
	res.Detail = fmt.Sprintf("service %s: restored %d dump artifact(s) into throwaway container %s and destroyed it", service, len(dumps), name)
	res.Output = strings.Join(output, "\n")
	res.Duration = time.Since(start)
	return res
}

// sqlArtifacts selects the dump artifacts: format "sql" or an
// unspecified format (the catalog's dump.format is free text, and an
// empty one defaults to whatever the dump command emits, which for
// database services is SQL).
func sqlArtifacts(artifacts []strategy.Artifact) []strategy.Artifact {
	var dumps []strategy.Artifact
	for _, a := range artifacts {
		if a.Format == "" || strings.EqualFold(a.Format, "sql") {
			dumps = append(dumps, a)
		}
	}
	return dumps
}

// randomSuffix returns six random hex characters — enough to keep
// throwaway names unique without a global registry.
func randomSuffix() string {
	buf := make([]byte, 3)
	if _, err := rand.Read(buf); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(buf)
}

// restoreDialect describes how one database family is restored.
type restoreDialect struct {
	server    string   // entrypoint override (normative MUST)
	extraRun  []string // extra `docker run` args
	readyArgs []string // readiness probe inside the container
	client    []string // client command reading the dump from stdin
}

// dialectFor picks the restore dialect from the image name. The
// catalog declares the image; dispatching on it keeps "new service =
// YAML entry, not Go code" true for any postgres/mysql-family service
// (generic dispatch on the engine, not per-service logic).
func dialectFor(image string) restoreDialect {
	lower := strings.ToLower(image)
	if strings.Contains(lower, "mysql") || strings.Contains(lower, "mariadb") {
		return restoreDialect{
			server:    "mysqld",
			extraRun:  []string{"-e", "MYSQL_ALLOW_EMPTY_PASSWORD=yes"}, // no secret travels in argv
			readyArgs: []string{"mysqladmin", "-s", "ping"},
			client:    []string{"mysql", "-u", "root"},
		}
	}
	return restoreDialect{
		server:    "postgres",
		readyArgs: []string{"pg_isready", "-q", "-U", "postgres"},
		// ON_ERROR_STOP=0 is explicit: psql must continue past errors
		// so stderr carries ALL of them for the whitelist filter;
		// stopping at the first one would hide the rest.
		client: []string{"psql", "-v", "ON_ERROR_STOP=0", "-q", "-U", "postgres"},
	}
}

// imageDriftWarning compares the live source container's image with
// the verify image, when a source container is known. Any difference
// is a warning, and inspect failure only downgrades to a warning —
// the restore itself still runs.
func (c *ContainerRestore) imageDriftWarning(ctx context.Context, source string, env []string) string {
	if source == "" {
		return ""
	}
	out, err := c.opts.Runner.Run(ctx, nil, nil, runner.Command{
		Name: c.opts.docker(),
		Args: []string{"inspect", "--format", "{{.Image}}", source},
		Env:  env,
	})
	if err != nil {
		return "could not compare " + source + "'s image with the test-restore image " + c.image + ": " + err.Error()
	}
	running := strings.TrimSpace(string(out.Stdout))
	if running != c.image {
		return "live container " + source + " runs image " + running + " but the test-restore uses " + c.image + " — the restore may not prove compatibility"
	}
	return ""
}

// startContainer creates the throwaway container with an explicit
// entrypoint override and no network.
func (c *ContainerRestore) startContainer(ctx context.Context, name string, d restoreDialect, env []string) error {
	// --network none: the throwaway runs an empty-password /
	// trust-local server, so it must not be reachable from other
	// containers during the check window. Every interaction happens
	// through docker exec, which works without a network.
	args := []string{"run", "--detach", "--network", "none", "--entrypoint", d.server, "--name", name}
	args = append(args, d.extraRun...)
	args = append(args, c.image)
	_, err := c.opts.Runner.Run(ctx, nil, nil, runner.Command{Name: c.opts.docker(), Args: args, Env: env})
	if err != nil {
		return fmt.Errorf("starting %s: %w", c.image, err)
	}
	return nil
}

// waitReady polls the server's readiness probe until it answers or
// the timeout expires.
func (c *ContainerRestore) waitReady(ctx context.Context, name string, d restoreDialect, env []string) error {
	timeout := c.ReadyTimeout
	if timeout <= 0 {
		timeout = defaultReadyTimeout
	}
	deadline := time.Now().Add(timeout)
	probe := runner.Command{Name: c.opts.docker(), Args: append([]string{"exec", name}, d.readyArgs...), Env: env}
	for {
		if _, err := c.opts.Runner.Run(ctx, nil, nil, probe); err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("not ready after %s (probe: %s)", timeout, strings.Join(d.readyArgs, " "))
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// restoreOne feeds one dump artifact into the container and applies
// the whitelist filter to stderr. Returned values:
//   - benign: stderr lines matched by the whitelist (for Output)
//   - leftover: unfiltered stderr lines (they fail the check)
//   - warn: a passing-with-warning note (non-zero exit with fully
//     benign stderr)
//   - err: the run failed and stderr had no filterable lines at all
func (c *ContainerRestore) restoreOne(ctx context.Context, name string, d restoreDialect, a strategy.Artifact, env []string) (benign, leftover []string, warn string, err error) {
	f, err := os.Open(a.Path)
	if err != nil {
		return nil, nil, "", fmt.Errorf("opening artifact: %w", err)
	}

	args := append([]string{"exec", "-i", name}, d.client...)
	out, runErr := c.opts.Runner.Run(ctx, f, nil, runner.Command{Name: c.opts.docker(), Args: args, Env: env})
	_ = f.Close()

	lines := stderrLines(out.Stderr)
	benign, leftover = splitFiltered(lines, c.filter)
	if len(leftover) > 0 {
		return benign, leftover, "", runErr
	}
	if runErr != nil && len(lines) == 0 {
		return benign, nil, "", fmt.Errorf("restore command: %w", runErr)
	}
	if runErr != nil {
		return benign, nil, "restore command exited with an error but every stderr line matched the catalog's benign whitelist: " + runErr.Error(), nil
	}
	return benign, nil, "", nil
}

// removeContainer destroys the throwaway container. It runs on a
// caller-provided (detached) context so cleanup survives job
// cancellation.
func (c *ContainerRestore) removeContainer(ctx context.Context, name string, env []string) error {
	_, err := c.opts.Runner.Run(ctx, nil, nil, runner.Command{
		Name: c.opts.docker(),
		Args: []string{"rm", "--force", name},
		Env:  env,
	})
	if err != nil {
		return fmt.Errorf("docker rm --force %s: %w", name, err)
	}
	return nil
}

// stderrLines splits captured stderr into lines, dropping empty ones.
func stderrLines(stderr []byte) []string {
	var lines []string
	for _, l := range strings.Split(string(stderr), "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, strings.TrimSpace(l))
		}
	}
	return lines
}

// splitFiltered partitions lines into whitelist-matched (benign) and
// unfiltered. Matching is substring-based: the catalog encodes the
// exact observed string (e.g. "already exists").
func splitFiltered(lines, whitelist []string) (benign, leftover []string) {
	for _, l := range lines {
		if matchesAny(l, whitelist) {
			benign = append(benign, l)
		} else {
			leftover = append(leftover, l)
		}
	}
	return benign, leftover
}

// matchesAny reports whether line contains any whitelist pattern.
func matchesAny(line string, whitelist []string) bool {
	for _, w := range whitelist {
		if w != "" && strings.Contains(line, w) {
			return true
		}
	}
	return false
}

// joinWarning appends a warning to an existing one ("" = none).
func joinWarning(existing, next string) string {
	if next == "" {
		return existing
	}
	if existing == "" {
		return next
	}
	return existing + " Also: " + next
}

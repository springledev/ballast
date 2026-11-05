package verify

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/springledev/ballast/internal/strategy"
)

// defaultReadyTimeout is how long the throwaway container gets to
// accept client connections before verification fails. First-boot
// initialization (initdb / mysql install-db, now that the image's own
// entrypoint script performs it — see the ContainerRestore "spec
// deviation" note) can take upwards of 20-30s for mysql's two-phase
// startup under load; this leaves comfortable headroom.
const defaultReadyTimeout = 90 * time.Second

// cleanupTimeout bounds removing the throwaway container and its
// volume, including the repeat removals after an interrupted start.
const cleanupTimeout = 30 * time.Second

// interruptedStartRemovals is how long cleanup waits before each repeat
// removal after `docker run` was killed mid-flight. A real daemon was
// seen finishing the create about 200 ms after the first removal, under
// load; the later passes leave room for a much busier host.
var interruptedStartRemovals = []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second}

// ContainerRestore implements the throwaway-container restore of
// spec §7.2: a database dump is fed into a fresh container of the
// entry's verify.image, stderr is filtered whitelist-style (only the
// catalog's filterStderr patterns are benign), and the container is
// destroyed afterwards — even when the check fails or is cancelled.
//
// The spec §7.2 normative MUSTs, each enforced by a test:
//
//   - The container is started with the image's own entrypoint AND
//     its own default command. NOTE (spec deviation, flagged per
//     AGENTS.md): spec §7.2 says to "override the entrypoint
//     explicitly" so the server actually starts; overriding
//     --entrypoint skips the entrypoint script's first-run
//     initialization (initdb / mysql install-db) entirely (confirmed
//     against real postgres:16 and mysql:8 images: the server then
//     refuses to start at all). Passing a bare server binary as the
//     command is not safe either: it discards the image's own server
//     arguments, and e.g. Immich's ghcr.io/immich-app/postgres image
//     loads its vector-search extensions through
//     `postgres -c config_file=/etc/postgresql/postgresql.conf` —
//     restoring an Immich dump into a bare `postgres` fails with
//     "vchord must be loaded via shared_preload_libraries" (confirmed
//     against the real image). Every supported database image's
//     default command already starts its server through the
//     init-and-start path this MUST is asking for, so the restore
//     runs the image exactly as the service's own stack does.
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
//     never leave a throwaway container behind. It also removes the
//     anonymous volume Docker creates for the image's declared data
//     directory, which would otherwise stay on disk after every drill.
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
func (*ContainerRestore) Method() string { return MethodDBRestoreDrill }

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
	name := drillNamePrefix + randomSuffix()

	// spec §7.2 MUST: warn when the live container's image differs
	// from the image we restore into. Comparison is best-effort: an
	// inspect failure or an empty SourceContainer only adds a
	// warning, never fails the check itself.
	image, warning := c.restoreImage(ctx, req.SourceContainer, req.Env)
	if warning != "" {
		res.Warning = warning
	}

	// Guaranteed cleanup (spec §7.2): every exit path destroys the
	// container and its anonymous volume. It runs on a detached
	// context so job cancellation cannot skip it, and the deferred
	// mutation of the named return attaches any cleanup failure to the
	// result's warnings, leaving the verdict as it was.
	startInterrupted := false
	defer func() {
		cleanCtx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		if err := c.cleanup(cleanCtx, name, req.Env, startInterrupted); err != nil {
			res.Warning = joinWarning(res.Warning,
				"throwaway container "+name+" and its temporary data could not be removed, so they may still be using disk space. To remove them yourself, run: docker rm --force --volumes "+name+" ("+err.Error()+")")
			return
		}
		if res.Passed {
			res.Detail += " and destroyed it"
		}
	}()

	if err := c.startContainer(ctx, name, image, dialect, req.Env); err != nil {
		startInterrupted = ctx.Err() != nil
		res.Detail = "service " + service + ": the restore drill could not start the test container " + name + " (the backup itself was made): " + err.Error()
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
			res.Detail = fmt.Sprintf("service %s: artifact %q (%s) failed to restore cleanly in the restore drill (the backup itself was made); stderr lines not on the catalog's benign whitelist: %s",
				service, a.Name, a.Path, strings.Join(leftover, "; "))
			res.Output = strings.Join(output, "\n")
			res.Duration = time.Since(start)
			return res
		}
		if err != nil {
			res.Detail = fmt.Sprintf("service %s: the restore drill failed while loading artifact %q (%s) into the test database (the backup itself was made): %v", service, a.Name, a.Path, err)
			res.Output = strings.Join(output, "\n")
			res.Duration = time.Since(start)
			return res
		}
		res.Warning = joinWarning(res.Warning, warn)
	}

	res.Passed = true
	res.Detail = fmt.Sprintf("service %s: restored %d dump artifact(s) into throwaway container %s", service, len(dumps), name)
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
			extraRun: []string{"-e", "MYSQL_ALLOW_EMPTY_PASSWORD=yes"}, // no secret travels in argv
			// MariaDB 11 images ship only mariadb-admin / mariadb;
			// older and MySQL images ship only mysqladmin / mysql.
			// Probe for the mariadb name first, fall back to mysql.
			// The probe goes over TCP on purpose: the image's
			// temporary first-boot server listens on its socket
			// only, so a socket probe can pass just before that
			// server shuts down and the restore then dies with
			// "Lost connection".
			readyArgs: []string{"sh", "-c", `if command -v mariadb-admin >/dev/null 2>&1; then exec mariadb-admin -s --protocol=tcp -h127.0.0.1 -u root ping; else exec mysqladmin -s --protocol=tcp -h127.0.0.1 -u root ping; fi`},
			client:    []string{"sh", "-c", `if command -v mariadb >/dev/null 2>&1; then exec mariadb -u root; else exec mysql -u root; fi`},
		}
	}
	return restoreDialect{
		// POSTGRES_HOST_AUTH_METHOD=trust: the throwaway container
		// exists only for the duration of the check (--network none
		// keeps it unreachable from anywhere else) and no secret
		// travels in argv, matching the mysql dialect's
		// MYSQL_ALLOW_EMPTY_PASSWORD=yes below.
		extraRun: []string{"-e", "POSTGRES_HOST_AUTH_METHOD=trust"},
		// TCP, not the socket: the temporary first-boot server
		// has listen_addresses='' and answers on its socket only.
		readyArgs: []string{"pg_isready", "-q", "-h", "127.0.0.1", "-U", "postgres"},
		// ON_ERROR_STOP=0 is explicit: psql must continue past errors
		// so stderr carries ALL of them for the whitelist filter;
		// stopping at the first one would hide the rest.
		client: []string{"psql", "-v", "ON_ERROR_STOP=0", "-q", "-U", "postgres"},
	}
}

// restoreImage picks the image the throwaway container runs. When the
// live source container is known, that is its exact image (the ID it
// really runs), so the test restore is the real one and no drift is
// possible. Only when the live image cannot be read does it fall back
// to the configured image, with a warning saying the test may be less
// exact.
func (c *ContainerRestore) restoreImage(ctx context.Context, source string, env []string) (string, string) {
	if source == "" {
		return c.image, ""
	}
	out, err := c.opts.Runner.Run(ctx, nil, nil, c.opts.Docker.Command(env, "inspect", "--format", "{{.Image}}", source))
	running := strings.TrimSpace(string(out.Stdout))
	if err != nil || running == "" {
		reason := "it returned nothing"
		if err != nil {
			reason = err.Error()
		}
		return c.image, "could not read the image " + source + " is running (" + reason + "), so the test restore uses " + c.image + " instead, which may not prove compatibility"
	}
	return running, ""
}

// startContainer creates the throwaway container with no network,
// keeping the image's own entrypoint and default command, so its
// first-run initialization (initdb / mysql install-db) runs and the
// server starts with the image's own arguments — see the
// package-level "spec deviation" note on ContainerRestore for why
// neither is overridden.
func (c *ContainerRestore) startContainer(ctx context.Context, name, image string, d restoreDialect, env []string) error {
	// --network none: the throwaway runs an empty-password /
	// trust-local server, so it must not be reachable from other
	// containers during the check window. Every interaction happens
	// through docker exec, which works without a network.
	args := []string{"run", "--detach", "--network", "none", "--name", name}
	args = append(args, drillLabelArgs(c.opts.DrillOwner)...)
	args = append(args, d.extraRun...)
	args = append(args, image)
	_, err := c.opts.Runner.Run(ctx, nil, nil, c.opts.Docker.Command(env, args...))
	if err != nil {
		return fmt.Errorf("starting %s: %w", image, err)
	}
	return nil
}

// readySettleDelay is how long waitReady waits before re-confirming a
// first successful readiness probe. Both the postgres and mysql
// official images run a brief, unadvertised "temporary" server to
// execute first-boot initialization before stopping it and starting
// the real one (confirmed against real postgres:16/mysql:8 images: a
// readiness probe can succeed against that temporary server only for
// it to disappear moments later, failing the restore that immediately
// follows with a connection error). Re-confirming after a short delay
// catches that transition instead of racing it.
const readySettleDelay = 300 * time.Millisecond

// waitReady polls the server's readiness probe until it answers twice
// in a row (settled) or the timeout expires — see readySettleDelay.
func (c *ContainerRestore) waitReady(ctx context.Context, name string, d restoreDialect, env []string) error {
	timeout := c.ReadyTimeout
	if timeout <= 0 {
		timeout = defaultReadyTimeout
	}
	deadline := time.Now().Add(timeout)
	probe := c.opts.Docker.Command(env, append([]string{"exec", name}, d.readyArgs...)...)
	for {
		if _, err := c.opts.Runner.Run(ctx, nil, nil, probe); err == nil {
			if err := sleepOrDone(ctx, readySettleDelay); err != nil {
				return err
			}
			if _, err := c.opts.Runner.Run(ctx, nil, nil, probe); err == nil {
				return nil
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("not ready after %s (probe: %s)", timeout, strings.Join(d.readyArgs, " "))
		}
		if err := sleepOrDone(ctx, 200*time.Millisecond); err != nil {
			return err
		}
	}
}

// sleepOrDone waits for d or ctx's cancellation, whichever comes
// first, returning ctx.Err() if it was cancelled first — unlike
// time.Sleep, it never keeps a caller waiting past a job being
// cancelled or timing out.
func sleepOrDone(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
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
	out, runErr := c.opts.Runner.Run(ctx, f, nil, c.opts.Docker.Command(env, args...))
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

// cleanup removes the throwaway container and its anonymous volume.
//
// When the job was cancelled while `docker run` was in flight, killing
// the docker CLI does not stop the daemon: it can finish creating the
// container a moment after a first removal found nothing and reported
// success (confirmed against a real daemon). startInterrupted removes
// the name again over interruptedStartRemovals to catch that container.
func (c *ContainerRestore) cleanup(ctx context.Context, name string, env []string, startInterrupted bool) error {
	err := c.removeContainer(ctx, name, env)
	if !startInterrupted {
		return err
	}
	for _, wait := range interruptedStartRemovals {
		if sleepErr := sleepOrDone(ctx, wait); sleepErr != nil {
			return fmt.Errorf("removing container %s after an interrupted start: %w", name, sleepErr)
		}
		err = c.removeContainer(ctx, name, env)
	}
	return err
}

// removeContainer destroys the throwaway container and its anonymous
// volumes. --volumes never touches named volumes, and the drill mounts
// none. It runs on a caller-provided (detached) context so cleanup
// survives job cancellation. A container that does not exist is not an
// error: docker rm --force exits 0.
func (c *ContainerRestore) removeContainer(ctx context.Context, name string, env []string) error {
	_, err := c.opts.Runner.Run(ctx, nil, nil, c.opts.Docker.Command(env, "rm", "--force", "--volumes", name))
	if err != nil {
		return fmt.Errorf("docker rm --force --volumes %s: %w", name, err)
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

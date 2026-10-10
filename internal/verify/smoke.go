package verify

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/springledev/ballast/internal/catalog"
	"github.com/springledev/ballast/internal/strategy"
)

// Restart-smoke defaults.
const (
	// DefaultSmokeTimeout bounds the whole restart-smoke check when the
	// catalog gives no smoke.timeout: long enough for a service to boot
	// after a stop-start backup, short enough to report a dead one.
	DefaultSmokeTimeout = 2 * time.Minute

	// smokePoll is the pause between attempts while the container is
	// still starting.
	smokePoll = 2 * time.Second

	// probeTimeoutSecs bounds one HTTP/TCP probe attempt.
	probeTimeoutSecs = "5"
)

// RestartSmoke is the restart-smoke checker (spec §7.4, §17.6): after
// a backup, the service's container must be running — and healthy, if
// it has a Docker health check — and, when the catalog declares one,
// an HTTP or TCP probe against the container's own localhost must
// succeed. The probe runs in a throwaway helper container that shares
// the service container's network, on the service's own Docker host,
// so it works the same for local and remote hosts and needs no
// published port. The check keeps retrying until the timeout, because
// a freshly restarted service takes a while to come up.
type RestartSmoke struct {
	service   string
	container string
	timeout   time.Duration
	poll      time.Duration
	http      *catalog.SmokeHTTP
	tcp       *catalog.SmokeTCP
	helper    string
	opts      Options
}

// newRestartSmoke is the restart-smoke Factory.
func newRestartSmoke(entry *catalog.Entry, opts Options) (Checker, error) {
	if opts.Runner == nil {
		return nil, fmt.Errorf("service %s: restart-smoke verification needs a command runner", entry.Service)
	}
	sm := entry.Verify.Smoke
	if sm == nil || sm.Container == "" {
		return nil, fmt.Errorf("service %s: restart-smoke verification needs verify.smoke.container (the container to check)", entry.Service)
	}
	timeout := DefaultSmokeTimeout
	if sm.Timeout != "" {
		d, err := time.ParseDuration(sm.Timeout)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("service %s: verify.smoke.timeout %q is not a valid duration (examples: 90s, 3m)", entry.Service, sm.Timeout)
		}
		timeout = d
	}
	if sm.HTTP != nil && sm.TCP != nil {
		return nil, fmt.Errorf("service %s: verify.smoke may declare an http or a tcp probe, not both", entry.Service)
	}
	helper := opts.HelperImage
	if helper == "" {
		helper = strategy.DefaultVolumeHelperImage
	}
	return &RestartSmoke{
		service:   entry.Service,
		container: sm.Container,
		timeout:   timeout,
		poll:      smokePoll,
		http:      sm.HTTP,
		tcp:       sm.TCP,
		helper:    helper,
		opts:      opts,
	}, nil
}

// Method implements Checker.
func (*RestartSmoke) Method() string { return MethodRestartSmoke }

// Check implements Checker.
func (s *RestartSmoke) Check(ctx context.Context, req Request) Result {
	start := time.Now()
	res := Result{Method: MethodRestartSmoke}
	cctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	var last, output string
	for {
		ok, final, why, out := s.attempt(cctx, req.Env)
		if out != "" {
			output = out
		}
		if ok {
			res.Passed = true
			res.Detail = s.passDetail()
			res.Output = output
			res.Duration = time.Since(start)
			return res
		}
		last = why
		if final {
			break
		}
		if err := sleepOrDone(cctx, s.poll); err != nil {
			break
		}
	}
	res.Output = output
	res.Duration = time.Since(start)
	if ctx.Err() != nil {
		res.Detail = fmt.Sprintf("The check of container %s was cancelled before it finished.", s.container)
		return res
	}
	res.Detail = fmt.Sprintf("Container %s did not come back healthy after the backup (waited %s): %s", s.container, s.timeout, last)
	return res
}

// passDetail describes a passed check in plain English.
func (s *RestartSmoke) passDetail() string {
	switch {
	case s.http != nil:
		return fmt.Sprintf("Container %s is running and answered an HTTP request on port %d (a status of %s is healthy).", s.container, s.http.Port, s.expectedText())
	case s.tcp != nil:
		return fmt.Sprintf("Container %s is running and accepts connections on port %d.", s.container, s.tcp.Port)
	default:
		return fmt.Sprintf("Container %s is running after the backup.", s.container)
	}
}

// acceptable reports whether got is an accepted HTTP status.
func (s *RestartSmoke) acceptable(got int) bool {
	if s.http != nil && len(s.http.ExpectStatuses) > 0 {
		for _, want := range s.http.ExpectStatuses {
			if got == want {
				return true
			}
		}
		return false
	}
	return got == s.expectStatus()
}

// expectedText describes the accepted statuses in plain English.
func (s *RestartSmoke) expectedText() string {
	if s.http != nil && len(s.http.ExpectStatuses) > 0 {
		parts := make([]string, len(s.http.ExpectStatuses))
		for i, v := range s.http.ExpectStatuses {
			parts[i] = strconv.Itoa(v)
		}
		return strings.Join(parts, " or ")
	}
	return strconv.Itoa(s.expectStatus())
}

// expectStatus is the HTTP status the probe must return.
func (s *RestartSmoke) expectStatus() int {
	if s.http != nil && s.http.ExpectStatus != 0 {
		return s.http.ExpectStatus
	}
	return 200
}

// attempt runs one round of the check. ok means it passed; final means
// retrying cannot help (the health check itself reports unhealthy).
// why explains a failure; out is captured probe output, if any.
func (s *RestartSmoke) attempt(ctx context.Context, env []string) (ok, final bool, why, out string) {
	res, err := s.opts.Runner.Run(ctx, nil, nil, s.opts.Docker.Command(env,
		"inspect", "--format", "{{.State.Status}}|{{if .State.Health}}{{.State.Health.Status}}{{end}}", s.container))
	if err != nil {
		return false, false, "could not inspect the container: " + err.Error(), ""
	}
	status, health, _ := strings.Cut(strings.TrimSpace(string(res.Stdout)), "|")
	switch {
	case status != "running":
		return false, false, fmt.Sprintf("the container is %s, not running", orUnknown(status)), ""
	case health == "unhealthy":
		return false, true, "Docker's own health check reports the container as unhealthy", ""
	case health == "starting":
		return false, false, "Docker's health check is still starting", ""
	}
	switch {
	case s.http != nil:
		return s.probeHTTP(ctx, env)
	case s.tcp != nil:
		return s.probeTCP(ctx, env)
	}
	return true, false, "", ""
}

// probeArgs returns the `docker run` prefix for a helper container that
// shares the service container's network namespace.
func (s *RestartSmoke) probeArgs(cmd ...string) []string {
	return append([]string{"run", "--rm", "--network", "container:" + s.container, s.helper}, cmd...)
}

// httpStatusLine matches a status line in busybox wget -S output.
var httpStatusLine = regexp.MustCompile(`HTTP/[0-9.]+ ([0-9]{3})`)

// probeHTTP requests the configured path from the container's
// localhost and compares the final status (after redirects).
func (s *RestartSmoke) probeHTTP(ctx context.Context, env []string) (bool, bool, string, string) {
	path := s.http.Path
	if path == "" {
		path = "/"
	}
	url := "http://127.0.0.1:" + strconv.Itoa(s.http.Port) + path
	res, err := s.opts.Runner.Run(ctx, nil, nil, s.opts.Docker.Command(env,
		s.probeArgs("wget", "-q", "-S", "-O", "/dev/null", "-T", probeTimeoutSecs, url)...))
	out := strings.TrimSpace(string(res.Stderr))
	matches := httpStatusLine.FindAllStringSubmatch(out, -1)
	if len(matches) == 0 {
		why := fmt.Sprintf("no HTTP answer on port %d", s.http.Port)
		if err != nil {
			why += ": " + err.Error()
		}
		return false, false, why, out
	}
	got, _ := strconv.Atoi(matches[len(matches)-1][1])
	if !s.acceptable(got) {
		why := fmt.Sprintf("HTTP request to %s returned status %d, expected %s", path, got, s.expectedText())
		if got == 401 {
			why += ": the server answered 401, so it is probably password-protected; set the expected status to 401 if that is intended"
		}
		return false, false, why, out
	}
	return true, false, "", out
}

// probeTCP opens a TCP connection to the container's localhost.
func (s *RestartSmoke) probeTCP(ctx context.Context, env []string) (bool, bool, string, string) {
	res, err := s.opts.Runner.Run(ctx, nil, nil, s.opts.Docker.Command(env,
		s.probeArgs("nc", "-z", "-w", probeTimeoutSecs, "127.0.0.1", strconv.Itoa(s.tcp.Port))...))
	out := strings.TrimSpace(string(res.Stderr))
	if err != nil {
		return false, false, fmt.Sprintf("nothing accepts connections on port %d", s.tcp.Port), out
	}
	return true, false, "", out
}

// orUnknown names an empty container status.
func orUnknown(s string) string {
	if s == "" {
		return "in an unknown state"
	}
	return s
}

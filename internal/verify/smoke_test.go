package verify

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/springledev/ballast/internal/catalog"
	"github.com/springledev/ballast/internal/strategy"
	"github.com/springledev/keel/dockerhost"
	"github.com/springledev/keel/runner"
)

// smokeDockerStub is a fake docker CLI covering the subcommands the
// restart-smoke checker runs: inspect (state/health), run (the
// throwaway wget/nc probe helper). Behaviour is steered through
// environment variables so tests stay hermetic — no docker daemon is
// needed:
//
//	STUB_LOG             one argv line per invocation (always)
//	STUB_COUNTER         file counting inspect calls, to walk
//	                     STUB_INSPECT_SEQ
//	STUB_INSPECT_SEQ     ';'-separated "status|health" answers, one
//	                     per successive inspect call; the last entry
//	                     repeats once the sequence is exhausted
//	STUB_WGET_STDERR     the probe helper's wget -S writes this to
//	                     stderr (status lines)
//	STUB_WGET_EXIT       the wget invocation's exit status
//	STUB_NC_EXIT         the nc invocation's exit status
const smokeDockerStub = `#!/bin/sh
printf '%s\n' "$*" >> "$STUB_LOG"
cmd="$1"

case "$cmd" in
  inspect)
    n=0
    if [ -f "$STUB_COUNTER" ]; then n=$(cat "$STUB_COUNTER"); fi
    n=$((n+1))
    echo "$n" > "$STUB_COUNTER"
    i=1
    out=""
    OLDIFS=$IFS
    IFS=';'
    for entry in $STUB_INSPECT_SEQ; do
      out="$entry"
      if [ "$i" -ge "$n" ]; then break; fi
      i=$((i+1))
    done
    IFS=$OLDIFS
    printf '%s\n' "$out"
    exit 0
    ;;
  run)
    case "$*" in
      *wget*)
        printf '%s' "$STUB_WGET_STDERR" >&2
        exit "${STUB_WGET_EXIT:-0}"
        ;;
      *nc*)
        exit "${STUB_NC_EXIT:-0}"
        ;;
    esac
    exit 0
    ;;
esac
exit 0
`

// smokeStubSetup installs the smoke docker stub and returns the
// options to give For, the invocation log path, and a function adding
// stub-steering environment to the runner's BaseEnv.
func smokeStubSetup(t *testing.T) (opts Options, logPath string, withEnv func(...string)) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "docker.log")
	counter := filepath.Join(dir, "inspect.count")
	bin := filepath.Join(dir, "docker-stub")
	if err := os.WriteFile(bin, []byte(smokeDockerStub), 0o755); err != nil {
		t.Fatalf("writing docker stub: %v", err)
	}
	r := &runner.Runner{BaseEnv: append(os.Environ(), "STUB_LOG="+logPath, "STUB_COUNTER="+counter)}
	return Options{Runner: r, Docker: dockerhost.NewLocal(bin)}, logPath, func(extra ...string) {
		r.BaseEnv = append(append([]string(nil), r.BaseEnv...), extra...)
	}
}

// smokeChecker builds a restart-smoke checker through For (so the
// registry's factory validation runs too) and returns it as its
// concrete type for field overrides (poll, timeout).
func smokeChecker(t *testing.T, opts Options, container string, http *catalog.SmokeHTTP, tcp *catalog.SmokeTCP) *RestartSmoke {
	t.Helper()
	entry := &catalog.Entry{
		Service: "svc",
		Verify: catalog.Verify{
			Method: "restart-smoke",
			Smoke:  &catalog.Smoke{Container: container, HTTP: http, TCP: tcp},
		},
	}
	checkers, err := For(entry, opts)
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	rs, ok := checkers[0].(*RestartSmoke)
	if !ok {
		t.Fatalf("checkers[0] = %T, want *RestartSmoke", checkers[0])
	}
	return rs
}

// TestRestartSmokeChecks is the hermetic table walk of spec §7.4: the
// container's running/health state gates everything, an unhealthy
// verdict is a final failure (no point waiting out the timeout), and
// the optional HTTP/TCP probe decides the rest.
//
// This drives RestartSmoke.attempt directly rather than looping
// through Check's retry-until-timeout wrapper: attempt is exactly one
// synchronous round with no timing dependency, so the table can pin
// each failure's exact wording without racing a deadline against
// process-spawn overhead (Check's own retry/timeout wiring is
// exercised separately, by TestRestartSmokeCheck, without pinning
// wording on the timed-out cases — see its comment for why).
func TestRestartSmokeChecks(t *testing.T) {
	tests := []struct {
		name       string
		inspectSeq string
		http       *catalog.SmokeHTTP
		tcp        *catalog.SmokeTCP
		wgetStderr string
		setWget    bool
		wgetExit   string
		ncExit     string
		wantOK     bool
		wantFinal  bool
		wantWhy    string
	}{
		{
			name:       "running no healthcheck no probe passes",
			inspectSeq: "running|",
			wantOK:     true,
		},
		{
			name:       "health starting is not final (worth retrying)",
			inspectSeq: "running|starting",
			wantOK:     false,
			wantFinal:  false,
			wantWhy:    "still starting",
		},
		{
			name:       "unhealthy is a final failure, not waiting for the timeout",
			inspectSeq: "running|unhealthy",
			wantOK:     false,
			wantFinal:  true,
			wantWhy:    "unhealthy",
		},
		{
			name:       "exited is not running",
			inspectSeq: "exited|",
			wantOK:     false,
			wantFinal:  false,
			wantWhy:    "exited",
		},
		{
			name:       "http 200 passes",
			inspectSeq: "running|",
			http:       &catalog.SmokeHTTP{Port: 80, Path: "/alive"},
			wgetStderr: "HTTP/1.1 200 OK\n",
			setWget:    true,
			wantOK:     true,
		},
		{
			name:       "http 404 fails mentioning the status",
			inspectSeq: "running|",
			http:       &catalog.SmokeHTTP{Port: 80, Path: "/missing"},
			wgetStderr: "HTTP/1.1 404 Not Found\n",
			setWget:    true,
			wantOK:     false,
			wantFinal:  false,
			wantWhy:    "404",
		},
		{
			name:       "http redirect chain whose final status is 200 passes",
			inspectSeq: "running|",
			http:       &catalog.SmokeHTTP{Port: 80, Path: "/"},
			wgetStderr: "HTTP/1.1 302 Found\nHTTP/1.1 200 OK\n",
			setWget:    true,
			wantOK:     true,
		},
		{
			name:       "http custom expectStatus passes",
			inspectSeq: "running|",
			http:       &catalog.SmokeHTTP{Port: 80, Path: "/", ExpectStatus: 201},
			wgetStderr: "HTTP/1.1 201 Created\n",
			setWget:    true,
			wantOK:     true,
		},
		{
			name:       "expectStatuses accepts 401",
			inspectSeq: "running|",
			http:       &catalog.SmokeHTTP{Port: 80, Path: "/_up", ExpectStatuses: []int{200, 401}},
			wgetStderr: "HTTP/1.1 401 Unauthorized\n",
			setWget:    true,
			wantOK:     true,
		},
		{
			name:       "401 not expected explains password protection",
			inspectSeq: "running|",
			http:       &catalog.SmokeHTTP{Port: 80, Path: "/_up"},
			wgetStderr: "HTTP/1.1 401 Unauthorized\n",
			setWget:    true,
			wantOK:     false,
			wantWhy:    "probably password-protected",
		},
		{
			name:       "no HTTP answer fails",
			inspectSeq: "running|",
			http:       &catalog.SmokeHTTP{Port: 80, Path: "/"},
			wgetExit:   "1",
			setWget:    true,
			wantOK:     false,
			wantFinal:  false,
			wantWhy:    "no HTTP answer",
		},
		{
			name:       "tcp passes",
			inspectSeq: "running|",
			tcp:        &catalog.SmokeTCP{Port: 80},
			wantOK:     true,
		},
		{
			name:       "tcp fails",
			inspectSeq: "running|",
			tcp:        &catalog.SmokeTCP{Port: 81},
			ncExit:     "1",
			wantOK:     false,
			wantFinal:  false,
			wantWhy:    "port 81",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts, _, withEnv := smokeStubSetup(t)
			withEnv("STUB_INSPECT_SEQ=" + tt.inspectSeq)
			if tt.setWget {
				withEnv("STUB_WGET_STDERR=" + tt.wgetStderr)
			}
			if tt.wgetExit != "" {
				withEnv("STUB_WGET_EXIT=" + tt.wgetExit)
			}
			if tt.ncExit != "" {
				withEnv("STUB_NC_EXIT=" + tt.ncExit)
			}
			rs := smokeChecker(t, opts, "svc_app", tt.http, tt.tcp)

			ok, final, why, _ := rs.attempt(context.Background(), nil)

			if ok != tt.wantOK {
				t.Fatalf("attempt() ok = %v, want %v (why: %q)", ok, tt.wantOK, why)
			}
			if !ok && final != tt.wantFinal {
				t.Errorf("attempt() final = %v, want %v", final, tt.wantFinal)
			}
			if tt.wantWhy != "" && !strings.Contains(why, tt.wantWhy) {
				t.Errorf("attempt() why = %q, want substring %q", why, tt.wantWhy)
			}
		})
	}
}

// TestRestartSmokeCheck exercises Check's own retry-until-timeout
// wrapper end to end: a state that never settles is retried (not
// returned immediately) and the eventual failure is bounded by the
// timeout, an unhealthy state is final and returns promptly without
// waiting the timeout out, and a state that resolves mid-retry passes.
// It intentionally does not pin the wording of a timed-out failure:
// Check's last attempt can have its subprocess cut off mid-flight by
// the very deadline that ends the loop, which surfaces as a generic
// cancellation message instead of the substantive one — an inherent
// race of "retry a non-final failure until the deadline", not a
// checker bug. TestRestartSmokeChecks pins exact wording without that
// race, by calling attempt() directly.
func TestRestartSmokeCheck(t *testing.T) {
	t.Run("passes immediately with no healthcheck and no probe", func(t *testing.T) {
		opts, _, withEnv := smokeStubSetup(t)
		withEnv("STUB_INSPECT_SEQ=running|")
		rs := smokeChecker(t, opts, "svc_app", nil, nil)
		rs.poll = 10 * time.Millisecond
		rs.timeout = 2 * time.Second

		res := rs.Check(context.Background(), Request{})
		if !res.Passed {
			t.Fatalf("Check() failed: %q", res.Detail)
		}
		if res.Method != MethodRestartSmoke {
			t.Errorf("Check() method = %q, want %q", res.Method, MethodRestartSmoke)
		}
		if !strings.Contains(res.Detail, "is running after the backup") {
			t.Errorf("Check() detail = %q, want the running-pass wording", res.Detail)
		}
	})

	t.Run("passes after a starting-then-healthy retry", func(t *testing.T) {
		opts, _, withEnv := smokeStubSetup(t)
		withEnv("STUB_INSPECT_SEQ=running|starting;running|healthy")
		rs := smokeChecker(t, opts, "svc_app", nil, nil)
		rs.poll = 10 * time.Millisecond
		rs.timeout = 2 * time.Second

		res := rs.Check(context.Background(), Request{})
		if !res.Passed {
			t.Fatalf("Check() failed: %q", res.Detail)
		}
	})

	t.Run("unhealthy fails immediately, not waiting for the timeout", func(t *testing.T) {
		opts, _, withEnv := smokeStubSetup(t)
		withEnv("STUB_INSPECT_SEQ=running|unhealthy")
		rs := smokeChecker(t, opts, "svc_app", nil, nil)
		rs.poll = 10 * time.Millisecond
		rs.timeout = 3 * time.Second

		start := time.Now()
		res := rs.Check(context.Background(), Request{})
		elapsed := time.Since(start)

		if res.Passed {
			t.Fatal("Check() passed against an unhealthy container, want failure")
		}
		if !strings.Contains(res.Detail, "unhealthy") {
			t.Errorf("Check() detail = %q, want it to mention unhealthy", res.Detail)
		}
		if elapsed > 500*time.Millisecond {
			t.Errorf("Check() took %s, want well under the 3s timeout (a final failure must not wait it out)", elapsed)
		}
	})

	t.Run("a state that never settles fails once the timeout is reached", func(t *testing.T) {
		opts, _, withEnv := smokeStubSetup(t)
		withEnv("STUB_INSPECT_SEQ=exited|")
		rs := smokeChecker(t, opts, "svc_app", nil, nil)
		rs.poll = 30 * time.Millisecond
		rs.timeout = 300 * time.Millisecond

		start := time.Now()
		res := rs.Check(context.Background(), Request{})
		elapsed := time.Since(start)

		if res.Passed {
			t.Fatal("Check() passed against a container stuck exited, want failure")
		}
		if !strings.Contains(res.Detail, "did not come back healthy") {
			t.Errorf("Check() detail = %q, want the timeout-exhausted wording", res.Detail)
		}
		if elapsed < rs.timeout {
			t.Errorf("Check() took %s, want at least the %s timeout (it must actually retry)", elapsed, rs.timeout)
		}
		if elapsed > 3*time.Second {
			t.Errorf("Check() took %s, want it bounded by the timeout plus a little slack", elapsed)
		}
	})
}

// TestRestartSmokeHTTPProbeArgs pins the exact docker argv of the HTTP
// probe: a throwaway helper container sharing the service container's
// network namespace, running busybox wget with -S (so status lines
// land on stderr) against the container's own localhost.
func TestRestartSmokeHTTPProbeArgs(t *testing.T) {
	opts, logPath, withEnv := smokeStubSetup(t)
	withEnv("STUB_INSPECT_SEQ=running|", "STUB_WGET_STDERR=HTTP/1.1 200 OK\n")

	rs := smokeChecker(t, opts, "svc_app", &catalog.SmokeHTTP{Port: 8080, Path: "/health"}, nil)
	rs.poll = 10 * time.Millisecond
	rs.timeout = 2 * time.Second

	res := rs.Check(context.Background(), Request{})
	if !res.Passed {
		t.Fatalf("Check() failed: %q", res.Detail)
	}

	log := readStubLog(t, logPath)
	wantInspect := "inspect --format {{.State.Status}}|{{if .State.Health}}{{.State.Health.Status}}{{end}} svc_app"
	wantWget := "run --rm --network container:svc_app " + strategy.DefaultVolumeHelperImage +
		" wget -q -S -O /dev/null -T 5 http://127.0.0.1:8080/health"
	if len(log) != 2 || log[0] != wantInspect || log[1] != wantWget {
		t.Fatalf("docker invocations = %v, want exactly [%q, %q]", log, wantInspect, wantWget)
	}
}

// TestRestartSmokeTCPProbeArgs pins the exact docker argv of the TCP
// probe: the same throwaway helper container, running busybox nc -z.
func TestRestartSmokeTCPProbeArgs(t *testing.T) {
	opts, logPath, withEnv := smokeStubSetup(t)
	withEnv("STUB_INSPECT_SEQ=running|")

	rs := smokeChecker(t, opts, "svc_app", nil, &catalog.SmokeTCP{Port: 8080})
	rs.poll = 10 * time.Millisecond
	rs.timeout = 2 * time.Second

	res := rs.Check(context.Background(), Request{})
	if !res.Passed {
		t.Fatalf("Check() failed: %q", res.Detail)
	}

	log := readStubLog(t, logPath)
	wantNC := "run --rm --network container:svc_app " + strategy.DefaultVolumeHelperImage +
		" nc -z -w 5 127.0.0.1 8080"
	if len(log) != 2 || log[1] != wantNC {
		t.Fatalf("docker invocations = %v, want the second one to be %q", log, wantNC)
	}
}

// TestRestartSmokeCustomHelperImage: Options.HelperImage overrides the
// default probe helper image.
func TestRestartSmokeCustomHelperImage(t *testing.T) {
	opts, logPath, withEnv := smokeStubSetup(t)
	withEnv("STUB_INSPECT_SEQ=running|")
	opts.HelperImage = "busybox:1.36"

	rs := smokeChecker(t, opts, "svc_app", nil, &catalog.SmokeTCP{Port: 80})
	rs.poll = 10 * time.Millisecond
	rs.timeout = 2 * time.Second

	res := rs.Check(context.Background(), Request{})
	if !res.Passed {
		t.Fatalf("Check() failed: %q", res.Detail)
	}
	log := readStubLog(t, logPath)
	if len(log) != 2 || !strings.Contains(log[1], "busybox:1.36") {
		t.Fatalf("docker invocations = %v, want the probe run with the custom helper image", log)
	}
}

// TestRestartSmokeContextCancelled: cancelling the job while the
// container never settles must fail promptly (never hang until the
// timeout), and the result is never a pass.
func TestRestartSmokeContextCancelled(t *testing.T) {
	opts, _, withEnv := smokeStubSetup(t)
	withEnv("STUB_INSPECT_SEQ=running|starting") // never settles

	rs := smokeChecker(t, opts, "svc_app", nil, nil)
	rs.poll = 10 * time.Millisecond
	rs.timeout = time.Minute // must not be reached

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	res := rs.Check(ctx, Request{})
	elapsed := time.Since(start)

	if res.Passed {
		t.Fatal("Check() passed with a cancelled context, want failure")
	}
	if !strings.Contains(res.Detail, "cancelled") {
		t.Errorf("Check() detail = %q, want it to name the cancellation", res.Detail)
	}
	if elapsed > 5*time.Second {
		t.Errorf("Check() took %s to notice cancellation, want it prompt", elapsed)
	}
}

// TestRestartSmokeRealDocker exercises the checker against a real
// container: an HTTP probe that must pass and one that must 404, a
// TCP probe on the listening port and one on a port nothing answers
// on, and finally the container is stopped and the check must fail
// within a short timeout. Skipped without Docker or in -short mode,
// mirroring the gating the pipeline integration tests use.
func TestRestartSmokeRealDocker(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not installed; skipping integration test")
	}
	for _, img := range []string{"busybox:latest", strategy.DefaultVolumeHelperImage} {
		if err := exec.Command("docker", "pull", img).Run(); err != nil {
			t.Skipf("could not pull %s: %v", img, err)
		}
	}

	ctx := context.Background()
	name := "ballast-smoke-test-" + randomSuffix()
	script := "mkdir -p /www && echo ok > /www/alive && exec httpd -f -p 80 -h /www"
	if out, err := exec.Command("docker", "run", "-d", "--name", name,
		"busybox:latest", "sh", "-c", script).CombinedOutput(); err != nil {
		t.Skipf("could not start busybox test container: %v: %s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "--force", name).Run()
	})

	opts := Options{Runner: &runner.Runner{}, Docker: dockerhost.NewLocal("docker")}

	t.Run("http alive passes", func(t *testing.T) {
		rs := smokeChecker(t, opts, name, &catalog.SmokeHTTP{Port: 80, Path: "/alive"}, nil)
		rs.timeout = 15 * time.Second
		rs.poll = 300 * time.Millisecond
		res := rs.Check(ctx, Request{})
		if !res.Passed {
			t.Fatalf("Check() failed: %q", res.Detail)
		}
	})

	t.Run("http missing 404s", func(t *testing.T) {
		// The checker retries a non-final HTTP failure until its
		// timeout; the exact wording of the LAST attempt's failure is
		// pinned deterministically in TestRestartSmokeChecks (a real
		// run can have its very last probe's subprocess cut off
		// mid-flight by the deadline, which is an inherent race of
		// "retry until timeout" and not itself a bug) — here only the
		// verdict matters.
		rs := smokeChecker(t, opts, name, &catalog.SmokeHTTP{Port: 80, Path: "/missing"}, nil)
		rs.timeout = 5 * time.Second
		rs.poll = 300 * time.Millisecond
		res := rs.Check(ctx, Request{})
		if res.Passed {
			t.Fatal("Check() passed for a missing path, want a 404 failure")
		}
		t.Logf("Check() detail = %q", res.Detail)
	})

	t.Run("tcp 80 passes", func(t *testing.T) {
		rs := smokeChecker(t, opts, name, nil, &catalog.SmokeTCP{Port: 80})
		rs.timeout = 15 * time.Second
		rs.poll = 300 * time.Millisecond
		res := rs.Check(ctx, Request{})
		if !res.Passed {
			t.Fatalf("Check() failed: %q", res.Detail)
		}
	})

	t.Run("tcp 81 fails", func(t *testing.T) {
		rs := smokeChecker(t, opts, name, nil, &catalog.SmokeTCP{Port: 81})
		rs.timeout = 3 * time.Second
		rs.poll = 300 * time.Millisecond
		res := rs.Check(ctx, Request{})
		if res.Passed {
			t.Fatal("Check() passed against a port nothing listens on, want failure")
		}
	})

	t.Run("stopped container fails within a short timeout", func(t *testing.T) {
		if out, err := exec.Command("docker", "stop", name).CombinedOutput(); err != nil {
			t.Fatalf("docker stop: %v: %s", err, out)
		}
		rs := smokeChecker(t, opts, name, nil, nil)
		rs.timeout = 5 * time.Second
		rs.poll = 300 * time.Millisecond
		start := time.Now()
		res := rs.Check(ctx, Request{})
		elapsed := time.Since(start)
		if res.Passed {
			t.Fatal("Check() passed against a stopped container, want failure")
		}
		if elapsed > 10*time.Second {
			t.Errorf("Check() took %s to notice the stopped container, want it bounded by the short timeout", elapsed)
		}
	})
}

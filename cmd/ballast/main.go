// Command ballast is the single static binary for the Ballast backup
// tool: `ballast serve` hosts the API and web GUI, while `ballast
// backup`, `run-due` and friends drive backups from cron/systemd.
//
// This is the scaffolding step (spec §14 step 1): a working Cobra CLI
// with a minimal `serve` that exposes the embedded stub page and a
// `version` command. Later §14 steps fill in the engine behind these
// commands.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/springledev/ballast/internal/api"
	"github.com/springledev/ballast/internal/buildinfo"
	"github.com/springledev/ballast/internal/config"
	"github.com/springledev/ballast/internal/notify"
	"github.com/springledev/ballast/internal/scheduler"
	"github.com/springledev/ballast/internal/store"
	"github.com/springledev/ballast/web"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	execrunner "github.com/springledev/keel/runner"
)

// version is the build version, overridable at build time with
// -ldflags "-X main.version=...". Defaults to a plain dev marker.
var version = "dev"

// buildVersion is the version to report: the stamped one, or the git
// commit the Go toolchain recorded for an unstamped source build.
func buildVersion() string {
	bi, _ := debug.ReadBuildInfo()
	return buildinfo.Resolve(version, bi)
}

var rootCmd = &cobra.Command{
	Use:   "ballast",
	Short: "Service-aware, verified-restorable backups for Docker homelabs",
	Long: `Ballast backs up Docker-hosted services using per-service
strategies from a YAML catalog, ships them via restic, and verifies
that every backup actually restores before marking it done.`,
	SilenceUsage:  true,
	SilenceErrors: true,
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Args:  noArgs,
	Short: "Print the ballast version",
	RunE: func(cmd *cobra.Command, args []string) error {
		cmd.Println(buildVersion())
		return nil
	},
}

var serveAddr string

var serveCmd = &cobra.Command{
	Args:  noArgs,
	Use:   "serve",
	Short: "Serve the Web UI and REST API",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		a, err := openApp(cfg, cmd.ErrOrStderr())
		if err != nil {
			return err
		}
		defer a.Close()
		a.warnReservedName()
		tokenDir := execrunner.FindCredentialDir(apiTokenCredential, cfg.CredentialDirs()...)
		tokenPath := filepath.Join(tokenDir, apiTokenCredential)
		if a.apiToken == "" {
			tok, err := api.GenerateToken()
			if err != nil {
				return fmt.Errorf("generating an access key: %w", err)
			}
			if err := writeAPIToken(tokenPath, tok); err != nil {
				return fmt.Errorf("no access key exists yet and Ballast could not create one: %w", err)
			}
			a.apiToken = tok
			a.secrets.Add(tok)
			// Printed once, here, to the terminal only (never to the log
			// writer); it is also saved root-only at tokenPath.
			fmt.Fprintf(cmd.ErrOrStderr(), "ballast: first start — your access key is:\n\n    %s\n\nSave it now. It is stored in %s (readable only by its owner); `ballast token rotate` makes a new one.\n", tok, tokenPath)
		}

		// A backup started by POST .../backup must outlive that single
		// request (net/http cancels a request's Context() the instant
		// its handler returns — almost immediately for a 202-Accepted
		// endpoint) but still be canceled when the server itself shuts
		// down: give it the server's own long-lived context instead of
		// each request's (see api.Ops.Ctx).
		ctx := cmd.Context()
		a.ops.Ctx = ctx

		webUI, err := web.Handler()
		if err != nil {
			return fmt.Errorf("loading embedded web UI: %w", err)
		}
		listen := cfg.Listen
		if cmd.Flags().Changed("listen") {
			listen = serveAddr
			if err := config.ValidateListen(listen); err != nil {
				return fmt.Errorf("--listen: %w", err)
			}
		}
		auth := api.NewAuth(a.apiToken, func(tok string) error {
			if err := writeAPIToken(tokenPath, tok); err != nil {
				return err
			}
			a.secrets.Add(tok)
			return nil
		}, listen)
		a.ops.ListenAddr = listen
		a.ops.InTerminal = startedFromTerminal(os.Getenv, os.Stdin)
		mux := http.NewServeMux()
		mux.Handle("/api/v1/", api.NewAuthedHandler(a.ops, auth))
		mux.Handle("/", webUI)

		ln, err := net.Listen("tcp", listen)
		if err != nil {
			return fmt.Errorf("starting web server: %w", err)
		}
		log.Printf("ballast %s listening on http://%s", version, ln.Addr())
		if !api.ListenIsLoopback(listen) {
			log.Printf("ballast: listening beyond this machine over plain HTTP; put Ballast behind a reverse proxy with HTTPS (or a tailnet) before exposing it")
		}

		// Runs a crashed or powered-off Ballast left unfinished are failed
		// now, whether or not the service has a schedule (the sweep only
		// looks at scheduled ones), so none lingers as "running".
		if _, err := a.coord.RecoverInterrupted(ctx); err != nil {
			a.logf("clearing runs left over from the last time Ballast stopped: %v", err)
		}

		schedDone := make(chan struct{})
		go func() {
			defer close(schedDone)
			runScheduleLoop(ctx, a)
		}()

		serveErr := serveUntilDone(ctx, ln, mux)
		<-schedDone
		a.ops.Wait()
		return serveErr
	},
}

// startedFromTerminal reports whether serve was started by hand in a
// terminal rather than by a service manager. systemd sets
// INVOCATION_ID for every unit it runs.
func startedFromTerminal(getenv func(string) string, stdin *os.File) bool {
	if getenv("INVOCATION_ID") != "" {
		return false
	}
	info, err := stdin.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// runScheduleLoop runs the in-process scheduler: every minute, aligned
// to the wall-clock minute boundary, it sweeps due jobs (ballast
// run-due's own logic, shared via the same Coordinator) and, nightly,
// freshness alerts. Each tick's sweep runs in its own goroutine so one
// long-running backup (e.g. a multi-hour Immich run) never delays the
// next minute's sweep from starting for every other service — run
// locks already make overlapping sweeps safe (spec §14 step 9:
// "a service never runs twice at once"). It returns once ctx is
// cancelled (serve shutting down), after every sweep it started has
// finished.
func runScheduleLoop(ctx context.Context, a *app) {
	if a.sweeps == nil {
		a.sweeps = &sweepWatch{}
	}
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		ticks, stop := scheduleTicker()
		select {
		case <-ctx.Done():
			stop()
			return
		case now := <-ticks:
			a.sweeps.begin(now)
			if a.heartbeat != nil {
				// Pinged from the loop's own tick, so a loop that has
				// stopped stops the heartbeat. A sweep that has hung
				// (see sweepWatch) stops it too, so a stuck daemon is
				// noticed even though its ticks go on. A failed ping is
				// logged inside Beat and never touches the sweep (spec
				// §21.7). The check runs here, not in the loop, so a hung
				// database cannot stop the ticks.
				wg.Add(1)
				go func() {
					defer wg.Done()
					if since, stalled := a.sweeps.stalled(now, sweepStuckLimit); stalled {
						if a.sweeps.changed(true) {
							a.logf("heartbeat: no scheduler sweep has finished since the one that started at %s, so the heartbeat is paused until one does", since.Local().Format(time.RFC3339))
						}
						return
					}
					if a.sweeps.changed(false) {
						a.logf("heartbeat: the scheduler is moving again, heartbeat resumed")
					}
					a.heartbeat.Beat(ctx, now)
				}()
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := sweepRun(ctx, a, now); err != nil {
					a.logf("run-due sweep: %v", err)
				}
				a.sweeps.finish(now)
			}()
		}
	}
}

// sweepRun is what each tick runs; a variable so tests can substitute a
// sweep that hangs.
var sweepRun = sweepDue

// scheduleTicker returns a channel that delivers the next wall-clock
// minute boundary and a function that cancels it. A variable so tests
// can drive the scheduler loop without waiting for real minutes.
var scheduleTicker = func() (<-chan time.Time, func()) {
	timer := time.NewTimer(time.Until(nextMinuteBoundary(time.Now())))
	return timer.C, func() { timer.Stop() }
}

// sweepDue is the one job-engine entry point behind both triggers:
// the daemon's scheduler loop calls it every minute and `ballast
// run-due` calls it once. It runs every due job through the
// Coordinator, then the nightly freshness sweep (for headless setups
// that never run `ballast serve`), so a job started either way
// produces the same run states, notifications and verification
// results (spec §18.3). The freshness sweep never hides the sweep's
// own error.
func sweepDue(ctx context.Context, a *app, now time.Time) error {
	if serr := a.ops.EnsureSelfBackupSchedule(ctx, now); serr != nil {
		a.logf("self-backup schedule: %v", serr)
	}
	err := a.coord.RunDue(ctx, now)
	maybeSweepFreshness(ctx, a)
	maybeSendDigest(ctx, a, now)
	if a.health != nil {
		if herr := a.health.SweepDue(ctx); herr != nil {
			a.logf("repository health sweep: %v", herr)
		}
	}
	return err
}

// digestMetaKey records when the weekly digest was last delivered.
const digestMetaKey = "digest_last_sent"

// digestInterval is how often the weekly digest is sent.
const digestInterval = 7 * 24 * time.Hour

// maybeSendDigest sends the weekly digest (api.Ops.SendDigest) when at
// least digestInterval has passed since one was last delivered. It is
// only recorded as sent when a channel delivered it, so turning the
// digest on for a channel sends the first one at the next sweep. With
// no opted-in channel it does nothing; other failures are logged, never
// fatal, like the freshness sweep.
func maybeSendDigest(ctx context.Context, a *app, now time.Time) {
	if last, err := a.store.MetaGet(digestMetaKey); err == nil {
		if t, perr := time.Parse(time.RFC3339, last); perr == nil && now.Sub(t) < digestInterval {
			return
		}
	} else if !errors.Is(err, store.ErrMetaNotFound) {
		a.logf("checking weekly digest schedule: %v", err)
		return
	}
	res, err := a.ops.SendDigest(ctx)
	if errors.Is(err, api.ErrNoDigestChannels) {
		return
	}
	if err != nil {
		a.logf("weekly digest: %v", err)
	}
	if len(res.Sent) > 0 {
		if err := a.store.MetaSet(digestMetaKey, now.UTC().Format(time.RFC3339)); err != nil {
			a.logf("recording weekly digest time: %v", err)
		}
	}
}

// nextMinuteBoundary returns the next wall-clock minute boundary
// strictly after now, so the scheduler loop's sweeps land on the
// minute regardless of what moment `ballast serve` itself started at.
func nextMinuteBoundary(now time.Time) time.Time {
	return now.Truncate(time.Minute).Add(time.Minute)
}

// freshnessSweepMetaKey records when the freshness sweep last ran
// (store.Store.MetaGet/MetaSet), so it runs at most once nightly
// regardless of how often maybeSweepFreshness is called.
const freshnessSweepMetaKey = "freshness_last_sweep"

// freshnessSweepInterval is how often the freshness sweep runs (spec
// §11: freshness alerting is a nightly sweep).
const freshnessSweepInterval = 24 * time.Hour

// maybeSweepFreshness runs notify.FreshnessChecker.SweepFreshness
// when at least freshnessSweepInterval has passed since it last ran
// (or it has never run), from `ballast serve`'s scheduler loop and
// the end of `ballast run-due` (spec §14 step 11 carry-over from step
// 10). Errors (including a per-service ErrNoChannels) are logged once
// per sweep, never fatal: a freshness alert that could not be
// delivered must not stop backups from running.
func maybeSweepFreshness(ctx context.Context, a *app) {
	due := true
	if last, err := a.store.MetaGet(freshnessSweepMetaKey); err == nil {
		if t, perr := time.Parse(time.RFC3339, last); perr == nil {
			due = time.Since(t) >= freshnessSweepInterval
		}
	} else if !errors.Is(err, store.ErrMetaNotFound) {
		a.logf("checking freshness sweep schedule: %v", err)
	}
	if !due {
		return
	}
	if err := notify.NewFreshnessChecker(a.store.DB()).SweepFreshness(ctx, a.notifier); err != nil {
		a.logf("freshness sweep: %v", err)
	}
	if err := a.store.MetaSet(freshnessSweepMetaKey, time.Now().UTC().Format(time.RFC3339)); err != nil {
		a.logf("recording freshness sweep time: %v", err)
	}
}

// serveShutdownTimeout bounds how long serve waits for in-flight
// requests to finish once asked to stop, well inside systemd's default
// 90 s stop timeout.
const serveShutdownTimeout = 10 * time.Second

// serveUntilDone serves h on ln until ctx is cancelled (SIGTERM or
// Ctrl-C, see shutdownContext), then stops accepting connections, lets
// in-flight requests finish for up to serveShutdownTimeout, and returns
// nil. It returns an error if serving fails or requests are still
// running when the timeout ends. ln is closed on return.
func serveUntilDone(ctx context.Context, ln net.Listener, h http.Handler) error {
	srv := &http.Server{Handler: h}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()

	select {
	case err := <-served:
		// Serve always returns a non-nil error; it only returns
		// before Shutdown when something went wrong.
		return fmt.Errorf("web server stopped: %w", err)
	case <-ctx.Done():
	}
	log.Printf("ballast: shutting down the web server")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), serveShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		closeErr := srv.Close()
		<-served
		return errors.Join(fmt.Errorf("shutting down web server: %w", err), closeErr)
	}
	if err := <-served; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("web server stopped: %w", err)
	}
	return nil
}

// loadConfig resolves the effective configuration from the
// environment and ballast.yaml.
func loadConfig() (config.Config, error) {
	cfg, err := config.Load(os.Getenv, os.UserHomeDir)
	if err != nil {
		return config.Config{}, fmt.Errorf("loading config: %w", err)
	}
	return cfg, nil
}

// withApp opens the app (see openApp), calls fn with it, and closes
// the database afterwards. A failure to close is reported alongside
// fn's own result rather than dropped: closing is when SQLite
// checkpoints the write-ahead log. Every command uses this (or
// openApp directly, for serve, which needs the app past fn's return).
func withApp(cfg config.Config, logw io.Writer, fn func(*app) error) (err error) {
	a, err := openApp(cfg, logw)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := a.Close(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("closing database: %w", cerr))
		}
	}()
	return fn(a)
}

// triggerTokenCredentialPrefix starts the name of the credential file
// `ballast trigger <service>` reads its token from when neither --token
// nor --token-file is given. The full name is
// ballast-trigger-token.<service>, so one credential directory can hold
// every service's token, e.g.
// `LoadCredential=ballast-trigger-token.immich:/etc/ballast/immich.token`.
const triggerTokenCredentialPrefix = "ballast-trigger-token."

// triggerTokenCredential returns the credential file name holding
// service's trigger token. service must be usable as part of a plain
// file name.
func triggerTokenCredential(service string) (string, error) {
	name := triggerTokenCredentialPrefix + service
	if service == "" || filepath.Base(name) != name {
		return "", fmt.Errorf("service name %q cannot be used in a credential file name", service)
	}
	return name, nil
}

// resolveTriggerToken picks service's trigger token from, in order,
// --token-file, --token, or the credential file
// ballast-trigger-token.<service> in credDir — the credential
// directory config resolved ($CREDENTIALS_DIRECTORY under systemd
// LoadCredential=, else <config dir>/credentials). Which directory was
// used is logged to stderr (spec §8); the token never is. --token
// still works (it is the spec §9 CLI interface) but warns, because
// anything on a command line is visible to every local user via the
// process list (spec rule 2).
func resolveTriggerToken(service, flagToken, tokenFile, credDir string, stderr io.Writer) (string, error) {
	switch {
	case flagToken != "" && tokenFile != "":
		return "", fmt.Errorf("use either --token or --token-file, not both")
	case tokenFile != "":
		return scheduler.ReadTokenFile(tokenFile)
	case flagToken != "":
		fmt.Fprintln(stderr, "ballast: warning: --token is visible to other users on this machine (process list); prefer --token-file or a credential file named "+triggerTokenCredentialPrefix+service)
		return flagToken, nil
	}
	name, err := triggerTokenCredential(service)
	if err != nil {
		return "", err
	}
	path := filepath.Join(credDir, name)
	_, err = os.Stat(path)
	switch {
	case err == nil:
		fmt.Fprintf(stderr, "ballast: %s: reading trigger token from credential directory %s\n", service, credDir)
		return scheduler.ReadTokenFile(path)
	case !errors.Is(err, fs.ErrNotExist):
		// Present but unreadable: say so, rather than claiming no
		// token was given.
		return "", fmt.Errorf("checking credential file %s: %w", path, err)
	}
	return "", fmt.Errorf("no trigger token given: pass --token-file <path>, or put the token in %s (under systemd: LoadCredential=%s:<path>)", path, name)
}

var runDueCmd = &cobra.Command{
	Use:   "run-due",
	Args:  noArgs,
	Short: "Run every backup job whose schedule is due (for systemd timers/cron)",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			a.warnReservedName()
			return sweepDue(cmd.Context(), a, time.Now())
		})
	},
}

var (
	triggerToken     string
	triggerTokenFile string
)

var triggerCmd = &cobra.Command{
	Use:   "trigger <service>",
	Short: "Run one backup using a trigger token (for scripts and automations that should only be able to start a backup)",
	Long: `Run one backup job now, authenticated by that service's trigger token.

The token is read from, in order:
  --token-file <path>   a file only you can read (chmod 600)
  --token <token>       works, but other users can see it in the process list
  the credential file ballast-trigger-token.<service> in Ballast's
  credential directory ($CREDENTIALS_DIRECTORY under systemd
  LoadCredential=, otherwise <config dir>/credentials)

If the service is busy, trigger waits up to 10 seconds for it to free up,
then gives up without starting a second run.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		token, err := resolveTriggerToken(args[0], triggerToken, triggerTokenFile, execrunner.FindCredentialDir(triggerTokenCredentialPrefix+args[0], cfg.CredentialDirs()...), cmd.ErrOrStderr())
		if err != nil {
			return fmt.Errorf("triggering %s: %w", args[0], err)
		}
		return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			runErr := a.coord.Trigger(cmd.Context(), args[0], token)
			if runErr != nil {
				if !api.InterruptedOnly(runErr) {
					return runErr
				}
				// Only an EARLIER run was found interrupted and marked
				// failed while claiming the lock (spec §14 step 11 hard
				// rule 6); this trigger's own run still succeeded and
				// is verified — report the old failure as a warning,
				// not as this command failing.
				fmt.Fprintf(cmd.ErrOrStderr(), "ballast: warning: %s\n", runErr)
			}
			return nil
		})
	},
}

func init() {
	serveCmd.Flags().StringVar(&serveAddr, "listen", config.DefaultListen, "host:port for the web server (default: the `listen` setting in ballast.yaml)")
	// --addr is the older name for --listen and keeps working.
	serveCmd.Flags().SetNormalizeFunc(func(_ *pflag.FlagSet, name string) pflag.NormalizedName {
		if name == "addr" {
			name = "listen"
		}
		return pflag.NormalizedName(name)
	})
	triggerCmd.Flags().StringVar(&triggerToken, "token", "", "external trigger token for the service (visible in the process list; prefer --token-file)")
	triggerCmd.Flags().StringVar(&triggerTokenFile, "token-file", "", "path to a file (mode 0600) holding the trigger token")
	rootCmd.AddCommand(serveCmd, versionCmd, runDueCmd, triggerCmd)
	rootCmd.AddCommand(waitIdleCmd, supportBundleCmd)
	registerServiceCommands(rootCmd)
	registerDestinationCommands(rootCmd)
	registerHostCommands(rootCmd)
	registerProxmoxCommands(rootCmd)
	registerScheduleCommands(rootCmd)
	registerNotifyCommands(rootCmd)
	registerSecretCommands(rootCmd)
	rootCmd.AddCommand(backupCmd, statusCmd, snapshotsCmd, restoreCmd, restoreProjectCmd, tokenCmd, catalogCmd, wizardCmd, verifyCmd, doctorCmd, selfBackupCmd)
}

// shutdownContext returns a context cancelled by the first SIGINT or
// SIGTERM (systemd's stop signal), so commands can wind down cleanly —
// above all, so a stop-start backup restarts the containers it stopped
// (spec §4.2) instead of being killed with them down. Once the first
// signal arrives, default handling is restored: a second signal ends
// the process immediately.
func shutdownContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		stop()
	}()
	return ctx, stop
}

// configureStreams makes results go to stdout. Cobra's Print helpers
// default to stderr, which hides results from `| grep` and `$(...)`
// (spec §21.2); errors and notices stay on stderr.
func configureStreams(root *cobra.Command) {
	root.SetOut(os.Stdout)
	root.SetErr(os.Stderr)
}

// verbose is the global --verbose flag.
var verbose bool

// exitError makes the program exit with a given code without printing
// an "Error:" line, for commands whose answer is their exit code (such
// as `ballast status --running`) and have already said it on stdout.
type exitError struct{ code int }

// Error implements error.
func (e *exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

// noArgs is the Args check for a command that takes no positional
// arguments. Unlike cobra.NoArgs it says what to do instead.
func noArgs(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	return fmt.Errorf("`%s` takes no arguments, but got %q; run `%s --help` to see what it accepts", cmd.CommandPath(), strings.Join(args, " "), cmd.CommandPath())
}

func init() {
	configureStreams(rootCmd)
	rootCmd.PersistentFlags().BoolVar(&verbose, "verbose", false, "show extra detail, such as which credential directory is in use")
}

func main() {
	ctx, stop := shutdownContext(context.Background())
	err := rootCmd.ExecuteContext(ctx)
	stop()
	if err != nil {
		var ee *exitError
		if errors.As(err, &ee) {
			os.Exit(ee.code)
		}
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

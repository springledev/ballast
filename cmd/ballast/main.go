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
	"syscall"
	"time"

	"github.com/springledev/ballast/internal/config"
	"github.com/springledev/ballast/internal/engine"
	"github.com/springledev/ballast/internal/scheduler"
	"github.com/springledev/ballast/internal/store"
	"github.com/springledev/ballast/web"
	"github.com/spf13/cobra"
)

// version is the build version, overridable at build time with
// -ldflags "-X main.version=...". Defaults to a plain dev marker.
var version = "dev"

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
	Short: "Print the ballast version",
	RunE: func(cmd *cobra.Command, args []string) error {
		cmd.Println(version)
		return nil
	},
}

var serveAddr string

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Serve the Web UI and REST API",
	RunE: func(cmd *cobra.Command, args []string) error {
		h, err := web.Handler()
		if err != nil {
			return fmt.Errorf("loading embedded web UI: %w", err)
		}
		ln, err := net.Listen("tcp", serveAddr)
		if err != nil {
			return fmt.Errorf("starting web server: %w", err)
		}
		log.Printf("ballast %s listening on http://%s", version, ln.Addr())
		return serveUntilDone(cmd.Context(), ln, h)
	},
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

// unimplementedRunner is the placeholder for the real backup
// pipeline (catalog → strategy → restic → verify), which lands with
// the REST API/CLI step (spec §14 step 11). run-due and trigger are
// wired against the scheduler.Runner interface now so that step can
// swap in the real orchestrator without touching this file.
//
// Step 11: the real runner must only ever be invoked through
// scheduler.Coordinator (RunDue/RunNow/Trigger) — including `ballast
// backup` and the REST API — because the Coordinator's per-service
// run lock is what prevents duplicate runs and lets interrupted runs
// be detected. See the step 11 note in PLAN.md.
var unimplementedRunner = scheduler.RunnerFunc(func(_ context.Context, service string) (engine.State, error) {
	return "", fmt.Errorf("service %s: job execution is not wired up yet (lands with the REST API/CLI step)", service)
})

// loadConfig resolves the effective configuration from the
// environment and ballast.yaml.
func loadConfig() (config.Config, error) {
	cfg, err := config.Load(os.Getenv, os.UserHomeDir)
	if err != nil {
		return config.Config{}, fmt.Errorf("loading config: %w", err)
	}
	return cfg, nil
}

// openCoordinator opens the SQLite store in cfg's data directory
// (creating it if needed) and returns a Coordinator whose run locks
// live in <data dir>/locks and whose sweep parallelism comes from
// max_parallel_jobs. The returned func closes the database and must be
// called when done.
func openCoordinator(cfg config.Config, runner scheduler.Runner, logw io.Writer) (*scheduler.Coordinator, func() error, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("creating data directory %s: %w", cfg.DataDir, err)
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "ballast.db"))
	if err != nil {
		return nil, nil, fmt.Errorf("opening database: %w", err)
	}
	// Step 11 must set Redact to the same secret list given to runner.Runner.Redact:
	// secrets must never reach logs or API responses (spec rule 2).
	return &scheduler.Coordinator{
		DB:          st.DB(),
		Locks:       scheduler.FileLocker{Dir: filepath.Join(cfg.DataDir, "locks")},
		Runner:      runner,
		Log:         logw,
		MaxParallel: cfg.MaxParallelJobs,
	}, st.Close, nil
}

// withCoordinator opens a Coordinator (see openCoordinator), calls fn
// with it, and closes the database afterwards. A failure to close is
// reported alongside fn's own result rather than dropped: closing is
// when SQLite checkpoints the write-ahead log.
func withCoordinator(cfg config.Config, runner scheduler.Runner, logw io.Writer, fn func(*scheduler.Coordinator) error) (err error) {
	c, closeDB, err := openCoordinator(cfg, runner, logw)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := closeDB(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("closing database: %w", cerr))
		}
	}()
	return fn(c)
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
	Short: "Run every backup job whose schedule is due (for systemd timers/cron)",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return withCoordinator(cfg, runner, cmd.ErrOrStderr(), func(c *scheduler.Coordinator) error {
			return c.RunDue(cmd.Context(), time.Now())
		})
	},
}

var (
	triggerToken     string
	triggerTokenFile string
)

// runner is the backup pipeline run-due and trigger execute through
// the Coordinator. Tests swap it for a stub.
var runner scheduler.Runner = unimplementedRunner

var triggerCmd = &cobra.Command{
	Use:   "trigger <service>",
	Short: "Run one backup job via its external trigger token (reserved for Phase-2 mobile/udev triggers)",
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
		token, err := resolveTriggerToken(args[0], triggerToken, triggerTokenFile, cfg.CredentialsDir, cmd.ErrOrStderr())
		if err != nil {
			return fmt.Errorf("triggering %s: %w", args[0], err)
		}
		return withCoordinator(cfg, runner, cmd.ErrOrStderr(), func(c *scheduler.Coordinator) error {
			return c.Trigger(cmd.Context(), args[0], token)
		})
	},
}

func init() {
	serveCmd.Flags().StringVar(&serveAddr, "addr", "127.0.0.1:8000", "listen address for the web server")
	triggerCmd.Flags().StringVar(&triggerToken, "token", "", "external trigger token for the service (visible in the process list; prefer --token-file)")
	triggerCmd.Flags().StringVar(&triggerTokenFile, "token-file", "", "path to a file (mode 0600) holding the trigger token")
	rootCmd.AddCommand(serveCmd, versionCmd, runDueCmd, triggerCmd)
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

func main() {
	ctx, stop := shutdownContext(context.Background())
	err := rootCmd.ExecuteContext(ctx)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

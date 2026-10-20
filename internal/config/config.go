// Package config loads Ballast's on-disk configuration: the data
// directory (SQLite DB and future artifacts), user catalog
// directories, and the credential directory.
//
// Credential-dir resolution follows systemd LoadCredential= semantics:
// if $CREDENTIALS_DIRECTORY is set (as it is under `systemd
// --user`/`systemd` units with LoadCredential=), that path wins. The
// resolved directory is returned *by path only* — secret contents
// never pass through this package.
package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"

	"gopkg.in/yaml.v3"
)

// Config is the resolved, effective configuration for Ballast.
type Config struct {
	// DataDir holds the SQLite database and per-run artifacts.
	DataDir string

	// CatalogDirs are directories of user YAML catalog entries,
	// loaded on top of the entries bundled in the binary.
	CatalogDirs []string

	// CredentialsDir is the directory of root-only 0600 secret
	// files (systemd LoadCredential= semantics). Under systemd it is
	// $CREDENTIALS_DIRECTORY: read-only and rebuilt on every start, so
	// Ballast only ever reads from it.
	CredentialsDir string

	// WritableCredentialsDir is where Ballast saves secrets it creates
	// itself (a generated repository password, an ntfy link typed into
	// the wizard). It is always <config dir>/credentials, never
	// $CREDENTIALS_DIRECTORY, so a secret written by an interactive
	// `sudo ballast wizard` is still there for the systemd service and
	// after every restart (spec §8: credential paths differ between
	// systemd and interactive runs). Equal to CredentialsDir outside
	// systemd.
	WritableCredentialsDir string

	// ConfigDir is the directory holding ballast.yaml and
	// user-supplied catalog/credential subdirectories.
	ConfigDir string

	// MaxParallelJobs is how many different services a scheduler
	// sweep may back up at the same time, so one long backup cannot
	// hold every other service's schedule hostage. A single service
	// never runs twice at once regardless. Always between 1 and
	// ParallelJobsLimit.
	MaxParallelJobs int

	// Listen is the host:port `ballast serve` listens on and the
	// address CLI commands look for a running daemon at. It comes from
	// the `listen` key in ballast.yaml, else DefaultListen. The
	// `--listen` flag overrides it for one `serve` run.
	Listen string
}

// DefaultListen is Listen when ballast.yaml does not set `listen`. It
// is reachable from the network, which is safe only because every API
// route requires the access key (internal/api Auth).
const DefaultListen = "0.0.0.0:8080"

// ValidateListen checks that addr looks like host:port with a port from 0
// to 65535 (0 picks any free port; the host may be empty, meaning every
// interface).
func ValidateListen(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%q is not a valid listen address; use host:port, for example 127.0.0.1:8000", addr)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 {
		return fmt.Errorf("%q has an invalid port %q; the port must be a number from 0 to 65535", addr, port)
	}
	return nil
}

// DefaultMaxParallelJobs is MaxParallelJobs when ballast.yaml does not
// set max_parallel_jobs: enough that a long backup does not delay
// everything else, without overloading a small homelab machine.
const DefaultMaxParallelJobs = 2

// ParallelJobsLimit is the largest max_parallel_jobs Ballast accepts.
// Every parallel job runs its own dump, restic upload and throwaway
// verification container, so a typo like 200 would swamp a homelab
// machine; a clear error is kinder than a machine brought to its knees.
const ParallelJobsLimit = 16

// configFile is the name of the optional YAML config file inside
// ConfigDir.
const configFile = "ballast.yaml"

// fileConfig mirrors the YAML config file keys. All fields optional.
type fileConfig struct {
	DataDir         string   `yaml:"data_dir"`
	CatalogDirs     []string `yaml:"catalog_dirs"`
	MaxParallelJobs *int     `yaml:"max_parallel_jobs"`
	Listen          string   `yaml:"listen"`
}

// Load resolves the effective configuration.
//
// Precedence (highest first):
//
//  1. Environment variables: BALLAST_DATA_DIR, BALLAST_CONFIG_DIR.
//  2. Optional YAML config file <config dir>/ballast.yaml.
//  3. XDG defaults (~/.local/state/ballast, ~/.config/ballast).
//
// getenv and homeDir are injectable for tests; production callers
// pass os.Getenv and os.UserHomeDir.
func Load(getenv func(string) string, homeDir func() (string, error)) (Config, error) {
	cfg, err := defaultDirs(getenv, homeDir)
	if err != nil {
		return Config{}, err
	}
	cfg.MaxParallelJobs = DefaultMaxParallelJobs
	cfg.Listen = DefaultListen

	if path := getenv("BALLAST_CONFIG_DIR"); path != "" {
		cfg.ConfigDir = filepath.Clean(path)
	}

	// Overlay the optional config file.
	data, err := os.ReadFile(filepath.Join(cfg.ConfigDir, configFile))
	if err == nil {
		var fc fileConfig
		if err := yaml.Unmarshal(data, &fc); err != nil {
			return Config{}, fmt.Errorf("parsing %s: %w", configFilePath(cfg.ConfigDir), err)
		}
		if fc.DataDir != "" {
			if got := getenv("BALLAST_DATA_DIR"); got == "" {
				cfg.DataDir = filepath.Clean(fc.DataDir)
			}
		}
		if len(fc.CatalogDirs) > 0 {
			cfg.CatalogDirs = cleanAll(fc.CatalogDirs)
		}
		if fc.Listen != "" {
			if err := ValidateListen(fc.Listen); err != nil {
				return Config{}, fmt.Errorf("%s: listen: %w", configFilePath(cfg.ConfigDir), err)
			}
			cfg.Listen = fc.Listen
		}
		if fc.MaxParallelJobs != nil {
			if n := *fc.MaxParallelJobs; n < 1 || n > ParallelJobsLimit {
				return Config{}, fmt.Errorf("%s: max_parallel_jobs must be between 1 and %d, got %d", configFilePath(cfg.ConfigDir), ParallelJobsLimit, n)
			}
			cfg.MaxParallelJobs = *fc.MaxParallelJobs
		}
	} else if !os.IsNotExist(err) {
		return Config{}, fmt.Errorf("reading %s: %w", configFilePath(cfg.ConfigDir), err)
	}

	if len(cfg.CatalogDirs) == 0 {
		cfg.CatalogDirs = []string{filepath.Join(cfg.ConfigDir, "catalog.d")}
	}

	cfg.CredentialsDir = credentialsDir(getenv, cfg.ConfigDir)
	cfg.WritableCredentialsDir = filepath.Join(cfg.ConfigDir, "credentials")
	return cfg, nil
}

// CredentialDirs lists every directory a credential file is looked up
// in, in priority order: CredentialsDir (what the operator provided,
// e.g. via LoadCredential=) first, then WritableCredentialsDir (what
// Ballast saved itself). Empty and duplicate entries are left out.
func (c Config) CredentialDirs() []string {
	var out []string
	for _, d := range []string{c.CredentialsDir, c.WritableCredentialsDir} {
		if d != "" && (len(out) == 0 || out[0] != d) {
			out = append(out, d)
		}
	}
	return out
}

// configFilePath returns the config file path for an error message.
// ConfigFilePath returns where ballast.yaml lives for this config.
func (c Config) ConfigFilePath() string { return configFilePath(c.ConfigDir) }

func configFilePath(dir string) string {
	return filepath.Join(dir, configFile)
}

// credentialsDir resolves the credential directory at runtime,
// preferring $CREDENTIALS_DIRECTORY (systemd LoadCredential=
// semantics). It returns the *path*; callers must never log secret
// contents.
func credentialsDir(getenv func(string) string, configDir string) string {
	if dir := getenv("CREDENTIALS_DIRECTORY"); dir != "" {
		return filepath.Clean(dir)
	}
	return filepath.Join(configDir, "credentials")
}

func cleanAll(dirs []string) []string {
	out := make([]string, 0, len(dirs))
	for _, d := range dirs {
		out = append(out, filepath.Clean(d))
	}
	return out
}

// System-wide directories used by `ballast install` and by a root
// process that has no home directory.
const (
	SystemConfigDir = "/etc/ballast"
	SystemDataDir   = "/var/lib/ballast"
)

// isRoot reports whether the process runs as root; a variable so tests
// can stand in for it.
var isRoot = func() bool { return os.Geteuid() == 0 }

// systemDirs are the system install's folders; variables so tests can
// point them at temporary directories.
var (
	systemConfigDir = SystemConfigDir
	systemDataDir   = SystemDataDir
)

// defaultDirs computes XDG-style defaults. Kept as a separate function
// so file overlays can be distinguished from env overrides in Load.
func defaultDirs(getenv func(string) string, homeDir func() (string, error)) (Config, error) {
	// A machine with `ballast install` done keeps everything in the
	// system folders. Neither folder variable is set, so the caller
	// means "this machine's Ballast": root gets it without being told
	// where it lives, anyone else is pointed at sudo instead of being
	// shown an empty per-user setup that does not match the service.
	if getenv("BALLAST_CONFIG_DIR") == "" && getenv("BALLAST_DATA_DIR") == "" {
		if fi, err := os.Stat(systemConfigDir); err == nil && fi.IsDir() {
			if !isRoot() {
				return Config{}, fmt.Errorf("Ballast is installed as a system service on this machine (%s), and its settings and backups can only be read by root; run this command with sudo, e.g. `sudo ballast ...`. To work with a separate per-user setup instead, set BALLAST_CONFIG_DIR and BALLAST_DATA_DIR", systemConfigDir)
			}
			return Config{DataDir: systemDataDir, ConfigDir: systemConfigDir}, nil
		}
	}
	// home is only needed when no environment variable names the
	// directory. A root process without $HOME (a systemd unit, a
	// minimal cron job) uses the system-wide directories instead of
	// failing; anyone else still gets a clear error.
	home, homeErr := homeDir()
	homeless := func() (string, bool, error) {
		if homeErr == nil {
			return home, false, nil
		}
		if isRoot() {
			return "", true, nil
		}
		return "", false, fmt.Errorf("resolving home directory: %w", homeErr)
	}

	dataDir := ""
	if p := getenv("BALLAST_DATA_DIR"); p != "" {
		dataDir = filepath.Clean(p)
	} else if state := getenv("XDG_STATE_HOME"); state != "" {
		dataDir = filepath.Join(state, "ballast")
	} else {
		h, system, err := homeless()
		if err != nil {
			return Config{}, err
		}
		if system {
			dataDir = SystemDataDir
		} else {
			dataDir = filepath.Join(h, ".local", "state", "ballast")
		}
	}

	configDir := ""
	if p := getenv("BALLAST_CONFIG_DIR"); p != "" {
		configDir = filepath.Clean(p)
	} else if xdg := getenv("XDG_CONFIG_HOME"); xdg != "" {
		configDir = filepath.Join(xdg, "ballast")
	} else {
		h, system, err := homeless()
		if err != nil {
			return Config{}, err
		}
		if system {
			configDir = SystemConfigDir
		} else {
			configDir = filepath.Join(h, ".config", "ballast")
		}
	}

	return Config{DataDir: dataDir, ConfigDir: configDir}, nil
}

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

// defaultDirs computes XDG-style defaults. Kept as a separate function
// so file overlays can be distinguished from env overrides in Load.
func defaultDirs(getenv func(string) string, homeDir func() (string, error)) (Config, error) {
	home, err := homeDir()
	if err != nil {
		return Config{}, fmt.Errorf("resolving home directory: %w", err)
	}

	dataDir := ""
	if p := getenv("BALLAST_DATA_DIR"); p != "" {
		dataDir = filepath.Clean(p)
	} else if state := getenv("XDG_STATE_HOME"); state != "" {
		dataDir = filepath.Join(state, "ballast")
	} else {
		dataDir = filepath.Join(home, ".local", "state", "ballast")
	}

	configDir := ""
	if p := getenv("BALLAST_CONFIG_DIR"); p != "" {
		configDir = filepath.Clean(p)
	} else if xdg := getenv("XDG_CONFIG_HOME"); xdg != "" {
		configDir = filepath.Join(xdg, "ballast")
	} else {
		configDir = filepath.Join(home, ".config", "ballast")
	}

	return Config{DataDir: dataDir, ConfigDir: configDir}, nil
}

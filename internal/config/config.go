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
	"os"
	"path/filepath"

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
	// files (systemd LoadCredential= semantics).
	CredentialsDir string

	// ConfigDir is the directory holding ballast.yaml and
	// user-supplied catalog/credential subdirectories.
	ConfigDir string
}

// configFile is the name of the optional YAML config file inside
// ConfigDir.
const configFile = "ballast.yaml"

// fileConfig mirrors the YAML config file keys. All fields optional.
type fileConfig struct {
	DataDir     string   `yaml:"data_dir"`
	CatalogDirs []string `yaml:"catalog_dirs"`
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
	} else if !os.IsNotExist(err) {
		return Config{}, fmt.Errorf("reading %s: %w", configFilePath(cfg.ConfigDir), err)
	}

	if len(cfg.CatalogDirs) == 0 {
		cfg.CatalogDirs = []string{filepath.Join(cfg.ConfigDir, "catalog.d")}
	}

	cfg.CredentialsDir = credentialsDir(getenv, cfg.ConfigDir)
	return cfg, nil
}

// configFilePath returns the config file path for an error message.
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

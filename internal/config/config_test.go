package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// getenv builds a test getenv from a map.
func getenv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func home(t *testing.T) func() (string, error) {
	t.Helper()
	return func() (string, error) { return t.TempDir(), nil }
}

func TestLoadDefaults(t *testing.T) {
	// No env, no config file: pure XDG defaults derived from home.
	h := t.TempDir()
	homeDir := func() (string, error) { return h, nil }

	cfg, err := Load(getenv(nil), homeDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DataDir != filepath.Join(h, ".local", "state", "ballast") {
		t.Errorf("DataDir = %q, want %q", cfg.DataDir, filepath.Join(h, ".local", "state", "ballast"))
	}
	if cfg.ConfigDir != filepath.Join(h, ".config", "ballast") {
		t.Errorf("ConfigDir = %q, want %q", cfg.ConfigDir, filepath.Join(h, ".config", "ballast"))
	}
}

func TestLoadDefaultsXDG(t *testing.T) {
	tmp := t.TempDir()
	env := map[string]string{
		"XDG_STATE_HOME":  filepath.Join(tmp, "state"),
		"XDG_CONFIG_HOME": filepath.Join(tmp, "config"),
	}
	cfg, err := Load(getenv(env), home(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	wantData := filepath.Join(tmp, "state", "ballast")
	wantCfg := filepath.Join(tmp, "config", "ballast")
	if cfg.DataDir != wantData {
		t.Errorf("DataDir = %q, want %q", cfg.DataDir, wantData)
	}
	if cfg.ConfigDir != wantCfg {
		t.Errorf("ConfigDir = %q, want %q", cfg.ConfigDir, wantCfg)
	}
	wantCreds := filepath.Join(wantCfg, "credentials")
	if cfg.CredentialsDir != wantCreds {
		t.Errorf("CredentialsDir = %q, want %q", cfg.CredentialsDir, wantCreds)
	}
}

func TestLoadEnvBeatsDefaults(t *testing.T) {
	tmp := t.TempDir()
	env := map[string]string{
		"BALLAST_DATA_DIR":   filepath.Join(tmp, "data"),
		"BALLAST_CONFIG_DIR": filepath.Join(tmp, "conf"),
	}
	cfg, err := Load(getenv(env), home(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DataDir != filepath.Join(tmp, "data") {
		t.Errorf("DataDir = %q", cfg.DataDir)
	}
	if cfg.ConfigDir != filepath.Join(tmp, "conf") {
		t.Errorf("ConfigDir = %q", cfg.ConfigDir)
	}
}

func TestLoadConfigFile(t *testing.T) {
	tmp := t.TempDir()
	confDir := filepath.Join(tmp, "conf")
	if err := os.MkdirAll(confDir, 0o700); err != nil {
		t.Fatal(err)
	}
	yaml := "data_dir: " + filepath.Join(tmp, "elsewhere") + "\ncatalog_dirs:\n  - /a/b\n"
	if err := os.WriteFile(filepath.Join(confDir, configFile), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"BALLAST_CONFIG_DIR": confDir}
	cfg, err := Load(getenv(env), home(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DataDir != filepath.Join(tmp, "elsewhere") {
		t.Errorf("DataDir = %q, want config-file value", cfg.DataDir)
	}
	if !reflect.DeepEqual(cfg.CatalogDirs, []string{"/a/b"}) {
		t.Errorf("CatalogDirs = %v", cfg.CatalogDirs)
	}
}

func TestLoadCatalogDirDefault(t *testing.T) {
	tmp := t.TempDir()
	env := map[string]string{"BALLAST_CONFIG_DIR": filepath.Join(tmp, "conf")}
	cfg, err := Load(getenv(env), home(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := filepath.Join(tmp, "conf", "catalog.d")
	if len(cfg.CatalogDirs) != 1 || cfg.CatalogDirs[0] != want {
		t.Errorf("CatalogDirs = %v, want [%s]", cfg.CatalogDirs, want)
	}
}

func TestLoadCredentialsDirectoryEnvWins(t *testing.T) {
	// systemd LoadCredential= semantics: $CREDENTIALS_DIRECTORY
	// always wins over the config-dir default.
	env := map[string]string{"CREDENTIALS_DIRECTORY": "/run/credentials/ballast"}
	cfg, err := Load(getenv(env), home(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.CredentialsDir != "/run/credentials/ballast" {
		t.Errorf("CredentialsDir = %q, want env value", cfg.CredentialsDir)
	}
}

// TestLoadWritableCredentialsDir: secrets Ballast creates itself must
// never go to $CREDENTIALS_DIRECTORY (read-only, rebuilt on every
// systemd start), and must land in the same place whether Ballast runs
// under systemd or interactively (spec §8).
func TestLoadWritableCredentialsDir(t *testing.T) {
	tests := []struct {
		name     string
		credsEnv string
		wantDirs func(conf string) []string
	}{
		{
			name:     "interactive",
			wantDirs: func(conf string) []string { return []string{filepath.Join(conf, "credentials")} },
		},
		{
			name:     "systemd",
			credsEnv: "/run/credentials/ballast.service",
			wantDirs: func(conf string) []string {
				return []string{"/run/credentials/ballast.service", filepath.Join(conf, "credentials")}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conf := filepath.Join(t.TempDir(), "conf")
			env := map[string]string{"BALLAST_CONFIG_DIR": conf, "CREDENTIALS_DIRECTORY": tt.credsEnv}
			cfg, err := Load(getenv(env), home(t))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if want := filepath.Join(conf, "credentials"); cfg.WritableCredentialsDir != want {
				t.Errorf("WritableCredentialsDir = %q, want %q", cfg.WritableCredentialsDir, want)
			}
			got, want := cfg.CredentialDirs(), tt.wantDirs(conf)
			if strings.Join(got, "|") != strings.Join(want, "|") {
				t.Errorf("CredentialDirs() = %v, want %v", got, want)
			}
		})
	}
}

func TestLoadBadConfigFile(t *testing.T) {
	tmp := t.TempDir()
	confDir := filepath.Join(tmp, "conf")
	if err := os.MkdirAll(confDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(confDir, configFile), []byte("\t\t::::not yaml"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(getenv(map[string]string{"BALLAST_CONFIG_DIR": confDir}), home(t))
	if err == nil {
		t.Fatal("expected error for malformed config file")
	}
}

func TestLoadHomeDirError(t *testing.T) {
	_, err := Load(getenv(nil), func() (string, error) { return "", errors.New("boom") })
	if err == nil {
		t.Fatal("expected error when home dir cannot be resolved")
	}
}

func TestLoadMaxParallelJobs(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string // empty: no config file
		want    int
		wantErr string
	}{
		{name: "default without a config file", want: DefaultMaxParallelJobs},
		{name: "default when unset", yaml: "catalog_dirs: [/a]\n", want: DefaultMaxParallelJobs},
		{name: "explicit one runs jobs one at a time", yaml: "max_parallel_jobs: 1\n", want: 1},
		{name: "explicit four", yaml: "max_parallel_jobs: 4\n", want: 4},
		{name: "explicit maximum", yaml: "max_parallel_jobs: 16\n", want: ParallelJobsLimit},
		{name: "zero rejected", yaml: "max_parallel_jobs: 0\n", wantErr: "must be between 1 and 16"},
		{name: "negative rejected", yaml: "max_parallel_jobs: -2\n", wantErr: "must be between 1 and 16"},
		{name: "above maximum rejected", yaml: "max_parallel_jobs: 17\n", wantErr: "must be between 1 and 16, got 17"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			confDir := filepath.Join(t.TempDir(), "conf")
			if err := os.MkdirAll(confDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if tt.yaml != "" {
				if err := os.WriteFile(filepath.Join(confDir, configFile), []byte(tt.yaml), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cfg, err := Load(getenv(map[string]string{"BALLAST_CONFIG_DIR": confDir}), home(t))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Load error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.MaxParallelJobs != tt.want {
				t.Errorf("MaxParallelJobs = %d, want %d", cfg.MaxParallelJobs, tt.want)
			}
		})
	}
}

func TestLoadListen(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		want    string
		wantErr string
	}{
		{name: "default", yaml: "", want: DefaultListen},
		{name: "from file", yaml: "listen: 0.0.0.0:9090\n", want: "0.0.0.0:9090"},
		{name: "all interfaces shorthand", yaml: "listen: \":8080\"\n", want: ":8080"},
		{name: "missing port", yaml: "listen: localhost\n", wantErr: "not a valid listen address"},
		{name: "bad port", yaml: "listen: 127.0.0.1:99999\n", wantErr: "invalid port"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.yaml != "" {
				if err := os.WriteFile(filepath.Join(dir, "ballast.yaml"), []byte(tt.yaml), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cfg, err := Load(getenv(map[string]string{"BALLAST_CONFIG_DIR": dir}), home(t))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Load error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.Listen != tt.want {
				t.Errorf("Listen = %q, want %q", cfg.Listen, tt.want)
			}
		})
	}
}

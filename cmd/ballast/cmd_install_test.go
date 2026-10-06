package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type installEnv struct {
	plan  installPlan
	calls *[]string
	home  string
}

func newInstallEnv(t *testing.T) installEnv {
	t.Helper()
	root := t.TempDir()
	var calls []string
	home := filepath.Join(root, "home")
	return installEnv{
		calls: &calls,
		home:  home,
		plan: installPlan{
			ConfigDir: filepath.Join(root, "etc", "ballast"),
			DataDir:   filepath.Join(root, "var", "ballast"),
			UnitPath:  filepath.Join(root, "systemd", "ballast.service"),
			Exe:       "/opt/ballast",
			Sources:   []installSource{{ConfigDir: filepath.Join(home, ".config", "ballast"), DataDir: filepath.Join(home, ".local", "state", "ballast")}},
			Systemctl: func(_ context.Context, args ...string) error {
				calls = append(calls, strings.Join(args, " "))
				return nil
			},
			Out: &bytes.Buffer{},
		},
	}
}

func writeFile(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func TestRunInstallFresh(t *testing.T) {
	e := newInstallEnv(t)
	res, err := runInstall(context.Background(), e.plan)
	if err != nil {
		t.Fatalf("runInstall: %v", err)
	}
	if res.Token == "" {
		t.Error("a fresh install must create and return an access key")
	}
	unit, err := os.ReadFile(e.plan.UnitPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(unit), "ExecStart=/opt/ballast serve") {
		t.Errorf("unit does not run the installed binary:\n%s", unit)
	}
	if got := strings.Join(*e.calls, ";"); got != "daemon-reload;enable --now ballast.service" {
		t.Errorf("systemctl calls = %q", got)
	}
	info, err := os.Stat(res.TokenPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("token file: %v %v, want mode 0600", err, info)
	}
	if res.Listen == "" {
		t.Error("Listen is empty")
	}
}

func TestRunInstallIsIdempotent(t *testing.T) {
	e := newInstallEnv(t)
	first, err := runInstall(context.Background(), e.plan)
	if err != nil {
		t.Fatal(err)
	}
	second, err := runInstall(context.Background(), e.plan)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if second.Token != "" {
		t.Error("second run must not create or reveal another key")
	}
	tok, _ := os.ReadFile(first.TokenPath)
	if strings.TrimSpace(string(tok)) != first.Token {
		t.Error("the access key changed on re-run")
	}
}

func TestRunInstallMigratesWithoutLoss(t *testing.T) {
	e := newInstallEnv(t)
	src := e.plan.Sources[0]
	writeFile(t, filepath.Join(src.ConfigDir, "ballast.yaml"), "listen: 127.0.0.1:9000\n", 0o644)
	writeFile(t, filepath.Join(src.ConfigDir, "credentials", "ballast-restic-password.x"), "pw", 0o600)
	writeFile(t, filepath.Join(src.ConfigDir, "credentials", apiTokenCredential), "old-key\n", 0o600)
	writeFile(t, filepath.Join(src.DataDir, "ballast.db"), "database-bytes", 0o600)
	before, err := treeManifest(src.DataDir)
	if err != nil {
		t.Fatal(err)
	}

	res, err := runInstall(context.Background(), e.plan)
	if err != nil {
		t.Fatalf("runInstall: %v", err)
	}
	after, err := treeManifest(e.plan.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(before, "\n") != strings.Join(after, "\n") {
		t.Errorf("data folder changed in the move:\nbefore %v\nafter  %v", before, after)
	}
	if b, _ := os.ReadFile(filepath.Join(e.plan.ConfigDir, "credentials", "ballast-restic-password.x")); string(b) != "pw" {
		t.Error("credential file was lost")
	}
	if res.Token != "" {
		t.Error("an existing access key must be kept, not replaced")
	}
	if res.Listen != "127.0.0.1:9000" {
		t.Errorf("Listen = %q, want the migrated config's value", res.Listen)
	}
	for _, d := range []string{src.ConfigDir, src.DataDir} {
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Errorf("%s should have been moved away", d)
		}
	}
	unit, _ := os.ReadFile(e.plan.UnitPath)
	if !strings.Contains(string(unit), "LoadCredential=ballast-api-token:"+filepath.Join(e.plan.ConfigDir, "credentials", apiTokenCredential)) {
		t.Errorf("unit should load the migrated token:\n%s", unit)
	}
	if strings.Contains(string(unit), "ballast-ntfy-topic") {
		t.Error("unit lists a credential file that does not exist, systemd would refuse to start")
	}
}

func TestRunInstallRefusals(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, e installEnv)
		want  string
	}{
		{"populated target and old install", func(t *testing.T, e installEnv) {
			writeFile(t, filepath.Join(e.plan.DataDir, "ballast.db"), "new", 0o600)
			writeFile(t, filepath.Join(e.plan.Sources[0].DataDir, "ballast.db"), "old", 0o600)
		}, "will not merge"},
		{"different unit", func(t *testing.T, e installEnv) {
			writeFile(t, e.plan.UnitPath, "[Service]\nExecStart=/somewhere/else\n", 0o644)
		}, "nothing was overwritten"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newInstallEnv(t)
			tt.setup(t, e)
			_, err := runInstall(context.Background(), e.plan)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.want)
			}
			if len(*e.calls) != 0 {
				t.Errorf("systemctl must not run after a refusal, got %v", *e.calls)
			}
		})
	}
}

func TestRunInstallRefusesTwoUserInstalls(t *testing.T) {
	e := newInstallEnv(t)
	other := filepath.Join(e.home, "other")
	e.plan.Sources = append(e.plan.Sources, installSource{ConfigDir: filepath.Join(other, "c"), DataDir: filepath.Join(other, "d")})
	writeFile(t, filepath.Join(other, "c", "ballast.yaml"), "x", 0o644)
	writeFile(t, filepath.Join(e.plan.Sources[0].ConfigDir, "ballast.yaml"), "y", 0o644)
	if _, err := runInstall(context.Background(), e.plan); err == nil || !strings.Contains(err.Error(), "more than one") {
		t.Fatalf("err = %v, want a refusal naming more than one setup", err)
	}
	if len(*e.calls) != 0 {
		t.Errorf("systemctl ran after a refusal: %v", *e.calls)
	}
}

func TestUIURL(t *testing.T) {
	tests := []struct{ listen, want string }{
		{"0.0.0.0:8080", "http://nas:8080"},
		{"127.0.0.1:8000", "http://127.0.0.1:8000"},
		{":9000", "http://nas:9000"},
	}
	for _, tt := range tests {
		if got := uiURL(tt.listen, "nas"); got != tt.want {
			t.Errorf("uiURL(%q) = %q, want %q", tt.listen, got, tt.want)
		}
	}
}

func TestStartedFromTerminal(t *testing.T) {
	f, err := os.Open(os.DevNull) // a character device, like a terminal
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	file := filepath.Join(t.TempDir(), "in")
	writeFile(t, file, "", 0o600)
	reg, _ := os.Open(file)
	defer reg.Close()

	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	tests := []struct {
		name  string
		env   map[string]string
		stdin *os.File
		want  bool
	}{
		{"terminal", nil, f, true},
		{"systemd unit", map[string]string{"INVOCATION_ID": "abc"}, f, false},
		{"piped input", nil, reg, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := startedFromTerminal(env(tt.env), tt.stdin); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

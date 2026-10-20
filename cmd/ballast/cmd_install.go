package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/springledev/ballast/deploy"
	"github.com/springledev/ballast/internal/api"
	"github.com/springledev/ballast/internal/config"
	"github.com/spf13/cobra"
)

// installPlan holds everything `ballast install` touches, so tests can
// point it at temporary directories and a fake systemctl.
type installPlan struct {
	// ConfigDir and DataDir are the system-wide homes (/etc/ballast,
	// /var/lib/ballast).
	ConfigDir, DataDir string
	// UnitPath is where the service file is written.
	UnitPath string
	// Exe is the ballast binary the unit will run.
	Exe string
	// Sources are existing per-user installs to move in: pairs of
	// config and state folders, as found for the invoking user and root.
	Sources []installSource
	// Systemctl runs `systemctl <args>`.
	Systemctl func(ctx context.Context, args ...string) error
	// Out receives progress messages.
	Out io.Writer
}

// installSource is one existing per-user install.
type installSource struct{ ConfigDir, DataDir string }

// installResult is what `ballast install` reports at the end.
type installResult struct {
	// Token is the new access key, "" when one already existed.
	Token string
	// TokenPath is where the access key is stored.
	TokenPath string
	// Listen is the address the service listens on.
	Listen string
}

const installUnitName = "ballast.service"

// renderUnit returns the shipped unit with ExecStart pointing at exe and
// every LoadCredential= line dropped whose file does not exist yet
// (systemd refuses to start a unit whose credential file is missing).
func renderUnit(unit, exe, configDir string) string {
	var out []string
	for _, line := range strings.Split(unit, "\n") {
		switch {
		case strings.HasPrefix(line, "ExecStart="):
			line = "ExecStart=" + exe + " serve"
		case strings.HasPrefix(line, "LoadCredential="):
			// LoadCredential=name:/etc/ballast/credentials/name
			spec := strings.TrimPrefix(line, "LoadCredential=")
			name, _, ok := strings.Cut(spec, ":")
			if !ok {
				continue
			}
			real := filepath.Join(configDir, "credentials", filepath.Base(name))
			if _, err := os.Stat(real); err != nil {
				continue
			}
			line = "LoadCredential=" + name + ":" + real
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// stripCredentialLines drops LoadCredential= lines so two units can be
// compared without caring which credential files exist.
func stripCredentialLines(unit string) string {
	var out []string
	for _, l := range strings.Split(unit, "\n") {
		if !strings.HasPrefix(l, "LoadCredential=") {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// dirHasEntries reports whether dir exists and is not empty.
func dirHasEntries(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return len(entries) > 0, nil
}

// copyTree copies src into dst preserving file modes and symlinks.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(target, info.Mode().Perm())
		case info.Mode()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case info.Mode().IsRegular():
			in, err := os.Open(path)
			if err != nil {
				return err
			}
			defer in.Close()
			out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, in); err != nil {
				out.Close()
				return err
			}
			return out.Close()
		}
		return nil
	})
}

// treeManifest lists every file and folder under dir with its size, so
// a copy can be compared with its source before the source is removed.
func treeManifest(dir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		info, err := d.Info()
		if err != nil {
			return err
		}
		size := int64(0)
		if info.Mode().IsRegular() {
			size = info.Size()
		}
		out = append(out, fmt.Sprintf("%s|%d|%v", rel, size, info.Mode().Perm()))
		return nil
	})
	sort.Strings(out)
	return out, err
}

// moveTree moves src to dst (dst must be missing or empty) by copying,
// comparing and only then deleting the source, so a failure never loses
// data.
func moveTree(src, dst string) error {
	if err := copyTree(src, dst); err != nil {
		return fmt.Errorf("copying %s to %s: %w", src, dst, err)
	}
	a, err := treeManifest(src)
	if err != nil {
		return err
	}
	b, err := treeManifest(dst)
	if err != nil {
		return err
	}
	if strings.Join(a, "\n") != strings.Join(b, "\n") {
		return fmt.Errorf("the copy of %s in %s does not match the original, so nothing was removed", src, dst)
	}
	return os.RemoveAll(src)
}

// pickSources chooses the single existing install to move in. Two
// different installs (for example the invoking user's and root's) are
// ambiguous, so the operator must decide.
func pickSources(p installPlan) (*installSource, error) {
	var found []installSource
	for _, s := range p.Sources {
		cfg, err := dirHasEntries(s.ConfigDir)
		if err != nil {
			return nil, fmt.Errorf("looking at %s: %w", s.ConfigDir, err)
		}
		data, err := dirHasEntries(s.DataDir)
		if err != nil {
			return nil, fmt.Errorf("looking at %s: %w", s.DataDir, err)
		}
		if cfg || data {
			found = append(found, s)
		}
	}
	switch len(found) {
	case 0:
		return nil, nil
	case 1:
		return &found[0], nil
	}
	var names []string
	for _, s := range found {
		names = append(names, s.ConfigDir)
	}
	return nil, fmt.Errorf("found more than one existing Ballast setup (%s); nothing was changed. Move the ones you do not want aside and run `ballast install` again", strings.Join(names, ", "))
}

// runInstall performs the install. It is idempotent and refuses to
// overwrite a different existing install: a populated system folder
// next to an old per-user install, or a different unit file.
func runInstall(ctx context.Context, p installPlan) (installResult, error) {
	var res installResult
	say := func(format string, a ...any) { fmt.Fprintf(p.Out, format+"\n", a...) }

	// Decide everything that can refuse before changing anything.
	src, err := pickSources(p)
	if err != nil {
		return res, err
	}
	if src != nil {
		for _, pair := range [][2]string{{src.ConfigDir, p.ConfigDir}, {src.DataDir, p.DataDir}} {
			if pair[0] == pair[1] {
				continue
			}
			used, err := dirHasEntries(pair[0])
			if err != nil {
				return res, fmt.Errorf("looking at %s: %w", pair[0], err)
			}
			if !used {
				continue
			}
			taken, err := dirHasEntries(pair[1])
			if err != nil {
				return res, fmt.Errorf("looking at %s: %w", pair[1], err)
			}
			if taken {
				return res, fmt.Errorf("%s already holds a Ballast install and so does %s; nothing was changed. Ballast will not merge two installs: keep one and move the other aside", pair[1], pair[0])
			}
		}
	}
	existingUnit, err := os.ReadFile(p.UnitPath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return res, fmt.Errorf("reading %s: %w", p.UnitPath, err)
	}

	for _, d := range []string{p.ConfigDir, p.DataDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return res, fmt.Errorf("creating %s: %w", d, err)
		}
	}
	if src != nil {
		for _, pair := range [][2]string{{src.ConfigDir, p.ConfigDir}, {src.DataDir, p.DataDir}} {
			if pair[0] == pair[1] {
				continue
			}
			used, _ := dirHasEntries(pair[0])
			if !used {
				continue
			}
			if err := moveTree(pair[0], pair[1]); err != nil {
				return res, fmt.Errorf("moving your existing setup: %w", err)
			}
			say("Moved %s to %s", pair[0], pair[1])
		}
	}

	// The access key is created here so it can be shown once, now.
	res.TokenPath = filepath.Join(p.ConfigDir, "credentials", apiTokenCredential)
	if _, err := os.Stat(res.TokenPath); errors.Is(err, fs.ErrNotExist) {
		tok, err := api.GenerateToken()
		if err != nil {
			return res, fmt.Errorf("generating an access key: %w", err)
		}
		if err := writeAPIToken(res.TokenPath, tok); err != nil {
			return res, err
		}
		res.Token = tok
	}

	unit := renderUnit(deploy.UnitFile, p.Exe, p.ConfigDir)
	switch {
	case len(existingUnit) == 0:
	case stripCredentialLines(string(existingUnit)) == stripCredentialLines(unit):
		// Same install; credential lines are the operator's to add.
		unit = string(existingUnit)
	default:
		return res, fmt.Errorf("%s already exists and is different from the one Ballast would install; nothing was overwritten. Remove or rename it if you want `ballast install` to replace it", p.UnitPath)
	}
	if string(existingUnit) != unit {
		if err := os.MkdirAll(filepath.Dir(p.UnitPath), 0o755); err != nil {
			return res, fmt.Errorf("creating %s: %w", filepath.Dir(p.UnitPath), err)
		}
		if err := os.WriteFile(p.UnitPath, []byte(unit), 0o644); err != nil {
			return res, fmt.Errorf("writing %s: %w", p.UnitPath, err)
		}
		say("Installed %s", p.UnitPath)
	}
	if err := p.Systemctl(ctx, "daemon-reload"); err != nil {
		return res, err
	}
	if err := p.Systemctl(ctx, "enable", "--now", installUnitName); err != nil {
		return res, err
	}

	env := func(k string) string {
		if k == "BALLAST_CONFIG_DIR" {
			return p.ConfigDir
		}
		if k == "BALLAST_DATA_DIR" {
			return p.DataDir
		}
		return ""
	}
	cfg, err := config.Load(env, func() (string, error) { return "", errors.New("no home") })
	if err != nil {
		return res, fmt.Errorf("reading the installed config: %w", err)
	}
	res.Listen = cfg.Listen
	return res, nil
}

// uiURL turns a listen address into a URL a person can open.
func uiURL(listen, hostname string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "http://" + listen
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = hostname
	}
	return "http://" + net.JoinHostPort(host, port)
}

// userInstallSources finds per-user installs for the invoking user
// (when run through sudo) and for the current user.
func userInstallSources() []installSource {
	var homes []string
	if name := os.Getenv("SUDO_USER"); name != "" {
		if u, err := lookupHome(name); err == nil {
			homes = append(homes, u)
		}
	}
	if h, err := os.UserHomeDir(); err == nil {
		homes = append(homes, h)
	}
	seen := map[string]bool{}
	var out []installSource
	for _, h := range homes {
		if seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, installSource{
			ConfigDir: filepath.Join(h, ".config", "ballast"),
			DataDir:   filepath.Join(h, ".local", "state", "ballast"),
		})
	}
	return out
}

// lookupHome returns a user's home directory from /etc/passwd.
func lookupHome(name string) (string, error) {
	f, err := os.Open("/etc/passwd")
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		parts := strings.Split(sc.Text(), ":")
		if len(parts) >= 6 && parts[0] == name {
			return parts[5], nil
		}
	}
	return "", fmt.Errorf("no user %q", name)
}

// installCmd is `ballast install`: run Ballast as a background service.
var installCmd = &cobra.Command{
	Use:   "install",
	Short: "Run Ballast as a background service that starts at boot (needs root)",
	Long: "Sets Ballast up as a systemd service: creates /etc/ballast and /var/lib/ballast, moves any existing\n" +
		"Ballast setup from your home folder into them, installs and starts the service, and prints the web\n" +
		"address and access key. It is safe to run again, and it never overwrites a different install.",
	Args: noArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if os.Geteuid() != 0 {
			return errors.New("ballast install needs root: run it with sudo")
		}
		exe, err := os.Executable()
		if err != nil {
			return fmt.Errorf("finding the ballast binary: %w", err)
		}
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		out := cmd.ErrOrStderr()
		res, err := runInstall(cmd.Context(), installPlan{
			ConfigDir: config.SystemConfigDir,
			DataDir:   config.SystemDataDir,
			UnitPath:  "/etc/systemd/system/" + installUnitName,
			Exe:       exe,
			Sources:   userInstallSources(),
			Systemctl: systemctl,
			Out:       out,
		})
		if err != nil {
			return err
		}
		host, _ := os.Hostname()
		cmd.Printf("Ballast is running as a service. Open %s in your browser.\n", uiURL(res.Listen, host))
		if res.Token != "" {
			cmd.Printf("Your access key (shown once, save it now):\n\n    %s\n\n", res.Token)
		} else {
			cmd.Printf("Your existing access key is kept in %s. `ballast token rotate` makes a new one.\n", res.TokenPath)
		}
		return nil
	},
}

// systemctl runs `systemctl <args>` and wraps a failure with its output.
func systemctl(ctx context.Context, args ...string) error {
	c := exec.CommandContext(ctx, "systemctl", args...)
	var buf bytes.Buffer
	c.Stdout, c.Stderr = &buf, &buf
	if err := c.Run(); err != nil {
		return fmt.Errorf("systemctl %s failed: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(buf.String()))
	}
	return nil
}

func init() {
	rootCmd.AddCommand(installCmd)
}

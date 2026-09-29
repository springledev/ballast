package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/springledev/ballast/internal/api"
	"github.com/springledev/ballast/internal/catalog"
	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/notify"
	"github.com/spf13/cobra"
	execrunner "github.com/springledev/keel/runner"
)

// wizardCmd is `ballast wizard` (spec §5.3.6, §9): a guided first run
// that gets a brand-new install to one *verified* backup in under 15
// minutes, with no manual docs needed (spec §15 acceptance checklist).
// It only ever calls the same Ops methods the REST API and every
// other command use (spec §10: "GUI is a convenience layer — every
// GUI action must be doable via CLI", which cuts both ways: the
// wizard is not allowed any privileged path either).
var wizardCmd = &cobra.Command{
	Use:   "wizard",
	Short: "Set up your first backup, step by step, and prove it can be restored",
	Long: `The wizard walks you through everything needed for your first backup:
what to back up, where to keep the backups, how to be told when something
goes wrong, and how often to run. It then runs the first backup and checks
it can really be restored — so you finish knowing your setup works, not just
hoping it does.

Every question has a sensible default: press Enter to accept it.`,
	RunE: runWizard,
}

// wizardSteps is how many numbered sections the wizard walks through.
const wizardSteps = 4

func wizardHeading(out io.Writer, step int, title string) {
	fmt.Fprintf(out, "\nStep %d of %d: %s\n", step, wizardSteps, title)
}

func runWizard(cmd *cobra.Command, args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	in := bufio.NewReader(cmd.InOrStdin())
	out := cmd.OutOrStdout()
	ctx := cmd.Context()

	return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
		fmt.Fprintln(out, "Welcome to Ballast! A few questions, then your first backup — checked to make sure it can be restored.")
		fmt.Fprintln(out, "Press Enter to accept the suggestion shown in [brackets].")

		if err := checkWizardPrerequisites(ctx, a, out); err != nil {
			return err
		}

		wizardHeading(out, 1, "What do you want to back up?")
		entry, err := chooseCatalogEntry(ctx, a, in, out)
		if err != nil {
			return err
		}
		serviceName, err := askLine(out, in, "Give it a short name (lowercase letters, numbers and dashes)", entry.Service)
		if err != nil {
			return err
		}
		if err := inventory.ValidateName(serviceName); err != nil {
			return err
		}
		params := make(map[string]string, len(entry.Parameters))
		for _, p := range entry.Parameters {
			prompt := p.Prompt
			if prompt == "" {
				prompt = p.Name
			}
			v, err := askLine(out, in, prompt, p.Default)
			if err != nil {
				return err
			}
			if v == "" && p.Required {
				return fmt.Errorf("%q is required — run the wizard again and fill it in", prompt)
			}
			params[p.Name] = v
		}

		wizardHeading(out, 2, "Where should the backups be kept?")
		destName, err := chooseDestination(ctx, a, in, out)
		if err != nil {
			return err
		}

		if err := a.ops.PutService(ctx, inventory.Service{
			Name: serviceName, CatalogEntry: entry.Service, Destination: destName, Params: params,
		}); err != nil {
			return fmt.Errorf("saving %s: %w", serviceName, err)
		}
		fmt.Fprintf(out, "  Saved %s.\n", serviceName)

		wizardHeading(out, 3, "How should Ballast tell you about problems? (optional)")
		if err := configureNotifications(ctx, a, in, out, serviceName); err != nil {
			return err
		}

		wizardHeading(out, 4, "How often should it back up?")
		if err := configureSchedule(ctx, a, in, out, serviceName); err != nil {
			return err
		}

		fmt.Fprintln(out, "\nRunning your first backup now, then restoring a sample of it to prove it works. This can take a few minutes...")
		runErr := a.ops.Backup(ctx, serviceName)
		reportErr := reportWizardResult(ctx, a, out, serviceName)
		if runErr != nil {
			return fmt.Errorf("the first backup did not finish successfully: %w", runErr)
		}
		return reportErr
	})
}

// checkWizardPrerequisites fails fast, in plain English, when restic
// itself is not usable — every backup strategy needs it, so there is
// no point walking through the questions only to hit a raw exec error
// at the very end. Docker is only a note: backing up a plain folder
// needs no Docker at all, so a missing Docker must never block the
// wizard outright.
func checkWizardPrerequisites(ctx context.Context, a *app, out io.Writer) error {
	res := a.ops.CheckPreflight(ctx)
	if res.ResticError != "" {
		return fmt.Errorf("ballast uses restic to store backups, and it can't be used yet: %s", res.ResticError)
	}
	if res.ResticWarning != "" {
		fmt.Fprintf(out, "\nWarning: %s\n", res.ResticWarning)
	}
	if !res.DockerInstalled {
		fmt.Fprintln(out, "\nNote: Docker isn't available, so apps running in Docker (databases, Immich, ...) can't be backed up yet. Plain folders still work.")
	}
	return nil
}

// chooseCatalogEntry lists the loaded catalog, best-effort-detects
// running Docker services to suggest one (spec §5.3.6), and prompts
// for a choice by catalog entry name. Detection failures (commonly:
// no Docker installed) are skipped quietly — checkWizardPrerequisites
// has already said Docker is unavailable, and a service can always be
// chosen by hand.
func chooseCatalogEntry(ctx context.Context, a *app, in *bufio.Reader, out io.Writer) (*catalog.Entry, error) {
	suggested := ""
	if res, err := a.ops.DetectServices(ctx, ""); err == nil && len(res.Suggestions) > 0 {
		suggested = res.Suggestions[0].Service
		names := make([]string, len(res.Suggestions[0].Containers))
		for i, c := range res.Suggestions[0].Containers {
			names[i] = c.Name
		}
		fmt.Fprintf(out, "  Found a running app that looks like %s (container: %s).\n", suggested, strings.Join(names, ", "))
	}

	entries := a.ops.ListCatalog()
	sort.Slice(entries, func(i, j int) bool { return entries[i].Service < entries[j].Service })
	for _, e := range entries {
		note := ""
		if e.Service == suggested {
			note = "  <- found on this machine"
		}
		fmt.Fprintf(out, "  %-12s %s%s\n", e.Service, e.Description, note)
	}

	def := suggested
	if def == "" {
		def = "folder"
	}
	for {
		choice, err := askLine(out, in, "Type one of the names above", def)
		if err != nil {
			return nil, err
		}
		if full, ok := a.ops.Catalog.Get(choice); ok {
			return full, nil
		}
		fmt.Fprintf(out, "  %q isn't in the list. Type one of the names on the left.\n", choice)
	}
}

// chooseDestination offers existing destinations, or walks through
// creating a brand-new local-folder one — generating and storing the
// repository password itself so a first-time user is never asked to
// invent or paste a secret (spec §5.3.6: a local folder destination
// must be reachable within the wizard's 15-minute budget).
func chooseDestination(ctx context.Context, a *app, in *bufio.Reader, out io.Writer) (string, error) {
	existing, err := a.ops.ListDestinations(ctx)
	if err != nil {
		return "", err
	}
	if len(existing) > 0 {
		names := make([]string, len(existing))
		for i, d := range existing {
			names[i] = d.Name
			fmt.Fprintf(out, "  %-12s %s\n", d.Name, d.Location)
		}
		choice, err := askLine(out, in, "Use one of these (type its name), or type a new name to set up another", names[0])
		if err != nil {
			return "", err
		}
		for _, n := range names {
			if n == choice {
				return choice, nil
			}
		}
		return createLocalDestination(ctx, a, in, out, choice)
	}
	name, err := askLine(out, in, "Name for this backup location", "local")
	if err != nil {
		return "", err
	}
	return createLocalDestination(ctx, a, in, out, name)
}

func createLocalDestination(ctx context.Context, a *app, in *bufio.Reader, out io.Writer, name string) (string, error) {
	if err := inventory.ValidateName(name); err != nil {
		return "", err
	}
	defaultDir := filepath.Join(a.cfg.DataDir, "backups", name)
	fmt.Fprintln(out, "  Tip: a folder on a different disk (or a mounted NAS share) protects you if this disk fails.")
	dir, err := askLine(out, in, "Folder to keep the backups in", defaultDir)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("the backup folder must be a full path starting with / (got %q)", dir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating backup folder %s: %w", dir, err)
	}

	// PutDestination itself generates and saves a repository password
	// when this is a new local-disk destination with no credential
	// file yet (internal/api.Ops.ensurePassword) — shared with the
	// GUI wizard and the plain "add destination" form, so a beginner is
	// never asked to invent or paste a secret through any of them.
	credName := "ballast-restic-password." + name
	// A password the operator already provided (in either credential
	// directory) is used as is; a new one is saved in Ballast's own,
	// persistent directory — never $CREDENTIALS_DIRECTORY, which
	// systemd rebuilds on every start (spec §8).
	credPath := filepath.Join(execrunner.FindCredentialDir(credName, a.cfg.CredentialDirs()...), credName)
	_, statErr := os.Stat(credPath)
	hadPassword := statErr == nil
	if !hadPassword {
		credPath = filepath.Join(a.cfg.WritableCredentialsDir, credName)
	}

	dest := inventory.Destination{Name: name, Location: dir, PasswordCredential: credName, KeepDaily: 7, KeepWeekly: 4, KeepMonthly: 6}
	if err := a.ops.PutDestination(ctx, dest, true); err != nil {
		return "", fmt.Errorf("setting up backup location %s: %w", name, err)
	}
	fmt.Fprintf(out, "  Backups will be kept in %s (encrypted).\n", dir)
	if !hadPassword {
		fmt.Fprintf(out, "  The encryption password was created for you and saved in:\n    %s\n", credPath)
		fmt.Fprintln(out, "  IMPORTANT: keep a copy of that file somewhere safe, away from this machine.")
		fmt.Fprintln(out, "  Without it, nobody — including you — can restore these backups.")
	}
	return name, nil
}

// configureNotifications optionally wires up an ntfy channel for
// service and turns on its success heartbeat, so the wizard's own
// first verified backup actually produces the "success notification"
// spec §5.3.6 describes end-to-end — not just a terminal message.
// Declining leaves notifications unconfigured; nothing about the
// backup itself depends on this.
//
// The credential file's content must be the FULL URL to POST to
// (notify.NtfyChannel sends to it exactly as stored — ntfy.sh or a
// self-hosted server both work the same way), not a bare topic name.
// The URL is a secret (anyone who knows it can read the alerts): it
// is added to the redaction list at once and never printed back.
func configureNotifications(ctx context.Context, a *app, in *bufio.Reader, out io.Writer, service string) error {
	fmt.Fprintln(out, "  Ballast can send alerts to your phone with ntfy (https://ntfy.sh — free, no account needed).")
	fmt.Fprintln(out, "  Install the ntfy app, subscribe to a hard-to-guess topic name, and paste its full link here.")
	url, err := askLine(out, in, "ntfy link, e.g. https://ntfy.sh/my-secret-topic (leave blank to skip)", "")
	if err != nil {
		return err
	}
	if url == "" {
		fmt.Fprintln(out, "  Skipped. You can set this up later: see Notifications in the web UI.")
		return nil
	}
	if !strings.HasPrefix(url, "https://") && !strings.HasPrefix(url, "http://") {
		return fmt.Errorf("the ntfy link must be a full web address starting with https:// (for example https://ntfy.sh/my-secret-topic)")
	}
	a.secrets.Add(url)
	credName := "ballast-ntfy-topic." + service
	// Ballast's own persistent directory, not $CREDENTIALS_DIRECTORY:
	// the file must still be there for the systemd service and after
	// a restart (spec §8). The notifier searches both.
	dir := a.cfg.WritableCredentialsDir
	path := filepath.Join(dir, credName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating credential directory %s: %w", dir, err)
	}
	if err := os.WriteFile(path, []byte(url+"\n"), 0o600); err != nil {
		return fmt.Errorf("saving the ntfy link to %s: %w", path, err)
	}
	if err := a.ops.SetNotificationSettings(ctx, service, notify.Settings{
		Service: service, Enabled: true, HeartbeatEnabled: true,
	}); err != nil {
		return fmt.Errorf("turning on notifications for %s: %w", service, err)
	}
	fmt.Fprintf(out, "  Saved (in %s). You'll get a message after every backup, good or bad.\n", path)
	if provided := filepath.Join(a.cfg.CredentialsDir, credName); provided != path {
		if _, err := os.Stat(provided); err == nil {
			fmt.Fprintf(out, "  Note: %s also exists and takes priority over the link you just entered. Remove it to use the new one.\n", provided)
		}
	}
	return nil
}

// schedulePresets are the plain-English answers configureSchedule
// accepts besides a raw cron expression.
var schedulePresets = map[string]string{
	"nightly": "0 3 * * *",
	"weekly":  "0 3 * * 0",
}

// configureSchedule asks how often to back up, offering plain-English
// presets so a beginner never has to write cron syntax.
func configureSchedule(ctx context.Context, a *app, in *bufio.Reader, out io.Writer, service string) error {
	fmt.Fprintln(out, "  nightly   every night at 3am (recommended)")
	fmt.Fprintln(out, "  weekly    every Sunday at 3am")
	fmt.Fprintln(out, "  none      only when you run it yourself")
	fmt.Fprintln(out, "  (or type a cron expression, e.g. \"30 2 * * *\")")
	choice, err := askLine(out, in, "How often", "nightly")
	if err != nil {
		return err
	}
	if strings.EqualFold(choice, "none") {
		fmt.Fprintf(out, "  No schedule. Run \"ballast backup %s\" whenever you like.\n", service)
		return nil
	}
	cron := choice
	if preset, ok := schedulePresets[strings.ToLower(choice)]; ok {
		cron = preset
	}
	if err := a.ops.SetSchedule(ctx, service, cron); err != nil {
		return fmt.Errorf("setting the schedule: %w", err)
	}
	fmt.Fprintf(out, "  Scheduled (%s).\n", cron)
	return nil
}

// verificationLabels describes each verification method in plain
// English for the wizard's final report.
var verificationLabels = map[string]string{
	"restic-read-back":  "Read files back out of the stored backup",
	"archive-integrity": "Checked the backup archive is complete and readable",
	"container-restore": "Restored the database into a temporary copy and checked it",
	"db-restore-drill":  "Restored the database into a temporary copy and checked it",
	"restart-smoke":     "Checked the service came back up after the backup",
}

// reportWizardResult prints the finished service's traffic-light
// status for every backup tier and its verification detail,
// regardless of whether the backup itself succeeded — the wizard's
// whole point is to show the truth, not just "done". Every tier is
// reported (spec rule 3): a failing secondary tier must never hide
// behind a green primary one.
func reportWizardResult(ctx context.Context, a *app, out io.Writer, service string) error {
	statuses, err := a.ops.Status(ctx, service, true)
	if err != nil {
		return err
	}
	if len(statuses) == 0 || len(statuses[0].Tiers) == 0 {
		return fmt.Errorf("%s: no backup status was recorded", service)
	}
	st := statuses[0]
	fmt.Fprintln(out)
	multi := len(st.Tiers) > 1
	for _, tier := range st.Tiers {
		label := service
		if multi {
			label = fmt.Sprintf("%s (%s copy)", service, tier.Tier)
		}
		printTierResult(out, label, tier)
	}
	if len(st.Verification) > 0 {
		fmt.Fprintln(out, "  Checks:")
	}
	for _, v := range st.Verification {
		result := "passed"
		if !v.Passed {
			result = "FAILED"
		}
		desc := verificationLabels[v.Method]
		if desc == "" {
			desc = v.Method
		}
		fmt.Fprintf(out, "    [%s] %s\n", result, desc)
	}
	allGreen := len(st.Tiers) > 0
	for _, tier := range st.Tiers {
		if tier.Light != "green" {
			allGreen = false
			break
		}
	}
	if allGreen {
		fmt.Fprintln(out, "\nAll done. Open the web UI (ballast serve) or run \"ballast status\" any time to check on your backups.")
	}
	return nil
}

func printTierResult(out io.Writer, label string, tier api.TierStatus) {
	switch tier.Light {
	case "green":
		fmt.Fprintf(out, "%s: SUCCESS. The backup was made and checked: it can be restored.\n", label)
	case "amber":
		fmt.Fprintf(out, "%s: WARNING. The backup was made but could NOT be checked. Don't rely on it until it shows green.\n", label)
	default:
		fmt.Fprintf(out, "%s: FAILED. The backup did not succeed.\n", label)
		if tier.LastError != "" {
			fmt.Fprintf(out, "  What went wrong: %s\n", tier.LastError)
		}
	}
}

// askLine prints prompt (with def shown in brackets when non-empty),
// reads one line from in, and returns def when the line is blank or
// input has ended (a non-interactive/piped stdin exhausted early —
// treated as "accept the default", not an error, since every prompt
// has a sane default of its own).
func askLine(out io.Writer, in *bufio.Reader, prompt, def string) (string, error) {
	if def != "" {
		fmt.Fprintf(out, "%s [%s]: ", prompt, def)
	} else {
		fmt.Fprintf(out, "%s: ", prompt)
	}
	line, err := in.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", fmt.Errorf("reading input: %w", err)
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return def, nil
	}
	return line, nil
}

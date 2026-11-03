package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func addLocalDestination(t *testing.T) {
	t.Helper()
	if _, err := execCLI(t, "destination", "add", "local", "--location", "/srv/backups"); err != nil {
		t.Fatalf("destination add: %v", err)
	}
}

func TestServiceAddListRemove(t *testing.T) {
	withStubRestic(t)
	cliEnv(t, &spyRunner{})
	addLocalDestination(t)

	out, err := execCLI(t, "service", "add", "docs", "--catalog", "folder", "--destination", "local", "--param", "path=/tmp")
	if err != nil {
		t.Fatalf("service add: %v (output %q)", err, out)
	}
	if !strings.Contains(out, "docs") || !strings.Contains(out, "folder") || !strings.Contains(out, "local") {
		t.Errorf("output = %q, want it to name the service, catalog entry and destination", out)
	}

	out, err = execCLI(t, "service", "list")
	if err != nil {
		t.Fatalf("service list: %v", err)
	}
	if !strings.Contains(out, "docs") {
		t.Errorf("list output = %q, want the service listed", out)
	}

	if _, err := execCLI(t, "service", "remove", "docs"); err != nil {
		t.Fatalf("service remove: %v", err)
	}
	out, err = execCLI(t, "service", "list")
	if err != nil {
		t.Fatalf("service list after remove: %v", err)
	}
	if strings.Contains(out, "docs") {
		t.Errorf("list output after remove = %q, want it gone", out)
	}
}

// TestServiceAddSecretFlag checks that `--secret name=` pre-declares
// a catalog {{ .Secrets.<name> }} reference (with the credential file
// name defaulted, since the value is empty) without needing an
// existing credential file yet — `ballast secret set` fills the value
// in afterwards.
func TestServiceAddSecretFlag(t *testing.T) {
	withStubRestic(t)
	cliEnv(t, &spyRunner{})
	addLocalDestination(t)

	if _, err := execCLI(t, "service", "add", "mysql", "--catalog", "folder", "--destination", "local",
		"--param", "path=/tmp", "--secret", "db_password="); err != nil {
		t.Fatalf("service add --secret: %v", err)
	}
	if _, err := execCLI(t, "secret", "set", "mysql", "db_password", "--value", "hunter2"); err != nil {
		t.Fatalf("secret set: %v", err)
	}
}

// TestServiceAddRetentionFlags checks that giving any --keep-* flag
// on `service add` sets a per-service retention override, filling in
// the destination's own value for any flag not given, and that a
// service added with none of them has no override at all (nil ==
// "follow the destination").
func TestServiceAddRetentionFlags(t *testing.T) {
	withStubRestic(t)
	cliEnv(t, &spyRunner{})
	if _, err := execCLI(t, "destination", "add", "local", "--location", "/srv/backups",
		"--keep-daily", "7", "--keep-weekly", "4", "--keep-monthly", "6"); err != nil {
		t.Fatalf("destination add: %v", err)
	}

	out, err := execCLI(t, "service", "add", "docs", "--catalog", "folder", "--destination", "local",
		"--param", "path=/tmp", "--keep-daily", "3")
	if err != nil {
		t.Fatalf("service add: %v (output %q)", err, out)
	}
	if !strings.Contains(out, "3 daily") {
		t.Errorf("output = %q, want it to confirm the override", out)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	a, err := openApp(cfg, &discardWriter{})
	if err != nil {
		t.Fatalf("openApp: %v", err)
	}
	defer a.Close()
	svc, err := a.ops.GetService(t.Context(), "docs")
	if err != nil {
		t.Fatalf("GetService: %v", err)
	}
	if svc.Retention == nil {
		t.Fatal("Retention = nil, want an override")
	}
	if svc.Retention.KeepDaily != 3 || svc.Retention.KeepWeekly != 4 || svc.Retention.KeepMonthly != 6 {
		t.Errorf("Retention = %+v, want {3 4 6} (unspecified flags fall back to the destination's own value)", svc.Retention)
	}

	if _, err := execCLI(t, "service", "add", "notes", "--catalog", "folder", "--destination", "local", "--param", "path=/tmp2"); err != nil {
		t.Fatalf("service add: %v", err)
	}
	svc2, err := a.ops.GetService(t.Context(), "notes")
	if err != nil {
		t.Fatalf("GetService: %v", err)
	}
	if svc2.Retention != nil {
		t.Errorf("Retention = %+v, want nil (no --keep-* flag given)", svc2.Retention)
	}
}

// TestServiceRetentionCommand checks `ballast service retention` can
// set an override, print the effective policy with no flags, and
// clear the override again with --inherit.
func TestServiceRetentionCommand(t *testing.T) {
	withStubRestic(t)
	cliEnv(t, &spyRunner{})
	if _, err := execCLI(t, "destination", "add", "local", "--location", "/srv/backups",
		"--keep-daily", "7", "--keep-weekly", "4", "--keep-monthly", "6"); err != nil {
		t.Fatalf("destination add: %v", err)
	}
	if _, err := execCLI(t, "service", "add", "docs", "--catalog", "folder", "--destination", "local", "--param", "path=/tmp"); err != nil {
		t.Fatalf("service add: %v", err)
	}

	out, err := execCLI(t, "service", "retention", "docs")
	if err != nil {
		t.Fatalf("service retention: %v", err)
	}
	if !strings.Contains(out, "same as local") {
		t.Errorf("output = %q, want it to say the service inherits the destination's policy", out)
	}

	out, err = execCLI(t, "service", "retention", "docs", "--keep-daily", "2")
	if err != nil {
		t.Fatalf("service retention --keep-daily: %v (output %q)", err, out)
	}
	if !strings.Contains(out, "2 daily") || !strings.Contains(out, "its own setting") {
		t.Errorf("output = %q, want it to show the new override", out)
	}

	out, err = execCLI(t, "service", "retention", "docs", "--inherit")
	if err != nil {
		t.Fatalf("service retention --inherit: %v (output %q)", err, out)
	}
	if !strings.Contains(out, "same as local") {
		t.Errorf("output = %q, want the override cleared", out)
	}

	if _, err := execCLI(t, "service", "retention", "docs", "--inherit", "--keep-daily", "1"); err == nil {
		t.Error("service retention --inherit --keep-daily: want an error (mutually exclusive)")
	}
}

func TestServiceAddRequiresCatalogAndDestination(t *testing.T) {
	cliEnv(t, &spyRunner{})
	if _, err := execCLI(t, "service", "add", "docs"); err == nil {
		t.Error("service add without --catalog/--destination: want an error")
	}
}

func TestServiceAddUnknownCatalogEntry(t *testing.T) {
	withStubRestic(t)
	cliEnv(t, &spyRunner{})
	addLocalDestination(t)
	_, err := execCLI(t, "service", "add", "docs", "--catalog", "ghost", "--destination", "local")
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Errorf("service add with unknown catalog entry = %v, want it named", err)
	}
}

func TestServiceAddMissingRequiredParam(t *testing.T) {
	withStubRestic(t)
	cliEnv(t, &spyRunner{})
	addLocalDestination(t)
	_, err := execCLI(t, "service", "add", "docs", "--catalog", "folder", "--destination", "local")
	if err == nil || !strings.Contains(err.Error(), "path") {
		t.Errorf("service add without required param = %v, want it to name the missing parameter", err)
	}
}

func TestServiceAddInvalidParamFormat(t *testing.T) {
	withStubRestic(t)
	cliEnv(t, &spyRunner{})
	addLocalDestination(t)
	_, err := execCLI(t, "service", "add", "docs", "--catalog", "folder", "--destination", "local", "--param", "no-equals-sign")
	if err == nil {
		t.Error("service add with a malformed --param: want an error")
	}
}

// TestServiceListGuestJobWithoutDestination checks that a Proxmox
// guest job with no restic destination is listed (it used to panic).
func TestServiceListGuestJobWithoutDestination(t *testing.T) {
	withStubRestic(t)
	cliEnv(t, &spyRunner{})
	srv := fakeProxmoxServer(t, "PVEAPIToken=ballast@pve!backup=s3cr3t")
	if out, err := execCLI(t, "proxmox", "connect", "--host", srv.URL,
		"--token-id", "ballast@pve!backup", "--token-secret", "s3cr3t", "--insecure"); err != nil {
		t.Fatalf("proxmox connect: %v (output %q)", err, out)
	}
	if out, err := execCLI(t, "proxmox", "job", "add", "--name", "pve-vm100", "--node", "pve",
		"--vmid", "100", "--storage", "backup-nas"); err != nil {
		t.Fatalf("proxmox job add: %v (output %q)", err, out)
	}
	out, err := execCLI(t, "service", "list")
	if err != nil {
		t.Fatalf("service list: %v", err)
	}
	if !strings.Contains(out, "pve-vm100") || !strings.Contains(out, "runs on Proxmox, no restic destination") {
		t.Errorf("list output = %q, want the guest job with its explanation", out)
	}
}

func TestServiceAdd_RelativePathRefused(t *testing.T) {
	withStubRestic(t)
	cliEnv(t, &spyRunner{})
	addLocalDestination(t)
	out, err := execCLI(t, "service", "add", "docs", "--catalog", "folder", "--destination", "local", "--param", "path=data")
	if err == nil {
		t.Fatalf("service add with a relative path succeeded; output %q", out)
	}
	if !strings.Contains(err.Error(), "full path") {
		t.Errorf("error = %v, want plain advice about a full path", err)
	}
	if list, _ := execCLI(t, "service", "list"); strings.Contains(list, "docs") {
		t.Errorf("service was saved anyway: %q", list)
	}
}

func TestServiceAdd_TrailingSpaceTrimmed(t *testing.T) {
	withStubRestic(t)
	cliEnv(t, &spyRunner{})
	addLocalDestination(t)
	dir := t.TempDir()
	stdout, stderr, err := execCLISplit(t, "service", "add", "docs", "--catalog", "folder", "--destination", "local", "--param", "path="+dir+" ")
	if err != nil {
		t.Fatalf("service add: %v", err)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want no warning for an existing folder", stderr)
	}
	if !strings.Contains(stdout, "docs") {
		t.Errorf("stdout = %q", stdout)
	}
}

func TestServiceAdd_MissingPathWarnsOnStderr(t *testing.T) {
	withStubRestic(t)
	cliEnv(t, &spyRunner{})
	addLocalDestination(t)
	missing := filepath.Join(t.TempDir(), "unmounted")
	stdout, stderr, err := execCLISplit(t, "service", "add", "docs", "--catalog", "folder", "--destination", "local", "--param", "path="+missing)
	if err != nil {
		t.Fatalf("service add: %v", err)
	}
	if want := "warning: " + missing + " does not exist yet; it is saved, but backups fail until it does (is the drive mounted?)"; !strings.Contains(stderr, want) {
		t.Errorf("stderr = %q, want %q", stderr, want)
	}
	if strings.Contains(stdout, "warning") || !strings.Contains(stdout, "configured") {
		t.Errorf("stdout = %q, want the usual line with no warning", stdout)
	}
}

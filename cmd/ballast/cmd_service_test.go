package main

import (
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

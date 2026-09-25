package main

import (
	"strings"
	"testing"
)

func TestCatalogList(t *testing.T) {
	cliEnv(t, &spyRunner{})
	out, err := execCLI(t, "catalog", "list")
	if err != nil {
		t.Fatalf("catalog list: %v", err)
	}
	for _, want := range []string{"postgres", "mysql", "folder"} {
		if !strings.Contains(out, want) {
			t.Errorf("catalog list output = %q, want it to include the bundled entry %q", out, want)
		}
	}
}

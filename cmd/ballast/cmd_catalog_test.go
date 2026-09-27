package main

import (
	"os"
	"path/filepath"
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

func TestCatalogValidate(t *testing.T) {
	cliEnv(t, &spyRunner{})

	// Write a valid minimal catalog YAML to a temp file.
	validYAML := `apiVersion: ballast.dev/v1
service: testservice
version: 1
display:
  name: Test Service
  description: A test service for validation
backup:
  strategy: files
  files:
    - path: "{{ .Params.path }}"
parameters:
  - name: path
    required: true
    prompt: "Path to back up"
verify:
  method: archive-integrity
`
	tmpFile := filepath.Join(t.TempDir(), "test-catalog.yaml")
	if err := os.WriteFile(tmpFile, []byte(validYAML), 0o644); err != nil {
		t.Fatalf("writing test catalog: %v", err)
	}

	out, err := execCLI(t, "catalog", "validate", tmpFile)
	if err != nil {
		t.Fatalf("catalog validate: %v", err)
	}
	if !strings.Contains(out, "testservice") {
		t.Errorf("catalog validate output = %q, want it to include service name %q", out, "testservice")
	}
	if !strings.Contains(out, "valid") {
		t.Errorf("catalog validate output = %q, want it to indicate validity", out)
	}
}

func TestCatalogValidateInvalid(t *testing.T) {
	cliEnv(t, &spyRunner{})

	// Write an invalid catalog YAML (missing required field: backup).
	invalidYAML := `apiVersion: ballast.dev/v1
service: testservice
version: 1
display:
  name: Test Service
`
	tmpFile := filepath.Join(t.TempDir(), "invalid-catalog.yaml")
	if err := os.WriteFile(tmpFile, []byte(invalidYAML), 0o644); err != nil {
		t.Fatalf("writing invalid catalog: %v", err)
	}

	_, err := execCLI(t, "catalog", "validate", tmpFile)
	if err == nil {
		t.Error("catalog validate should have failed for invalid YAML, but it succeeded")
	}
}

func TestCatalogValidateInvalidImagePattern(t *testing.T) {
	cliEnv(t, &spyRunner{})

	// detect.docker.imagePattern is not a valid regular expression
	// (unbalanced parenthesis): catalog validate must catch this, not
	// let it silently compile into "never matches" at detect time.
	invalidYAML := `apiVersion: ballast.dev/v1
service: testservice
version: 1
display:
  name: Test Service
  description: A test service for validation
detect:
  docker:
    imagePattern: "testservice("
backup:
  strategy: files
  files:
    - path: "{{ .Params.path }}"
parameters:
  - name: path
    required: true
    prompt: "Path to back up"
verify:
  method: archive-integrity
`
	tmpFile := filepath.Join(t.TempDir(), "bad-pattern-catalog.yaml")
	if err := os.WriteFile(tmpFile, []byte(invalidYAML), 0o644); err != nil {
		t.Fatalf("writing invalid catalog: %v", err)
	}

	out, err := execCLI(t, "catalog", "validate", tmpFile)
	if err == nil {
		t.Fatalf("catalog validate with a broken imagePattern regex: want an error (output %q)", out)
	}
	if !strings.Contains(err.Error(), "not a valid regular expression") {
		t.Errorf("catalog validate error = %v, want it to name the bad imagePattern", err)
	}
}

func TestCatalogInstall(t *testing.T) {
	configDir := filepath.Join(t.TempDir(), "config")
	dataDir := filepath.Join(t.TempDir(), "data")
	t.Setenv("BALLAST_CONFIG_DIR", configDir)
	t.Setenv("BALLAST_DATA_DIR", dataDir)
	t.Setenv("CREDENTIALS_DIRECTORY", "")
	prev := runner
	runner = &spyRunner{}
	t.Cleanup(func() {
		runner = prev
	})

	// Write a valid catalog YAML with a unique service name (not in bundled entries).
	validYAML := `apiVersion: ballast.dev/v1
service: customservice
version: 1
display:
  name: Custom Service
  description: A custom test service
backup:
  strategy: files
  files:
    - path: "{{ .Params.path }}"
parameters:
  - name: path
    required: true
    prompt: "Path to back up"
verify:
  method: archive-integrity
`
	tmpFile := filepath.Join(t.TempDir(), "custom-catalog.yaml")
	if err := os.WriteFile(tmpFile, []byte(validYAML), 0o644); err != nil {
		t.Fatalf("writing custom catalog: %v", err)
	}

	out, err := execCLI(t, "catalog", "install", tmpFile)
	if err != nil {
		t.Fatalf("catalog install: %v", err)
	}
	if !strings.Contains(out, "customservice") {
		t.Errorf("catalog install output = %q, want it to include service name %q", out, "customservice")
	}
	if !strings.Contains(out, "Installed") {
		t.Errorf("catalog install output = %q, want it to indicate installation success", out)
	}

	// Verify the file was written to the correct location.
	expectedPath := filepath.Join(configDir, "catalog.d", "customservice.yaml")
	if _, err := os.Stat(expectedPath); err != nil {
		t.Errorf("expected installed catalog file at %s, but got error: %v", expectedPath, err)
	}

	// Verify the installed file has the correct contents.
	installedData, err := os.ReadFile(expectedPath)
	if err != nil {
		t.Fatalf("reading installed catalog: %v", err)
	}
	if string(installedData) != validYAML {
		t.Errorf("installed catalog contents differ from original")
	}
}

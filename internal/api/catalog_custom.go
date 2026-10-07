package api

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/springledev/ballast/internal/catalog"
)

// CatalogTemplate is a starting point for a custom catalog entry (spec
// §19.9). The YAML is ordinary catalog text: it goes through the same
// validation as any other entry before it can be installed.
type CatalogTemplate struct {
	// ID is the stable template name ("database-dump", ...).
	ID string `json:"id"`
	// Label is the plain-English name shown in the picker.
	Label string `json:"label"`
	// Description says what the template is for.
	Description string `json:"description"`
	// YAML is the starting entry, with comments that explain each part.
	YAML string `json:"yaml"`
}

// CatalogValidation is the outcome of checking a catalog entry's text.
type CatalogValidation struct {
	// Valid is true when the text is a complete, valid entry.
	Valid bool `json:"valid"`
	// Service is the entry's service name when it is valid.
	Service string `json:"service,omitempty"`
	// Error is the same message `ballast catalog validate` prints.
	Error string `json:"error,omitempty"`
	// Notice is a non-fatal remark (for example, that this replaces an
	// entry you installed earlier).
	Notice string `json:"notice,omitempty"`
}

// CatalogInstallResult is returned after a custom entry is installed.
type CatalogInstallResult struct {
	// Service is the installed entry's service name.
	Service string `json:"service"`
	// Path is the file it was written to.
	Path string `json:"path"`
}

// CatalogTemplates returns the custom-entry templates.
func (o *Ops) CatalogTemplates() []CatalogTemplate {
	out := make([]CatalogTemplate, len(catalogTemplates))
	copy(out, catalogTemplates)
	return out
}

// ValidateCatalogYAML checks entry text with exactly the rules and
// messages of `ballast catalog validate`, and additionally reports a
// name clash with a built-in entry, which installing would refuse.
func (o *Ops) ValidateCatalogYAML(text string) CatalogValidation {
	entry, err := catalog.Load([]byte(text))
	if err != nil {
		return CatalogValidation{Error: err.Error()}
	}
	v := CatalogValidation{Valid: true, Service: entry.Service}
	if _, ok := o.Catalog.Get(entry.Service); ok {
		if o.Catalog.IsUser(entry.Service) {
			v.Notice = fmt.Sprintf("A custom entry named %q is already installed; installing replaces it.", entry.Service)
		} else {
			v.Valid = false
			v.Error = fmt.Sprintf("service %q is a built-in catalog entry; pick a different service name for your own entry", entry.Service)
		}
	}
	return v
}

// InstallCatalogYAML validates entry text and installs it into the
// first user catalog directory as <service>.yaml, the same place
// `ballast catalog install` uses, and makes it available at once.
func (o *Ops) InstallCatalogYAML(text string) (*CatalogInstallResult, error) {
	if len(o.CatalogDirs) == 0 {
		return nil, fmt.Errorf("%w: no custom catalog folder is configured", ErrValidation)
	}
	entry, err := catalog.Load([]byte(text))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	dir := o.CatalogDirs[0]
	dest := filepath.Join(dir, entry.Service+".yaml")
	// Register first: it refuses a clash before anything is written.
	if err := o.Catalog.Install(entry, dest); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	if err := writeFileAtomic(dir, dest, []byte(text)); err != nil {
		return nil, fmt.Errorf("installing catalog entry %s: %w", entry.Service, err)
	}
	return &CatalogInstallResult{Service: entry.Service, Path: dest}, nil
}

// writeFileAtomic writes data to dest via a temp file in dir, so a
// crash never leaves half an entry that would stop Ballast starting.
func writeFileAtomic(dir, dest string, data []byte) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating catalog directory %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".entry-*.tmp")
	if err != nil {
		return fmt.Errorf("writing %s: %w", dest, err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("writing %s: %w", dest, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", dest, err)
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", dest, err)
	}
	if err := os.Rename(tmp.Name(), dest); err != nil {
		return fmt.Errorf("writing %s: %w", dest, err)
	}
	return nil
}

var catalogTemplates = []CatalogTemplate{
	{
		ID:          "database-dump",
		Label:       "Database dump",
		Description: "Dump a database running in a container, then prove the dump restores in a throwaway container.",
		YAML: `apiVersion: ballast.dev/v1
# The short name of this service: lower-case letters, digits and dashes.
service: my-database
version: 1
display:
  name: My database
  description: A PostgreSQL database in a Docker container
parameters:
  - name: container
    required: true
    prompt: "Name of the database container"
  - name: image
    required: true
    prompt: "Image for the test restore (same major version as your server, e.g. postgres:16)"
backup:
  strategy: dump
  dumps:
    - name: database
      container: "{{ .Params.container }}"
      # The command runs inside the container and prints the dump.
      command: ["pg_dumpall", "-U", "postgres"]
      format: sql
verify:
  method: db-restore-drill
  image: "{{ .Params.image }}"
  restartPolicy: never
notifications:
  failureVerbosity: full
`,
	},
	{
		ID:          "stop-and-copy-folder",
		Label:       "Stop a container and copy its folder",
		Description: "Briefly stop a container, copy a folder it uses, start it again, and check the copy is readable.",
		YAML: `apiVersion: ballast.dev/v1
service: my-app
version: 1
display:
  name: My app
  description: An app that keeps its data in one folder, stopped briefly while copied
parameters:
  - name: container
    required: true
    prompt: "Name of the container to stop while copying"
backup:
  strategy: stop-start
  stopStart:
    containers:
      - "{{ .Params.container }}"
  hostPaths:
    # The folder is read from where the container keeps it; "mount" is the
    # path inside the container.
    - container: "{{ .Params.container }}"
      mount: /data
verify:
  method: archive-integrity
  restartPolicy: always
notifications:
  failureVerbosity: full
`,
	},
	{
		ID:          "volume",
		Label:       "Docker volume",
		Description: "Copy a Docker volume while its container is stopped.",
		YAML: `apiVersion: ballast.dev/v1
service: my-volume
version: 1
display:
  name: My volume
  description: A Docker volume, copied while its container is stopped
parameters:
  - name: volume
    required: true
    picker: docker-volume
    prompt: "Name of the Docker volume"
  - name: container
    required: true
    prompt: "Container that uses the volume (stopped during the copy)"
backup:
  strategy: volume
  volumes:
    - name: "{{ .Params.volume }}"
  stopStart:
    containers:
      - "{{ .Params.container }}"
verify:
  method: archive-integrity
  restartPolicy: always
notifications:
  failureVerbosity: full
`,
	},
	{
		ID:          "script",
		Label:       "My own script",
		Description: "Run your own backup script and your own check script. Without a check script the light stays amber.",
		YAML: `apiVersion: ballast.dev/v1
service: my-script
version: 1
display:
  name: My script
  description: A backup made by my own script
backup:
  strategy: exec
  exec:
    # Runs on the Ballast machine. Write the backup files into the folder
    # named by $BALLAST_ARTIFACT_DIR and exit 0 when it worked.
    script: /usr/local/bin/my-backup.sh
    # Optional. Runs after the backup with the same $BALLAST_ARTIFACT_DIR;
    # exit 0 means "I checked this backup and it can be restored".
    # Leave it out and Ballast shows amber (backed up, not proven).
    verifyScript: /usr/local/bin/my-backup-check.sh
verify:
  method: exec-script
notifications:
  failureVerbosity: full
`,
	},
}

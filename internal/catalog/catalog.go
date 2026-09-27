// Package catalog loads and validates Ballast service catalog entries.
//
// A catalog entry is a YAML file describing how to back up one service (for
// example Immich): which strategy to use, what parameters the user must
// supply, and how the backup is verified. Entries are validated against the
// versioned JSON Schema in schema/catalog-v1.json before they are parsed into
// the typed Entry struct, so unknown keys and malformed fields are rejected
// as early as possible.
//
// This package contains no per-service logic: adding a new service is a
// YAML file, not Go code.
package catalog

// Entry is a validated service catalog definition.
type Entry struct {
	APIVersion string  `yaml:"apiVersion" json:"apiVersion"`
	Service    string  `yaml:"service" json:"service"`
	Version    int     `yaml:"version" json:"version"`
	Display    Display `yaml:"display,omitempty" json:"display,omitempty"`
	Detect     Detect  `yaml:"detect,omitempty" json:"detect,omitempty"`
	// Discovery declares, per referenced container/guest, how to find
	// it at job-run time instead of hardcoding a name (spec §6.1). Keys
	// become {{ .Discovery.<name> }} placeholders.
	Discovery     map[string]DiscoveryRule `yaml:"discovery,omitempty" json:"discovery,omitempty"`
	Parameters    []Parameter              `yaml:"parameters,omitempty" json:"parameters,omitempty"`
	Backup        Backup                   `yaml:"backup" json:"backup"`
	Verify        Verify                   `yaml:"verify" json:"verify"`
	Notifications Notifications            `yaml:"notifications,omitempty" json:"notifications,omitempty"`
}

// Display holds user-facing names shown in the CLI and web UI.
type Display struct {
	Name        string `yaml:"name,omitempty" json:"name,omitempty"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
}

// Detect describes how to auto-detect a running instance of the service.
type Detect struct {
	Docker *DockerDetect `yaml:"docker,omitempty" json:"docker,omitempty"`
}

// DockerDetect matches service containers by image pattern or env vars.
type DockerDetect struct {
	ImagePattern string            `yaml:"imagePattern,omitempty" json:"imagePattern,omitempty"`
	OrEnv        map[string]string `yaml:"orEnv,omitempty" json:"orEnv,omitempty"`
}

// DiscoveryRule resolves one {{ .Discovery.<name> }} value at job-run
// time (spec §6.1). Today the only supported type is "docker-label":
// match is a "label=value" filter passed to `docker ps --filter`;
// exactly one running container must match. Resolution failure MUST
// fail the job loudly — fallback, a plain template string (typically
// {{ .Params.* }}), is used only when discovery finds no match, never
// silently beyond that.
type DiscoveryRule struct {
	Type     string `yaml:"type" json:"type"`
	Match    string `yaml:"match" json:"match"`
	Fallback string `yaml:"fallback,omitempty" json:"fallback,omitempty"`
}

// Parameter is a user-supplied value (e.g. "library_dir") referenced in
// backup/verify templates as {{ .Params.NAME }}.
type Parameter struct {
	Name     string `yaml:"name" json:"name"`
	Required bool   `yaml:"required,omitempty" json:"required,omitempty"`
	Prompt   string `yaml:"prompt,omitempty" json:"prompt,omitempty"`
	Default  string `yaml:"default,omitempty" json:"default,omitempty"`
}

// Backup describes how to create the backup artifacts.
type Backup struct {
	Strategy  string      `yaml:"strategy" json:"strategy"`
	Dumps     []Dump      `yaml:"dumps,omitempty" json:"dumps,omitempty"`
	Files     []FileSpec  `yaml:"files,omitempty" json:"files,omitempty"`
	StopStart *StopStart  `yaml:"stopStart,omitempty" json:"stopStart,omitempty"`
	Exec      *ExecBackup `yaml:"exec,omitempty" json:"exec,omitempty"`
}

// Dump is one dump command run inside a container (typically docker exec)
// producing an artifact (e.g. pg_dumpall to stdout).
type Dump struct {
	Name      string `yaml:"name" json:"name"`
	Container string `yaml:"container,omitempty" json:"container,omitempty"`
	// Transport selects how Container is reached (spec §4.2):
	// "docker-exec" (default) runs the command via `docker exec`;
	// "pct-exec" runs it via Proxmox's `pct exec` for bare-LXC guests
	// with no Docker daemon. pct-exec is read/dump-only and does not
	// support SecretEnv (no argv-safe forwarding mechanism exists for
	// it, unlike docker exec -e KEY).
	Transport string   `yaml:"transport,omitempty" json:"transport,omitempty"`
	Command   []string `yaml:"command" json:"command"`
	Format    string   `yaml:"format,omitempty" json:"format,omitempty"`
	// SecretEnv is extra KEY: "{{ .Secrets.NAME }}" environment for the
	// dump command, e.g. MYSQL_PWD for mysqldump. Values are template
	// strings resolved by Entry.ResolveSecrets — never by Resolve,
	// which has no access to secret values — and are forwarded to the
	// command exactly like strategy.Request.SecretEnv: in the
	// environment, never argv, and redacted from every log line and
	// error. {{ .Params.* }} / {{ .Services.* }} are not accepted
	// here; only {{ .Secrets.NAME }} is (spec §6 schema rule: a
	// deliberately narrow grammar).
	SecretEnv map[string]string `yaml:"secretEnv,omitempty" json:"secretEnv,omitempty"`
}

// FileSpec is a filesystem path included directly in the backup.
type FileSpec struct {
	Path string `yaml:"path" json:"path"`
}

// StopStart lists the containers that the stop-start strategy must stop
// (tar their data) and guarantee to restart afterwards, even on failure.
type StopStart struct {
	Containers []string `yaml:"containers,omitempty" json:"containers,omitempty"`
}

// ExecBackup is the Phase-2 reserved external contract: run a user-supplied
// script; exit 0 means a dump artifact exists. Verification is a paired
// script when defined. No per-service logic ships in v0.1.
type ExecBackup struct {
	Script       string   `yaml:"script" json:"script"`
	Args         []string `yaml:"args,omitempty" json:"args,omitempty"`
	VerifyScript string   `yaml:"verifyScript,omitempty" json:"verifyScript,omitempty"`
}

// Verify defines the verification hook paired with the backup strategy.
// Every entry must define at least method "archive-integrity".
type Verify struct {
	Method        string   `yaml:"method" json:"method"`
	Image         string   `yaml:"image,omitempty" json:"image,omitempty"`
	FilterStderr  []string `yaml:"filterStderr,omitempty" json:"filterStderr,omitempty"`
	RestartPolicy string   `yaml:"restartPolicy,omitempty" json:"restartPolicy,omitempty"`
}

// Notifications configures per-service alerting behaviour.
type Notifications struct {
	FailureVerbosity string `yaml:"failureVerbosity,omitempty" json:"failureVerbosity,omitempty"`
	SuccessHeartbeat bool   `yaml:"successHeartbeat,omitempty" json:"successHeartbeat,omitempty"`
}

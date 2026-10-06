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
	Discovery  map[string]DiscoveryRule `yaml:"discovery,omitempty" json:"discovery,omitempty"`
	Parameters []Parameter              `yaml:"parameters,omitempty" json:"parameters,omitempty"`
	// EnvSource lets setup read the app's documented .env keys from
	// one directory path (spec §18.2).
	EnvSource     *EnvSource    `yaml:"envSource,omitempty" json:"envSource,omitempty"`
	Backup        Backup        `yaml:"backup" json:"backup"`
	Verify        Verify        `yaml:"verify" json:"verify"`
	Notifications Notifications `yaml:"notifications,omitempty" json:"notifications,omitempty"`
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
	// Picker tells the web UI how to help the user fill the value in.
	// "docker-volume" offers the named volumes on the service's Docker
	// host; empty means a plain text box. It never changes how the
	// value is used — the parameter is still an ordinary string.
	Picker string `yaml:"picker,omitempty" json:"picker,omitempty"`
}

// EnvSource declares the documented keys of an app's .env file, giving
// a one-field setup: the user supplies the app's directory and setup
// reads each key (spec §18.2). A key fills either a parameter (a
// non-secret value such as a database user, shown for confirmation) or
// a secret (stored only as an env-file reference, never copied).
type EnvSource struct {
	// Prompt asks for the app's directory in plain English.
	Prompt string `yaml:"prompt,omitempty" json:"prompt,omitempty"`
	// File is the .env file name inside that directory (default ".env").
	File string         `yaml:"file,omitempty" json:"file,omitempty"`
	Keys []EnvSourceKey `yaml:"keys" json:"keys"`
}

// EnvSourceKey maps one .env key to a parameter or a secret name.
// Exactly one of Param and Secret is set.
type EnvSourceKey struct {
	Key    string `yaml:"key" json:"key"`
	Param  string `yaml:"param,omitempty" json:"param,omitempty"`
	Secret string `yaml:"secret,omitempty" json:"secret,omitempty"`
	// Optional keys that are absent are skipped instead of reported
	// as missing.
	Optional bool `yaml:"optional,omitempty" json:"optional,omitempty"`
}

// Backup describes how to create the backup artifacts.
type Backup struct {
	Strategy string     `yaml:"strategy" json:"strategy"`
	Dumps    []Dump     `yaml:"dumps,omitempty" json:"dumps,omitempty"`
	Files    []FileSpec `yaml:"files,omitempty" json:"files,omitempty"`
	// Volumes are the named Docker volumes the volume strategy
	// archives (spec §17.5).
	Volumes []VolumeSpec `yaml:"volumes,omitempty" json:"volumes,omitempty"`
	// HostPaths are folders on the service's Docker host, archived
	// through a read-only helper container on that host (spec §19.2).
	// Unlike Files, they are never read from the Ballast machine.
	HostPaths []HostPathSpec `yaml:"hostPaths,omitempty" json:"hostPaths,omitempty"`
	StopStart *StopStart     `yaml:"stopStart,omitempty" json:"stopStart,omitempty"`
	Exec      *ExecBackup    `yaml:"exec,omitempty" json:"exec,omitempty"`
	// Compose is the Docker Compose project the compose strategy
	// snapshots (config files + .env + container labels; spec §14
	// step 22).
	Compose *ComposeSpec `yaml:"compose,omitempty" json:"compose,omitempty"`
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
	// Exclude lists patterns for things inside Path that are not
	// backed up (see ValidateExcludes for the pattern rules).
	Exclude []string `yaml:"exclude,omitempty" json:"exclude,omitempty"`
}

// HostPathSpec is one folder on the service's Docker host. Exactly one
// of Path (an absolute path on that host) and Mount (a container mount
// point such as /data, resolved from `docker inspect` of Container at
// run time, so a moved folder is followed automatically) is set.
type HostPathSpec struct {
	Path      string `yaml:"path,omitempty" json:"path,omitempty"`
	Mount     string `yaml:"mount,omitempty" json:"mount,omitempty"`
	Container string `yaml:"container,omitempty" json:"container,omitempty"`
	// Exclude lists patterns for things inside the folder that are not
	// backed up (see ValidateExcludes for the pattern rules).
	Exclude []string `yaml:"exclude,omitempty" json:"exclude,omitempty"`
}

// VolumeSpec is one named Docker volume archived by the volume
// strategy. It is read through a throwaway helper container on the
// service's Docker host, so no host filesystem access is needed.
type VolumeSpec struct {
	Name string `yaml:"name" json:"name"`
}

// ComposeSpec names the Docker Compose project the compose strategy
// snapshots. Project is looked up by name among the projects Ballast
// can see running (or last run) on the service's Docker host — never a
// host path, so a remote host needs no filesystem access beyond what
// `docker ps` already exposes via container labels.
type ComposeSpec struct {
	Project string `yaml:"project" json:"project"`
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

// Verify defines the verification methods paired with the backup
// strategy (spec §7, §17.6). An entry names exactly one of Method (a
// single method) or Methods (several, all of which must pass).
type Verify struct {
	Method        string   `yaml:"method,omitempty" json:"method,omitempty"`
	Methods       []string `yaml:"methods,omitempty" json:"methods,omitempty"`
	Image         string   `yaml:"image,omitempty" json:"image,omitempty"`
	FilterStderr  []string `yaml:"filterStderr,omitempty" json:"filterStderr,omitempty"`
	RestartPolicy string   `yaml:"restartPolicy,omitempty" json:"restartPolicy,omitempty"`
	Smoke         *Smoke   `yaml:"smoke,omitempty" json:"smoke,omitempty"`
}

// LegacyContainerRestore is the pre-registry name of the
// db-restore-drill method. Entries written before the rename keep
// working; MethodList reports the new name.
const LegacyContainerRestore = "container-restore"

// MethodList returns the declared verification methods in catalog
// order, without duplicates, with the legacy "container-restore" name
// reported as "db-restore-drill".
func (v Verify) MethodList() []string {
	all := v.Methods
	if v.Method != "" {
		all = append([]string{v.Method}, all...)
	}
	out := make([]string, 0, len(all))
	seen := make(map[string]bool, len(all))
	for _, m := range all {
		if m == LegacyContainerRestore {
			m = "db-restore-drill"
		}
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}

// Smoke configures the restart-smoke verification method: after the
// backup, Container must be running (and healthy, if it has a Docker
// health check), and the optional HTTP or TCP probe must succeed from
// inside the container's own network — all within Timeout.
type Smoke struct {
	Container string     `yaml:"container" json:"container"`
	Timeout   string     `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	HTTP      *SmokeHTTP `yaml:"http,omitempty" json:"http,omitempty"`
	TCP       *SmokeTCP  `yaml:"tcp,omitempty" json:"tcp,omitempty"`
}

// SmokeHTTP is an HTTP probe against the container's own localhost.
// ExpectStatus defaults to 200; Path defaults to "/".
type SmokeHTTP struct {
	Port         int    `yaml:"port" json:"port"`
	Path         string `yaml:"path,omitempty" json:"path,omitempty"`
	ExpectStatus int    `yaml:"expectStatus,omitempty" json:"expectStatus,omitempty"`
}

// SmokeTCP is a TCP connect probe against the container's own
// localhost.
type SmokeTCP struct {
	Port int `yaml:"port" json:"port"`
}

// Notifications configures per-service alerting behaviour.
type Notifications struct {
	FailureVerbosity string `yaml:"failureVerbosity,omitempty" json:"failureVerbosity,omitempty"`
	SuccessHeartbeat bool   `yaml:"successHeartbeat,omitempty" json:"successHeartbeat,omitempty"`
}

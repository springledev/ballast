// Types mirror the JSON shapes internal/api and its dependencies
// produce (spec §9: "All operations are also full REST API
// endpoints ... the GUI uses exactly this API"). Field names are
// snake_case to match the Go JSON tags exactly, so no translation
// layer can drift out of sync with the server.

export interface CatalogParameter {
  name: string;
  required?: boolean;
  prompt?: string;
  default?: string;
  /** "docker-volume" / "compose-project": show a picker of the service's
   * Docker host's volumes / Compose projects. */
  picker?: string;
}

export interface CatalogSummary {
  service: string;
  name: string;
  description: string;
  parameters?: CatalogParameter[];
}

/** How many of the newest daily/weekly/monthly backups to keep. */
export interface Retention {
  keep_daily: number;
  keep_weekly: number;
  keep_monthly: number;
}

export interface Service {
  name: string;
  catalog_entry: string;
  destination: string;
  params?: Record<string, string>;
  containers?: Record<string, string>;
  /** Which Docker host runs this service's containers. Defaults to "local". */
  docker_host?: string;
  /** This service's own backup-retention override. Unset: follow the destination's policy. */
  retention?: Retention | null;
  /** Groups this service with others of the same Docker Compose stack, for the full-stack restore wizard. */
  project?: string;
  /** The Proxmox node this service's guest lives on, when attached to one. Set together with proxmox_vmid. */
  proxmox_node?: string;
  /** The Proxmox VM/LXC ID this service is attached to, when set. Set together with proxmox_node. */
  proxmox_vmid?: number;
}

/** One service's outcome within a full-stack restore (`ballast restore-project`). */
export interface RestoreProjectResult {
  service: string;
  kind: string;
  snapshot_id?: string;
  restored_to?: string;
  volumes?: string[];
  error?: string;
}

/** The outcome of restoring every service in a project together. */
export interface RestoreProjectReport {
  project: string;
  results: RestoreProjectResult[];
}

/** Which storage backend a destination uses. */
export type DestinationType = "local" | "b2" | "s3" | "r2" | "sftp" | "custom";

/**
 * A destination's non-secret backend settings. Which fields apply
 * depends on its type; the server rejects any others being sent.
 */
export interface DestinationSettings {
  path?: string;
  endpoint?: string;
  bucket?: string;
  region?: string;
  account_id?: string;
  host?: string;
  port?: number;
  user?: string;
  url?: string;
}

export interface Destination {
  name: string;
  type: DestinationType;
  settings: DestinationSettings;
  /** The restic repository string, derived from type + settings. */
  location: string;
  password_credential: string;
  /** The credential file holding the backend's own key/secret, when it needs one. */
  backend_credential?: string;
  keep_daily: number;
  keep_weekly: number;
  keep_monthly: number;
}

/**
 * Credentials typed into a destination form. Write-only: the server
 * never returns these, and leaving them empty on an update keeps
 * whatever was already saved.
 */
export interface DestinationCredentials {
  access_key_id?: string;
  secret_access_key?: string;
  ssh_private_key?: string;
}

/** POST /api/v1/destinations request body. */
export interface CreateDestinationRequest {
  name: string;
  type: DestinationType;
  settings: DestinationSettings;
  keep_daily?: number;
  keep_weekly?: number;
  keep_monthly?: number;
  init: boolean;
  credentials?: DestinationCredentials;
}

export interface ScheduledJob {
  service: string;
  cron: string;
  enabled: boolean;
}

export interface TierStatus {
  tier: string;
  light: "green" | "amber" | "red" | "running" | "none";
  last_run_id?: number;
  last_state?: string;
  last_started_at?: string;
  last_finished_at?: string;
  last_error?: string;
  snapshot_id?: string;
  last_verified_at?: string;
  verified_age_seconds?: number;
  max_age_seconds?: number;
  stale?: boolean;
}

export interface VerificationRecord {
  method: string;
  passed: boolean;
  warning?: string;
  detail?: string;
  output?: string;
  duration_ms: number;
  recorded_at: string;
}

export interface ScheduleStatus {
  cron: string;
  enabled: boolean;
}

export interface ServiceStatus {
  service: string;
  catalog_entry: string;
  destination: string;
  schedule?: ScheduleStatus;
  tiers: TierStatus[];
  verification?: VerificationRecord[];
  freshness_age_seconds?: number;
}

export interface Snapshot {
  id: string;
  short_id: string;
  time: string;
  hostname: string;
  tags?: string[];
  paths?: string[];
}

export interface NotificationSettings {
  service: string;
  enabled: boolean;
  max_age_hours?: number;
  heartbeat_enabled: boolean;
}

export interface DetectedContainer {
  name: string;
  image: string;
}

export interface DetectSuggestion {
  service: string;
  containers: DetectedContainer[];
}

export interface DetectResult {
  containers: DetectedContainer[];
  suggestions: DetectSuggestion[];
}

export interface PreflightResult {
  restic_installed: boolean;
  restic_version?: string;
  restic_warning?: string;
  restic_error?: string;
  docker_installed: boolean;
  docker_error?: string;
}

export interface ApiErrorBody {
  error: string;
}

/** Which kind of Docker daemon a host entry points at. */
export type DockerHostKind = "local" | "remote-ssh" | "remote-tls";

export interface DockerHost {
  name: string;
  kind: DockerHostKind;
  address: string;
  /** The credential file holding this host's SSH key or TLS certs, when it needs one. */
  credential?: string;
}

/**
 * Credentials typed into a Docker host form. Write-only: the server
 * never returns these, and leaving them empty on an update keeps
 * whatever was already saved.
 */
export interface DockerHostCredentials {
  ssh_private_key?: string;
  ca_cert?: string;
  client_cert?: string;
  client_key?: string;
}

/** POST /api/v1/docker-hosts request body. */
export interface CreateDockerHostRequest {
  name: string;
  kind: DockerHostKind;
  address: string;
  credentials?: DockerHostCredentials;
  /** Save without testing the connection first. */
  skip_test?: boolean;
}

export interface DockerHostTestResult {
  server_version: string;
}

/** One Docker volume on a host, for the "docker-volume" parameter picker. */
export interface DockerVolume {
  name: string;
  driver: string;
  /** The Compose project the volume belongs to, when it has one. */
  compose_project?: string;
}

/** One container in a DockerComposeProject, for display only. */
export interface DockerComposeContainer {
  name: string;
  service: string;
  image: string;
  labels?: Record<string, string>;
}

/** One Docker Compose project on a host, for the "compose-project"
 * parameter picker. */
export interface DockerComposeProject {
  name: string;
  working_dir: string;
  config_files?: string[];
  services?: string[];
  containers?: DockerComposeContainer[];
}

/** POST /api/v1/docker-hosts response. */
export interface CreateDockerHostResponse {
  host: DockerHost;
  test?: DockerHostTestResult;
}

/** Ballast's single saved Proxmox VE connection (GET /api/v1/proxmox). */
export interface ProxmoxConnection {
  host: string;
  token_id: string;
  /** The credential file holding this connection's API token secret. */
  credential?: string;
  insecure: boolean;
}

/**
 * The token secret typed into the Proxmox connection form. Write-only:
 * the server never returns it, and leaving it empty on an update keeps
 * whatever was already saved.
 */
export interface ProxmoxCredentials {
  token_secret?: string;
}

/** PUT /api/v1/proxmox request body. */
export interface ProxmoxConnectionSetupRequest {
  host: string;
  token_id: string;
  insecure?: boolean;
  credentials?: ProxmoxCredentials;
  /** Save without testing the connection first. */
  skip_test?: boolean;
}

export interface ProxmoxTestResult {
  version: string;
}

/** PUT /api/v1/proxmox response. */
export interface ProxmoxConnectionResult {
  connection: ProxmoxConnection;
  test?: ProxmoxTestResult;
}

/** One VM or LXC container on the Proxmox host, from GET /api/v1/proxmox/guests. */
export interface ProxmoxGuest {
  node: string;
  vmid: number;
  type: "qemu" | "lxc";
  name: string;
  status: string;
}

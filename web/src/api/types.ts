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
}

export interface CatalogSummary {
  service: string;
  name: string;
  description: string;
  parameters?: CatalogParameter[];
}

export interface Service {
  name: string;
  catalog_entry: string;
  destination: string;
  params?: Record<string, string>;
  containers?: Record<string, string>;
}

export interface Destination {
  name: string;
  location: string;
  password_credential: string;
  keep_daily: number;
  keep_weekly: number;
  keep_monthly: number;
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

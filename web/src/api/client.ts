import { markSignedOut } from "./auth";
import type {
  ApiErrorBody,
  BallastHealth,
  CatalogSummary, EnvSourceProposal,
  ChannelInput,
  ChannelView,
  CreateDestinationRequest,
  CreateDockerHostRequest,
  CreateDockerHostResponse,
  Destination,
  DetectResult,
  DigestSendResult,
  DockerComposeProject,
  DockerHost,
  DockerHostTestResult,
  DockerVolume,
  ExcludePreview,
  NotificationSettings,
  PreflightResult,
  ProxmoxConnection,
  ProxmoxConnectionResult,
  ProxmoxConnectionSetupRequest,
  ProxmoxGuest,
  ProxmoxTestResult,
  RestoreProjectReport,
  ScheduledJob,
  Service,
  ServiceStatus,
  Snapshot,
} from "./types";

// ApiError carries the HTTP status so callers can special-case 401
// (bad/missing token -> prompt again) vs. other failures, without
// parsing the message text.
export class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
  ) {
    super(message);
    this.name = "ApiError";
  }
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const headers: Record<string, string> = {};
  if (body !== undefined) {
    headers["Content-Type"] = "application/json";
  }
  const res = await fetch(path, {
    method,
    headers,
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });

  if (res.status === 204) {
    return undefined as T;
  }

  const text = await res.text();
  let data: unknown = undefined;
  if (text) {
    try {
      data = JSON.parse(text);
    } catch {
      // Non-JSON body (shouldn't happen from this API); fall through
      // with data left undefined so the raw text still surfaces below.
    }
  }

  if (!res.ok) {
    const message =
      (data as ApiErrorBody | undefined)?.error || text || `${method} ${path} failed with ${res.status}`;
    if (res.status === 401) {
      // The session ended (expired, signed out, or the key was
      // rotated): fall back to the sign-in screen instead of silently
      // failing every call from here on.
      markSignedOut();
    }
    throw new ApiError(res.status, message);
  }
  return data as T;
}

const base = "/api/v1";

export const api = {
  // --- status & catalog ---
  status(service?: string, withVerify?: boolean): Promise<ServiceStatus[]> {
    const params = new URLSearchParams();
    if (service) params.set("service", service);
    if (withVerify) params.set("verify", "1");
    const qs = params.toString();
    return request("GET", `${base}/status${qs ? `?${qs}` : ""}`);
  },
  login(token: string): Promise<void> {
    return request("POST", `${base}/auth/login`, { token });
  },
  logout(): Promise<void> {
    return request("POST", `${base}/auth/logout`);
  },
  session(): Promise<{ ok: boolean; insecure_exposure: boolean }> {
    return request("GET", `${base}/auth/session`);
  },
  rotateKey(): Promise<{ token: string }> {
    return request("POST", `${base}/auth/rotate`);
  },
  signOutEverywhere(): Promise<void> {
    return request("POST", `${base}/auth/sign-out-everywhere`);
  },
  catalog(): Promise<CatalogSummary[]> {
    return request("GET", `${base}/catalog`);
  },
  readEnvSource(entry: string, dir: string): Promise<EnvSourceProposal> {
    return request("POST", `${base}/catalog/${encodeURIComponent(entry)}/env-source`, { dir });
  },
  detect(host?: string): Promise<DetectResult> {
    return request("GET", `${base}/detect${host ? `?host=${encodeURIComponent(host)}` : ""}`);
  },
  preflight(): Promise<PreflightResult> {
    return request("GET", `${base}/preflight`);
  },

  // --- services ---
  listServices(): Promise<Service[]> {
    return request("GET", `${base}/services`);
  },
  getService(name: string): Promise<Service> {
    return request("GET", `${base}/services/${encodeURIComponent(name)}`);
  },
  createService(s: Service): Promise<Service> {
    return request("POST", `${base}/services`, s);
  },
  updateService(name: string, s: Service): Promise<Service> {
    return request("PUT", `${base}/services/${encodeURIComponent(name)}`, s);
  },
  previewExcludes(name: string, excludes: string[]): Promise<ExcludePreview> {
    return request("POST", `${base}/services/${encodeURIComponent(name)}/excludes/preview`, { excludes });
  },
  deleteService(name: string): Promise<void> {
    return request("DELETE", `${base}/services/${encodeURIComponent(name)}`);
  },

  // --- backups, snapshots, restore ---
  startBackup(name: string): Promise<{ started: boolean; previous_run_interrupted: boolean }> {
    return request("POST", `${base}/services/${encodeURIComponent(name)}/backup`);
  },
  snapshots(name: string): Promise<Snapshot[]> {
    return request("GET", `${base}/services/${encodeURIComponent(name)}/snapshots`);
  },
  restore(name: string, req: { snapshot?: string; paths?: string[]; to: string }): Promise<{ restored: boolean }> {
    return request("POST", `${base}/services/${encodeURIComponent(name)}/restore`, req);
  },

  // --- full-stack restore (spec §14 step 23) ---
  listProjects(): Promise<string[]> {
    return request("GET", `${base}/projects`);
  },
  restoreProject(project: string, req: { as_of?: string; to: string }): Promise<RestoreProjectReport> {
    return request("POST", `${base}/projects/${encodeURIComponent(project)}/restore`, req);
  },

  // --- schedules ---
  getSchedule(name: string): Promise<ScheduledJob> {
    return request("GET", `${base}/services/${encodeURIComponent(name)}/schedule`);
  },
  setSchedule(name: string, cron: string): Promise<ScheduledJob> {
    return request("PUT", `${base}/services/${encodeURIComponent(name)}/schedule`, { cron });
  },
  setScheduleEnabled(name: string, enabled: boolean): Promise<{ enabled: boolean }> {
    return request("PUT", `${base}/services/${encodeURIComponent(name)}/schedule/enabled`, { enabled });
  },

  // --- trigger tokens ---
  generateTriggerToken(name: string): Promise<{ token: string }> {
    return request("POST", `${base}/services/${encodeURIComponent(name)}/trigger-token`);
  },

  // --- notification settings ---
  getNotificationSettings(name: string): Promise<NotificationSettings> {
    return request("GET", `${base}/services/${encodeURIComponent(name)}/notifications`);
  },
  setNotificationSettings(name: string, s: Omit<NotificationSettings, "service">): Promise<NotificationSettings> {
    return request("PUT", `${base}/services/${encodeURIComponent(name)}/notifications`, s);
  },

  // --- notification channels ---
  listChannels(): Promise<ChannelView[]> {
    return request("GET", `${base}/notification-channels`);
  },
  createChannel(ch: ChannelInput): Promise<ChannelView> {
    return request("POST", `${base}/notification-channels`, ch);
  },
  updateChannel(name: string, ch: ChannelInput): Promise<ChannelView> {
    return request("PUT", `${base}/notification-channels/${encodeURIComponent(name)}`, ch);
  },
  deleteChannel(name: string): Promise<void> {
    return request("DELETE", `${base}/notification-channels/${encodeURIComponent(name)}`);
  },
  testChannel(name: string): Promise<{ ok: boolean; detail?: string }> {
    return request("POST", `${base}/notification-channels/${encodeURIComponent(name)}/test`);
  },

  // --- ballast's own health & weekly digest ---
  ballastHealth(): Promise<BallastHealth> {
    return request("GET", `${base}/ballast-health`);
  },
  sendDigest(): Promise<DigestSendResult> {
    return request("POST", `${base}/digest/send`);
  },

  // --- destinations ---
  listDestinations(): Promise<Destination[]> {
    return request("GET", `${base}/destinations`);
  },
  createDestination(req: CreateDestinationRequest): Promise<Destination> {
    return request("POST", `${base}/destinations`, req);
  },
  deleteDestination(name: string): Promise<void> {
    return request("DELETE", `${base}/destinations/${encodeURIComponent(name)}`);
  },
  testDestination(name: string): Promise<{ ok: boolean }> {
    return request("POST", `${base}/destinations/${encodeURIComponent(name)}/test`);
  },
  initDestination(name: string): Promise<{ initialized: boolean }> {
    return request("POST", `${base}/destinations/${encodeURIComponent(name)}/init`);
  },

  // --- docker hosts ---
  listDockerHosts(): Promise<DockerHost[]> {
    return request("GET", `${base}/docker-hosts`);
  },
  createDockerHost(req: CreateDockerHostRequest): Promise<CreateDockerHostResponse> {
    return request("POST", `${base}/docker-hosts`, req);
  },
  deleteDockerHost(name: string): Promise<void> {
    return request("DELETE", `${base}/docker-hosts/${encodeURIComponent(name)}`);
  },
  testDockerHost(name: string): Promise<DockerHostTestResult> {
    return request("POST", `${base}/docker-hosts/${encodeURIComponent(name)}/test`);
  },
  listDockerHostVolumes(name: string): Promise<DockerVolume[]> {
    return request("GET", `${base}/docker-hosts/${encodeURIComponent(name)}/volumes`);
  },
  listDockerHostComposeProjects(name: string): Promise<DockerComposeProject[]> {
    return request("GET", `${base}/docker-hosts/${encodeURIComponent(name)}/compose-projects`);
  },

  // --- proxmox ---
  getProxmoxConnection(): Promise<ProxmoxConnection> {
    return request("GET", `${base}/proxmox`);
  },
  setupProxmoxConnection(req: ProxmoxConnectionSetupRequest): Promise<ProxmoxConnectionResult> {
    return request("PUT", `${base}/proxmox`, req);
  },
  deleteProxmoxConnection(): Promise<void> {
    return request("DELETE", `${base}/proxmox`);
  },
  testProxmoxConnection(): Promise<ProxmoxTestResult> {
    return request("POST", `${base}/proxmox/test`);
  },
  listProxmoxGuests(): Promise<ProxmoxGuest[]> {
    return request("GET", `${base}/proxmox/guests`);
  },
};

<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from "vue";
import { useRoute } from "vue-router";

import { api } from "@/api/client";
import type { CatalogSummary, Destination, DockerComposeProject, ExcludePreview, ServicePreview, DockerHost, DockerVolume, ProxmoxGuest, Retention, Service, ServiceTarget, Snapshot } from "@/api/types";
import { formatBytes, formatDateTime, formatRelative } from "@/lib/format";
import { describeServiceRetention } from "@/lib/retention";
import ListTools from "@/components/ListTools.vue";
import { COLLAPSE_SAFE_FROM, collapsesGroup, countLights, emptyQuery, filterEntries, groupEntries, needsTools, type ListEntry, type ListQuery } from "@/lib/listView";
import { worstLight, type Light } from "@/lib/status";
import TrafficLight from "@/components/TrafficLight.vue";
import { checkParam, warningsNote } from "@/lib/params";
import { askConfirm } from "@/lib/confirm";
import { revealOnError } from "@/lib/revealError";

const services = ref<Service[]>([]);

// Search and grouping, shown only once the list is long.
// The Overview links here with ?q=name to show one backup's details.
const linkedSearch = useRoute().query.q;
const query = ref<ListQuery>({ ...emptyQuery(), text: typeof linkedSearch === "string" ? linkedSearch : "" });
// Each backup's traffic light, so a long list can be sorted by what needs attention.
// A failed status read only hides the lights; the list itself still works.
const lights = ref<Record<string, Light>>({});
const listEntry = (s: Service): ListEntry => ({ name: s.name, type: s.catalog_entry, destination: s.destination, light: lights.value[s.name] });
const counts = computed(() => countLights(services.value, listEntry));
// A long healthy group stays folded until asked for, as on the Overview.
const safeOpen = ref(false);
const folded = (g: { key: string; items: unknown[] }) => collapsesGroup(g, showTools.value ? query.value : emptyQuery(), safeOpen.value);
const showTools = computed(() => needsTools(services.value.length));
const shownServices = computed(() => (showTools.value ? filterEntries(services.value, listEntry, query.value) : services.value));
const serviceGroups = computed(() =>
  groupEntries(shownServices.value, listEntry, showTools.value ? query.value.group : "none", (t) => entryName(t)),
);
const catalog = ref<CatalogSummary[]>([]);
const destinations = ref<Destination[]>([]);
const dockerHosts = ref<DockerHost[]>([]);
const loading = ref(true);
const error = ref("");
revealOnError(error);
const saved = ref("");
const saving = ref(false);
const showForm = ref(false);

const destinationByName = computed(() => {
  const map = new Map<string, Destination>();
  for (const d of destinations.value) map.set(d.name, d);
  return map;
});

/** The retention policy actually applied to a service's backups: its own override, or its destination's. */
function effectiveRetention(s: Service): Retention {
  if (s.retention) return s.retention;
  const d = destinationByName.value.get(s.destination);
  return { keep_last: d?.keep_last ?? 0, keep_daily: d?.keep_daily ?? 0, keep_weekly: d?.keep_weekly ?? 0, keep_monthly: d?.keep_monthly ?? 0 };
}

function retentionLine(s: Service): string {
  return describeServiceRetention(effectiveRetention(s), s.destination, !!s.retention);
}

// --- backups (snapshots), loaded on demand per service ---

interface SnapshotState {
  open: boolean;
  loading: boolean;
  error: string;
  loaded: boolean;
  snapshots: Snapshot[];
}

const snapshotState = reactive<Record<string, SnapshotState>>({});

function stateFor(name: string): SnapshotState {
  if (!snapshotState[name]) {
    snapshotState[name] = { open: false, loading: false, error: "", loaded: false, snapshots: [] };
  }
  return snapshotState[name];
}

async function toggleBackups(name: string) {
  const st = stateFor(name);
  st.open = !st.open;
  if (!st.open || st.loaded || st.loading) return;
  st.loading = true;
  st.error = "";
  try {
    st.snapshots = (await api.snapshots(name)).slice().sort((a, b) => (a.time < b.time ? 1 : -1));
    st.loaded = true;
  } catch (e) {
    st.error = (e as Error).message;
  } finally {
    st.loading = false;
  }
}

// --- per-service retention override editor ---

interface RetentionForm {
  keep_last: number;
  keep_daily: number;
  keep_weekly: number;
  keep_monthly: number;
}

const retentionForms = reactive<Record<string, RetentionForm>>({});
const retentionSaving = ref<Set<string>>(new Set());

function retentionFormFor(s: Service): RetentionForm {
  if (!retentionForms[s.name]) {
    const eff = effectiveRetention(s);
    retentionForms[s.name] = { keep_last: eff.keep_last ?? 0, keep_daily: eff.keep_daily, keep_weekly: eff.keep_weekly, keep_monthly: eff.keep_monthly };
  }
  return retentionForms[s.name];
}

/** The service with its main destination's keep-settings changed (targets carry it). */
function withMainRetention(s: Service, retention: Service["retention"]): Service {
  return {
    ...s,
    retention,
    targets: s.targets?.map((t, i) => (i === 0 ? { ...t, retention: retention ?? undefined } : t)),
  };
}

async function saveRetentionOverride(s: Service) {
  const form = retentionFormFor(s);
  retentionSaving.value.add(s.name);
  error.value = "";
  try {
    const updated = await api.updateService(s.name, withMainRetention(s, { ...form }));
    Object.assign(s, updated);
    saved.value = `Saved backup-keeping settings for ${s.name}.`;
  } catch (e) {
    error.value = (e as Error).message;
  } finally {
    retentionSaving.value.delete(s.name);
  }
}

// --- "skip these" editor ---

const excludeText = reactive<Record<string, string>>({});
const excludePreviews = reactive<Record<string, ExcludePreview | undefined>>({});
const excludeBusy = ref<Set<string>>(new Set());
const previews = reactive<Record<string, ServicePreview | undefined>>({});
const previewBusy = ref<Set<string>>(new Set());

async function loadPreview(s: Service) {
  previewBusy.value.add(s.name);
  error.value = "";
  try {
    previews[s.name] = await api.servicePreview(s.name);
  } catch (e) {
    error.value = (e as Error).message;
  } finally {
    previewBusy.value.delete(s.name);
  }
}

function excludeTextFor(s: Service): string {
  if (excludeText[s.name] === undefined) excludeText[s.name] = (s.excludes ?? []).join("\n");
  return excludeText[s.name];
}

function parseExcludes(text: string): string[] {
  return text
    .split("\n")
    .map((l) => l.trim())
    .filter((l) => l !== "");
}

async function previewExcludes(s: Service) {
  excludeBusy.value.add(s.name);
  error.value = "";
  try {
    excludePreviews[s.name] = await api.previewExcludes(s.name, parseExcludes(excludeTextFor(s)));
  } catch (e) {
    error.value = (e as Error).message;
  } finally {
    excludeBusy.value.delete(s.name);
  }
}

async function saveExcludes(s: Service) {
  excludeBusy.value.add(s.name);
  error.value = "";
  try {
    const updated = await api.updateService(s.name, { ...s, excludes: parseExcludes(excludeTextFor(s)) });
    Object.assign(s, updated);
    saved.value = `Saved what ${s.name} skips.`;
  } catch (e) {
    error.value = (e as Error).message;
  } finally {
    excludeBusy.value.delete(s.name);
  }
}

async function useDestinationRetention(s: Service) {
  retentionSaving.value.add(s.name);
  error.value = "";
  try {
    const updated = await api.updateService(s.name, withMainRetention(s, null));
    Object.assign(s, updated);
    delete retentionForms[s.name];
    saved.value = `${s.name} now follows ${s.destination}'s backup-keeping setting.`;
  } catch (e) {
    error.value = (e as Error).message;
  } finally {
    retentionSaving.value.delete(s.name);
  }
}

// Most people only ever have "local"; don't show a picker for a
// choice that isn't really a choice yet.
const showHostPicker = computed(() => dockerHosts.value.length > 1);

const editingName = ref<string | null>(null);
// Kept so submit() can carry a retention override forward: the edit
// form itself has no retention fields (that's handled by the
// per-service "Backups to keep" panel below), and PUT replaces the
// whole service, so submitting the edit form must not silently clear
// an override set through that panel.
const editingService = ref<Service | null>(null);
const form = reactive({
  name: "",
  catalog_entry: "",
  destination: "",
  docker_host: "local",
  project: "",
  proxmox_node: "",
  proxmox_vmid: 0,
  params: {} as Record<string, string>,
});

// "Also send to…": one entry per other destination, ticked when the
// service sends there, with an optional comma-separated list of parts.
const also = reactive<Record<string, { on: boolean; parts: string }>>({});

const otherDestinations = computed(() => destinations.value.filter((d) => d.name !== form.destination));

/** A plain-English price warning for pay-as-you-go cloud storage. */
function costHint(type: string | undefined): string {
  if (type === "b2") return "Backblaze B2 is billed by the amount stored: very roughly $6 a month per terabyte. Check their current prices.";
  if (type === "s3" || type === "r2") return "Cloud storage is billed by the amount stored (and sometimes by downloads). Check your provider's current prices before sending a large library.";
  return "";
}

function alsoFor(name: string) {
  if (!also[name]) also[name] = { on: false, parts: "" };
  return also[name];
}

function resetAlso(s: Service | null) {
  for (const k of Object.keys(also)) delete also[k];
  for (const t of (s?.targets ?? []).slice(1)) {
    also[t.destination] = { on: true, parts: (t.parts ?? []).join(", ") };
  }
}

/** The targets the form describes, main destination first. */
function buildTargets(): ServiceTarget[] {
  const main: ServiceTarget = { destination: form.destination };
  const old = editingService.value?.targets?.[0];
  if (old && old.destination === form.destination) {
    main.parts = old.parts;
    main.retention = old.retention;
  } else if (editingService.value && !editingService.value.targets) {
    main.retention = editingService.value.retention;
  }
  const out = [main];
  for (const d of otherDestinations.value) {
    const a = also[d.name];
    if (!a?.on) continue;
    const parts = a.parts.split(",").map((p) => p.trim()).filter(Boolean);
    const prev = editingService.value?.targets?.find((t) => t.destination === d.name);
    out.push({ destination: d.name, parts: parts.length ? parts : undefined, retention: prev?.retention });
  }
  return out;
}

const selectedEntry = computed(() => catalog.value.find((c) => c.service === form.catalog_entry));

function entryName(id: string): string {
  return catalog.value.find((c) => c.service === id)?.name ?? id;
}

// --- Docker volume picker (parameters with picker: "docker-volume") ---

const volumeOptions = ref<DockerVolume[]>([]);
const volumeError = ref("");
const volumeHost = ref("");

const hasVolumePicker = computed(() => (selectedEntry.value?.parameters ?? []).some((p) => p.picker === "docker-volume"));

/** Loads the volumes on form.docker_host, for the datalist of any
 * "docker-volume" parameter. Best-effort: on failure the picker just
 * falls back to a plain text box. */
async function loadVolumes() {
  if (!hasVolumePicker.value || !form.docker_host) {
    volumeOptions.value = [];
    volumeError.value = "";
    return;
  }
  const host = form.docker_host;
  volumeHost.value = host;
  volumeError.value = "";
  try {
    const vols = await api.listDockerHostVolumes(host);
    if (volumeHost.value !== host) return; // the host changed meanwhile
    volumeOptions.value = vols;
  } catch {
    if (volumeHost.value !== host) return;
    volumeOptions.value = [];
    volumeError.value = "Couldn't list volumes on this host — type the volume name.";
  }
}

function volumeLabel(v: DockerVolume): string {
  return v.compose_project ? `${v.name} (${v.compose_project})` : v.name;
}

// --- Compose project picker (parameters with picker: "compose-project") ---

const composeProjectOptions = ref<DockerComposeProject[]>([]);
const composeProjectError = ref("");
const composeProjectHost = ref("");

const hasComposeProjectPicker = computed(() =>
  (selectedEntry.value?.parameters ?? []).some((p) => p.picker === "compose-project"),
);

/** Loads the Compose projects on form.docker_host, for the datalist of
 * any "compose-project" parameter. Best-effort: on failure the picker
 * just falls back to a plain text box. */
async function loadComposeProjects() {
  if (!hasComposeProjectPicker.value || !form.docker_host) {
    composeProjectOptions.value = [];
    composeProjectError.value = "";
    return;
  }
  const host = form.docker_host;
  composeProjectHost.value = host;
  composeProjectError.value = "";
  try {
    const projects = await api.listDockerHostComposeProjects(host);
    if (composeProjectHost.value !== host) return; // the host changed meanwhile
    composeProjectOptions.value = projects;
  } catch {
    if (composeProjectHost.value !== host) return;
    composeProjectOptions.value = [];
    composeProjectError.value = "Couldn't list Compose projects on this host — type the project name.";
  }
}

watch(() => [form.catalog_entry, form.docker_host], () => {
  loadVolumes();
  loadComposeProjects();
});

// --- Proxmox guest picker (attaching a service to a VM/LXC) ---

const proxmoxGuests = ref<ProxmoxGuest[]>([]);
const proxmoxGuestsError = ref("");
const proxmoxGuestsLoaded = ref(false);

/** Loads the Proxmox host's guests once, for the "attach to a guest"
 * picker. Best-effort: on failure (most likely no Proxmox connection
 * configured yet) the picker just falls back to plain node/VMID boxes. */
async function loadProxmoxGuests() {
  if (proxmoxGuestsLoaded.value) return;
  proxmoxGuestsError.value = "";
  try {
    proxmoxGuests.value = await api.listProxmoxGuests();
    proxmoxGuestsLoaded.value = true;
  } catch (e) {
    proxmoxGuests.value = [];
    proxmoxGuestsError.value = (e as Error).message;
  }
}

function guestLabel(g: ProxmoxGuest): string {
  return `${g.name} (${g.vmid}, ${g.type === "lxc" ? "LXC" : "VM"})`;
}

function guestKey(node: string, vmid: number): string {
  return `${node}:${vmid}`;
}

const selectedGuestKey = computed({
  get: () => (form.proxmox_node && form.proxmox_vmid ? guestKey(form.proxmox_node, form.proxmox_vmid) : ""),
  set: (key: string) => {
    if (!key) {
      form.proxmox_node = "";
      form.proxmox_vmid = 0;
      return;
    }
    const g = proxmoxGuests.value.find((g) => guestKey(g.node, g.vmid) === key);
    if (g) {
      form.proxmox_node = g.node;
      form.proxmox_vmid = g.vmid;
    }
  },
});

let groupedOnce = false;
async function load() {
  loading.value = true;
  error.value = "";
  try {
    [services.value, catalog.value, destinations.value, dockerHosts.value] = await Promise.all([
      api.listServices(),
      api.catalog(),
      api.listDestinations(),
      api.listDockerHosts(),
    ]);
    api
      .status()
      .then((list) => {
        lights.value = Object.fromEntries(list.map((st) => [st.service, worstLight(st)]));
        // A long list opens with problems on top, like the Overview.
        if (!groupedOnce && needsTools(services.value.length) && query.value.group === "none") {
          groupedOnce = true;
          query.value = { ...query.value, group: "status" };
        }
      })
      .catch(() => {});
  } catch (e) {
    error.value = (e as Error).message;
  } finally {
    loading.value = false;
  }
}

function openAdd() {
  editingName.value = null;
  editingService.value = null;
  form.name = "";
  form.catalog_entry = "";
  form.destination = destinations.value[0]?.name ?? "";
  form.docker_host = "local";
  form.project = "";
  form.proxmox_node = "";
  form.proxmox_vmid = 0;
  form.params = {};
  resetAlso(null);
  saved.value = "";
  showForm.value = true;
  volumeOptions.value = [];
  volumeError.value = "";
  void loadProxmoxGuests();
}

function startEdit(s: Service) {
  editingName.value = s.name;
  editingService.value = s;
  form.name = s.name;
  form.catalog_entry = s.catalog_entry;
  form.destination = s.destination;
  form.docker_host = s.docker_host ?? "local";
  form.project = s.project ?? "";
  form.proxmox_node = s.proxmox_node ?? "";
  form.proxmox_vmid = s.proxmox_vmid ?? 0;
  form.params = { ...(s.params ?? {}) };
  resetAlso(s);
  saved.value = "";
  showForm.value = true;
  void loadVolumes();
  void loadProxmoxGuests();
}

function closeForm() {
  showForm.value = false;
  editingName.value = null;
  editingService.value = null;
}

function onEntryChange() {
  const next: Record<string, string> = {};
  for (const p of selectedEntry.value?.parameters ?? []) {
    next[p.name] = form.params[p.name] ?? p.default ?? "";
  }
  form.params = next;
  if (!editingName.value && !form.name) form.name = form.catalog_entry;
}

async function submit() {
  error.value = "";
  if (!form.catalog_entry) return void (error.value = "Choose what you want to back up.");
  if (!form.name) return void (error.value = "Give it a name.");
  if (!form.destination) return void (error.value = "Choose where the backups should go.");
  for (const p of selectedEntry.value?.parameters ?? []) {
    const problem = checkParam(p, form.params[p.name]);
    if (problem) return void (error.value = problem);
  }
  saving.value = true;
  const payload: Service = {
    name: form.name,
    catalog_entry: form.catalog_entry,
    destination: form.destination,
    docker_host: form.docker_host,
    project: form.project || undefined,
    proxmox_node: form.proxmox_node || undefined,
    proxmox_vmid: form.proxmox_vmid || undefined,
    params: { ...form.params },
    retention: editingService.value?.retention,
    targets: buildTargets(),
  };
  try {
    if (editingName.value) {
      const res = await api.updateService(editingName.value, payload);
      saved.value = [`Saved changes to ${form.name}.`, warningsNote(res.warnings)].filter(Boolean).join(" ");
    } else {
      const res = await api.createService(payload);
      saved.value = `Added ${form.name}. Set when it runs on the "When it runs" page, or back it up now from the Overview.` + (warningsNote(res.warnings) ? ` ${warningsNote(res.warnings)}` : "");
    }
    closeForm();
    await load();
  } catch (e) {
    error.value = (e as Error).message;
  } finally {
    saving.value = false;
  }
}

async function makeMain(name: string, destination: string) {
  error.value = "";
  try {
    await api.setPrimaryTarget(name, destination);
    saved.value = `${destination} is now the main destination for ${name}. Its backup history is kept.`;
    await load();
  } catch (e) {
    error.value = (e as Error).message;
  }
}

async function remove(name: string) {
  if (!(await askConfirm(`Stop backing up "${name}"? Backups already made are kept; Ballast just stops making new ones.`, "Stop backing up"))) {
    return;
  }
  error.value = "";
  try {
    await api.deleteService(name);
    saved.value = `Stopped backing up ${name}.`;
    await load();
  } catch (e) {
    error.value = (e as Error).message;
  }
}

onMounted(load);
</script>

<template>
  <header class="page-head">
    <h2>Backups</h2>
    <p class="lede">Each entry is one app or folder Ballast backs up. The command line calls these "services".</p>
  </header>

  <p v-if="error" class="notice notice--error" role="alert">{{ error }}</p>
  <p v-if="saved" class="notice notice--ok" role="status">{{ saved }}</p>
  <p v-if="loading" class="loading">Loading…</p>

  <template v-else>
    <div v-if="!services.length && !showForm" class="empty">
      <p>Nothing is being backed up yet.</p>
      <div class="actions" style="justify-content: center">
        <router-link class="btn btn-primary" to="/wizard">Set up with the guide</router-link>
        <button class="btn" type="button" @click="openAdd">Add one here</button>
      </div>
    </div>

    <ListTools
      v-if="showTools"
      v-model="query"
      :counts="Object.keys(lights).length ? counts : undefined"
      :groups="Object.keys(lights).length ? ['none', 'status', 'type', 'destination'] : ['none', 'type', 'destination']"
      :shown="shownServices.length"
      :total="services.length"
      label="backups"
    />
    <p v-if="showTools && !shownServices.length" class="empty">
      Nothing matches. <button class="btn btn-quiet" type="button" @click="query = emptyQuery()">Show everything</button>
    </p>

    <section v-for="g in services.length ? serviceGroups : []" :key="g.key" class="group" :aria-label="g.label || undefined">
    <h3 v-if="g.label" class="group-head">{{ g.label }} <span class="muted">{{ g.items.length }}</span></h3>
    <p v-if="folded(g)" class="group-folded">
      <button class="btn btn-quiet" type="button" @click="safeOpen = true">Show all {{ g.items.length }} safe backups</button>
    </p>
    <button v-else-if="g.key === 'green' && safeOpen && g.items.length > COLLAPSE_SAFE_FROM" class="btn btn-quiet btn-small group-fold" type="button" @click="safeOpen = false">Hide safe backups</button>
    <ul v-if="g.items.length && !folded(g)" class="list">
      <li v-for="s in g.items" :key="s.name">
        <div class="item-head">
          <div>
            <div class="item-title">{{ s.name }}</div>
            <div class="item-sub">
              {{ entryName(s.catalog_entry) }}, stored in {{ s.destination }}
              <span v-if="s.project"> · project: {{ s.project }}</span>
              <span v-if="s.proxmox_vmid"> · Proxmox guest {{ s.proxmox_vmid }}</span>
            </div>
          </div>
          <div class="actions">
            <TrafficLight v-if="lights[s.name]" :light="lights[s.name]" />
            <button class="btn" type="button" @click="toggleBackups(s.name)">
              {{ stateFor(s.name).open ? "Hide backups" : "Backups" }}
            </button>
            <button class="btn" type="button" @click="startEdit(s)">Edit</button>
            <button class="btn btn-quiet btn-danger" type="button" @click="remove(s.name)">Stop backing up</button>
          </div>
        </div>

        <p class="item-sub">{{ retentionLine(s) }}</p>
        <p v-for="t in (s.targets ?? []).slice(1)" :key="t.destination" class="item-sub">
          Also sends to {{ t.destination }}{{ t.parts?.length ? " (only " + t.parts.join(", ") + ")" : "" }}
          <button class="btn btn-quiet" type="button" @click="makeMain(s.name, t.destination)">Make main</button>
        </p>

        <div v-if="stateFor(s.name).open" class="panel" style="margin-top: 0.6rem">
          <p v-if="stateFor(s.name).loading" class="loading">Loading backups…</p>
          <p v-else-if="stateFor(s.name).error" class="notice notice--error" role="alert">{{ stateFor(s.name).error }}</p>
          <p v-else-if="!stateFor(s.name).snapshots.length">No backups yet.</p>
          <ul v-else class="list">
            <li v-for="(snap, i) in stateFor(s.name).snapshots" :key="snap.id">
              {{ formatDateTime(snap.time) }} ({{ formatRelative(snap.time) }})<span v-if="i === 0"> — most recent</span>
              <details class="advanced" style="display: inline-block; margin-left: 0.5rem">
                <summary>Details</summary>
                <div class="advanced-body">
                  <dl class="kv">
                    <dt>ID</dt>
                    <dd><code>{{ snap.short_id }}</code></dd>
                    <dt v-if="snap.paths?.length">Paths</dt>
                    <dd v-if="snap.paths?.length">{{ snap.paths.join(", ") }}</dd>
                  </dl>
                </div>
              </details>
            </li>
          </ul>
        </div>

        <details class="advanced">
          <summary>Details</summary>
          <div class="advanced-body">
            <dl v-if="s.params && Object.keys(s.params).length" class="kv">
              <dt>Catalog entry</dt>
              <dd><code>{{ s.catalog_entry }}</code></dd>
              <template v-for="(v, k) in s.params" :key="k">
                <dt>{{ k }}</dt>
                <dd>{{ v }}</dd>
              </template>
            </dl>

            <div class="field-label">Backups to keep</div>
            <div class="inline-fields">
              <div class="field">
                <label :for="`ret-last-${s.name}`" class="hint">Newest (any age)</label>
                <input :id="`ret-last-${s.name}`" v-model.number="retentionFormFor(s).keep_last" type="number" min="0" />
              </div>
              <div class="field">
                <label :for="`ret-daily-${s.name}`" class="hint">Daily</label>
                <input :id="`ret-daily-${s.name}`" v-model.number="retentionFormFor(s).keep_daily" type="number" min="0" />
              </div>
              <div class="field">
                <label :for="`ret-weekly-${s.name}`" class="hint">Weekly</label>
                <input :id="`ret-weekly-${s.name}`" v-model.number="retentionFormFor(s).keep_weekly" type="number" min="0" />
              </div>
              <div class="field">
                <label :for="`ret-monthly-${s.name}`" class="hint">Monthly</label>
                <input :id="`ret-monthly-${s.name}`" v-model.number="retentionFormFor(s).keep_monthly" type="number" min="0" />
              </div>
            </div>
            <div class="actions">
              <button
                class="btn"
                type="button"
                :disabled="retentionSaving.has(s.name)"
                @click="saveRetentionOverride(s)"
              >
                {{ retentionSaving.has(s.name) ? "Saving…" : "Save" }}
              </button>
              <button
                v-if="s.retention"
                class="btn btn-quiet"
                type="button"
                :disabled="retentionSaving.has(s.name)"
                @click="useDestinationRetention(s)"
              >
                Use {{ s.destination }}'s setting
              </button>
            </div>
          </div>
        </details>

        <details class="advanced">
          <summary>What would a backup do?</summary>
          <div class="advanced-body">
            <p class="hint">A look ahead. Nothing is backed up, stopped or changed.</p>
            <div class="actions">
              <button class="btn btn-quiet" type="button" :disabled="previewBusy.has(s.name)" @click="loadPreview(s)">
                Show me
              </button>
            </div>
            <div v-if="previews[s.name]" class="hint">
              <p v-if="previews[s.name]!.stop_containers.length">
                Stops {{ previews[s.name]!.stop_containers.join(", ") }} while the data is copied<template
                  v-if="previews[s.name]!.stopped_seconds !== undefined"
                  >; last time that took about {{ previews[s.name]!.stopped_seconds }} seconds</template
                >.
              </p>
              <p v-else>Nothing is stopped; the service keeps running.</p>
              <p v-if="previews[s.name]!.estimated_bytes !== undefined">
                About {{ formatBytes(previews[s.name]!.estimated_bytes!) }} of folders on this machine.
              </p>
              <ul>
                <li v-for="p in previews[s.name]!.parts" :key="p.name">
                  <strong>{{ p.name }}</strong>: {{ p.what }}. Goes to {{ p.goes.length ? p.goes.join(", ") : "nowhere" }}.
                </li>
              </ul>
              <ul>
                <li v-for="t in previews[s.name]!.targets" :key="t.destination">
                  <strong>{{ t.destination }}</strong
                  ><template v-if="t.secondary"> (second copy)</template>: keeps {{ t.keeps }}. {{ t.note }}
                </li>
              </ul>
              <p v-if="previews[s.name]!.excludes.length">Skipped: {{ previews[s.name]!.excludes.join(", ") }}</p>
              <p v-for="n in previews[s.name]!.notes" :key="n">{{ n }}</p>
            </div>
          </div>
        </details>

        <details class="advanced">
          <summary>Skip these files and folders</summary>
          <div class="advanced-body">
            <div class="field">
              <label :for="`skip-${s.name}`">One per line, for example <code>*.log</code> or <code>node_modules</code></label>
              <textarea
                :id="`skip-${s.name}`"
                :value="excludeTextFor(s)"
                rows="4"
                spellcheck="false"
                placeholder="*.log"
                @input="excludeText[s.name] = ($event.target as HTMLTextAreaElement).value"
              ></textarea>
              <p class="hint">A name matches anywhere inside the folder. Start with / to match from the top only.</p>
            </div>
            <div class="actions">
              <button class="btn btn-quiet" type="button" :disabled="excludeBusy.has(s.name)" @click="previewExcludes(s)">
                Show what would be skipped
              </button>
              <button class="btn" type="button" :disabled="excludeBusy.has(s.name)" @click="saveExcludes(s)">Save</button>
            </div>
            <div v-if="excludePreviews[s.name]" class="hint">
              <p>
                Would skip {{ excludePreviews[s.name]!.skipped_count }} item(s),
                {{ formatBytes(excludePreviews[s.name]!.skipped_bytes) }}; {{ formatBytes(excludePreviews[s.name]!.kept_bytes) }} would
                still be backed up.
              </p>
              <ul>
                <li v-for="it in excludePreviews[s.name]!.skipped" :key="it.path">
                  <code>{{ it.path }}</code> ({{ formatBytes(it.bytes) }})
                </li>
              </ul>
              <p v-if="excludePreviews[s.name]!.note">{{ excludePreviews[s.name]!.note }}</p>
            </div>
          </div>
        </details>
      </li>
    </ul>
    </section>

    <button v-if="services.length && !showForm" class="btn" type="button" @click="openAdd">Add another</button>

    <form v-if="showForm" class="panel" @submit.prevent="submit">
      <h3>{{ editingName ? `Edit ${editingName}` : "Back up something new" }}</h3>

      <div class="field">
        <label for="svc-entry">What is it?</label>
        <select id="svc-entry" v-model="form.catalog_entry" @change="onEntryChange">
          <option value="" disabled>Choose…</option>
          <option v-for="c in catalog" :key="c.service" :value="c.service">{{ c.name }}</option>
        </select>
        <p v-if="selectedEntry" class="hint">{{ selectedEntry.description }}</p>
      </div>

      <template v-if="selectedEntry">
        <div class="field">
          <label for="svc-name">Name</label>
          <input id="svc-name" v-model="form.name" :disabled="!!editingName" />
          <p class="hint">Lowercase letters, numbers and dashes, e.g. <code>photos</code>.</p>
        </div>
        <div v-for="p in selectedEntry.parameters ?? []" :key="p.name" class="field">
          <label :for="`param-${p.name}`">{{ p.prompt || p.name }}<span v-if="!p.required" class="muted"> (optional)</span></label>
          <template v-if="p.picker === 'docker-volume'">
            <input :id="`param-${p.name}`" v-model="form.params[p.name]" list="docker-volume-options" autocomplete="off" />
            <p v-if="volumeError" class="hint">{{ volumeError }}</p>
          </template>
          <template v-else-if="p.picker === 'compose-project'">
            <input :id="`param-${p.name}`" v-model="form.params[p.name]" list="compose-project-options" autocomplete="off" />
            <p v-if="composeProjectError" class="hint">{{ composeProjectError }}</p>
          </template>
          <input v-else :id="`param-${p.name}`" v-model="form.params[p.name]" />
        </div>
        <datalist v-if="hasVolumePicker" id="docker-volume-options">
          <option v-for="v in volumeOptions" :key="v.name" :value="v.name">{{ volumeLabel(v) }}</option>
        </datalist>
        <datalist v-if="hasComposeProjectPicker" id="compose-project-options">
          <option v-for="cp in composeProjectOptions" :key="cp.name" :value="cp.name">{{ cp.name }}</option>
        </datalist>
        <div v-if="showHostPicker" class="field">
          <label for="svc-host">Docker host</label>
          <select id="svc-host" v-model="form.docker_host">
            <option v-for="h in dockerHosts" :key="h.name" :value="h.name">{{ h.name }}</option>
          </select>
          <p class="hint">
            Which machine runs this container. Add more machines on
            <router-link to="/docker-hosts">Docker hosts</router-link>.
          </p>
        </div>
        <div class="field">
          <label for="svc-dest">Where should backups go?</label>
          <select id="svc-dest" v-model="form.destination">
            <option value="" disabled>Choose…</option>
            <option v-for="d in destinations" :key="d.name" :value="d.name">{{ d.name }} ({{ d.location }})</option>
          </select>
          <p v-if="!destinations.length" class="hint">
            You haven't set up anywhere to keep backups yet. Add a place on
            <router-link to="/destinations">Where backups go</router-link> first.
          </p>
        </div>

        <div v-if="otherDestinations.length" class="field">
          <label>Also send to…</label>
          <p class="hint">
            One backup run makes the backup once and sends it everywhere you tick. Each place is checked on
            its own, so a problem at one never hides how the other did.
          </p>
          <div v-for="d in otherDestinations" :key="d.name" class="also-row">
            <label class="check">
              <input v-model="alsoFor(d.name).on" type="checkbox" />
              {{ d.name }} ({{ d.location }})
            </label>
            <input
              v-if="alsoFor(d.name).on"
              v-model="alsoFor(d.name).parts"
              :aria-label="`Which parts go to ${d.name}`"
              placeholder="Everything — or list parts, e.g. library"
            />
            <p v-if="costHint(d.type)" class="hint" style="flex-basis: 100%">{{ costHint(d.type) }}</p>
          </div>
        </div>

        <details class="advanced">
          <summary>Advanced: part of a bigger stack?</summary>
          <div class="advanced-body">
            <div class="field">
              <label for="svc-project">Project name</label>
              <input id="svc-project" v-model="form.project" placeholder="e.g. myapp" />
              <p class="hint">
                If this backs up one part of a Docker Compose stack (its config, a volume, a database — each added as
                its own service), give them all the same project name here. You'll then be able to restore the
                whole stack together from <router-link to="/restore">Restore</router-link>.
              </p>
            </div>
          </div>
        </details>

        <details class="advanced">
          <summary>Advanced: attach to a Proxmox guest?</summary>
          <div class="advanced-body">
            <p class="hint">
              If this service runs inside a Proxmox VM or LXC container, you can note which one here. This doesn't
              change what a backup does yet — it's inventory for whole-guest backups, coming later.
            </p>
            <template v-if="proxmoxGuestsLoaded && proxmoxGuests.length">
              <div class="field">
                <label for="svc-proxmox-guest">Guest</label>
                <select id="svc-proxmox-guest" v-model="selectedGuestKey">
                  <option value="">Not attached to a guest</option>
                  <option v-for="g in proxmoxGuests" :key="guestKey(g.node, g.vmid)" :value="guestKey(g.node, g.vmid)">
                    {{ guestLabel(g) }}
                  </option>
                </select>
              </div>
            </template>
            <template v-else>
              <p v-if="proxmoxGuestsError" class="hint">
                No Proxmox connection configured yet — set one up on the <router-link to="/proxmox">Proxmox</router-link> page.
              </p>
              <div class="inline-fields">
                <div class="field">
                  <label for="svc-proxmox-node" class="hint">Node</label>
                  <input id="svc-proxmox-node" v-model="form.proxmox_node" placeholder="e.g. pve1" autocomplete="off" />
                </div>
                <div class="field">
                  <label for="svc-proxmox-vmid" class="hint">VMID</label>
                  <input id="svc-proxmox-vmid" v-model.number="form.proxmox_vmid" type="number" min="0" />
                </div>
              </div>
            </template>
          </div>
        </details>
      </template>

      <div class="actions">
        <button class="btn btn-primary" type="submit" :disabled="saving">
          {{ saving ? "Saving…" : editingName ? "Save changes" : "Add" }}
        </button>
        <button class="btn btn-quiet" type="button" @click="closeForm">Cancel</button>
      </div>
    </form>
  </template>
</template>

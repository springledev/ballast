<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from "vue";

import { api } from "@/api/client";
import type { CatalogSummary, Destination, DockerComposeProject, DockerHost, DockerVolume, Retention, Service, Snapshot } from "@/api/types";
import { formatDateTime, formatRelative } from "@/lib/format";
import { describeServiceRetention } from "@/lib/retention";

const services = ref<Service[]>([]);
const catalog = ref<CatalogSummary[]>([]);
const destinations = ref<Destination[]>([]);
const dockerHosts = ref<DockerHost[]>([]);
const loading = ref(true);
const error = ref("");
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
  return { keep_daily: d?.keep_daily ?? 0, keep_weekly: d?.keep_weekly ?? 0, keep_monthly: d?.keep_monthly ?? 0 };
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
  keep_daily: number;
  keep_weekly: number;
  keep_monthly: number;
}

const retentionForms = reactive<Record<string, RetentionForm>>({});
const retentionSaving = ref<Set<string>>(new Set());

function retentionFormFor(s: Service): RetentionForm {
  if (!retentionForms[s.name]) {
    const eff = effectiveRetention(s);
    retentionForms[s.name] = { keep_daily: eff.keep_daily, keep_weekly: eff.keep_weekly, keep_monthly: eff.keep_monthly };
  }
  return retentionForms[s.name];
}

async function saveRetentionOverride(s: Service) {
  const form = retentionFormFor(s);
  retentionSaving.value.add(s.name);
  error.value = "";
  try {
    const updated = await api.updateService(s.name, { ...s, retention: { ...form } });
    Object.assign(s, updated);
    saved.value = `Saved backup-keeping settings for ${s.name}.`;
  } catch (e) {
    error.value = (e as Error).message;
  } finally {
    retentionSaving.value.delete(s.name);
  }
}

async function useDestinationRetention(s: Service) {
  retentionSaving.value.add(s.name);
  error.value = "";
  try {
    const updated = await api.updateService(s.name, { ...s, retention: null });
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
  params: {} as Record<string, string>,
});

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
  form.params = {};
  saved.value = "";
  showForm.value = true;
  volumeOptions.value = [];
  volumeError.value = "";
}

function startEdit(s: Service) {
  editingName.value = s.name;
  editingService.value = s;
  form.name = s.name;
  form.catalog_entry = s.catalog_entry;
  form.destination = s.destination;
  form.docker_host = s.docker_host ?? "local";
  form.project = s.project ?? "";
  form.params = { ...(s.params ?? {}) };
  saved.value = "";
  showForm.value = true;
  void loadVolumes();
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
    if (p.required && !form.params[p.name]) return void (error.value = `Fill in "${p.prompt || p.name}".`);
  }
  saving.value = true;
  const payload: Service = {
    name: form.name,
    catalog_entry: form.catalog_entry,
    destination: form.destination,
    docker_host: form.docker_host,
    project: form.project || undefined,
    params: { ...form.params },
    retention: editingService.value?.retention,
  };
  try {
    if (editingName.value) {
      await api.updateService(editingName.value, payload);
      saved.value = `Saved changes to ${form.name}.`;
    } else {
      await api.createService(payload);
      saved.value = `Added ${form.name}. Set when it runs on the "When it runs" page, or back it up now from the Overview.`;
    }
    closeForm();
    await load();
  } catch (e) {
    error.value = (e as Error).message;
  } finally {
    saving.value = false;
  }
}

async function remove(name: string) {
  if (!confirm(`Stop backing up "${name}"? Backups already made are kept; Ballast just stops making new ones.`)) {
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
  <h2>Backups</h2>
  <p class="lede">Each entry is one app or folder Ballast backs up. The command line calls these "services".</p>

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

    <ul v-if="services.length" class="list">
      <li v-for="s in services" :key="s.name">
        <div class="item-head">
          <div>
            <div class="item-title">{{ s.name }}</div>
            <div class="item-sub">
              {{ entryName(s.catalog_entry) }}, stored in {{ s.destination }}
              <span v-if="s.project"> · project: {{ s.project }}</span>
            </div>
          </div>
          <div class="actions">
            <button class="btn" type="button" @click="toggleBackups(s.name)">
              {{ stateFor(s.name).open ? "Hide backups" : "Backups" }}
            </button>
            <button class="btn" type="button" @click="startEdit(s)">Edit</button>
            <button class="btn btn-quiet btn-danger" type="button" @click="remove(s.name)">Stop backing up</button>
          </div>
        </div>

        <p class="item-sub">{{ retentionLine(s) }}</p>

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
      </li>
    </ul>

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

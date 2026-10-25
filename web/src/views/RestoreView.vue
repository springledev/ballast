<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from "vue";

import { api } from "@/api/client";
import type { RestoreProjectReport, Service, Snapshot } from "@/api/types";
import { formatDateTime, formatRelative } from "@/lib/format";
import { groupEntries, matchesText, needsTools } from "@/lib/listView";

const services = ref<Service[]>([]);
const selectedService = ref("");
const snapshots = ref<Snapshot[]>([]);
const loadingSnapshots = ref(false);
const error = ref("");
const success = ref("");
const restoring = ref(false);

const form = reactive({ from: "", snapshot: "", paths: "", to: "" });

// Where the chosen service keeps backups: its main destination first.
// Only offered when there is more than one place to restore from.
const serviceDestinations = computed(() => {
  const s = services.value.find((x) => x.name === selectedService.value);
  if (!s) return [];
  const names = (s.targets ?? []).map((t) => t.destination);
  return names.length ? names : s.destination ? [s.destination] : [];
});

// Newest first, so the default choice is the most recent backup.
// A long list of services is split by kind so the picker stays usable.
const serviceFilter = ref("");
const serviceGroups = computed(() =>
  groupEntries(
    [...services.value]
      .filter(
        (s) =>
          s.name === selectedService.value ||
          matchesText({ name: s.name, type: s.catalog_entry, destination: s.destination }, serviceFilter.value),
      )
      .sort((a, b) => a.name.localeCompare(b.name)),
    (s) => ({ name: s.name, type: s.catalog_entry, destination: s.destination }),
    needsTools(services.value.length) ? "type" : "none",
  ),
);

const sorted = computed(() => [...snapshots.value].sort((a, b) => b.time.localeCompare(a.time)));

// --- full-stack restore (a project's compose config + volumes + DB dumps together, spec §14 step 23) ---

const projects = ref<string[]>([]);
const projectForm = reactive({ project: "", from: "", asOf: "", to: "" });
const allDestinations = ref<string[]>([]);
const projectError = ref("");
const projectReport = ref<RestoreProjectReport | null>(null);
const projectRestoring = ref(false);

async function restoreProject() {
  projectError.value = "";
  projectReport.value = null;
  if (!projectForm.to.startsWith("/")) {
    projectError.value = "Enter a full folder path starting with /, for example /tmp/restored.";
    return;
  }
  projectRestoring.value = true;
  try {
    // The API takes a full RFC3339 timestamp; a plain date input is
    // extended to midnight UTC so "as of this date" reads naturally.
    const asOf = projectForm.asOf ? `${projectForm.asOf}T00:00:00Z` : undefined;
    projectReport.value = await api.restoreProject(projectForm.project, { from: projectForm.from || undefined, as_of: asOf, to: projectForm.to });
  } catch (e) {
    projectError.value = (e as Error).message;
  } finally {
    projectRestoring.value = false;
  }
}

onMounted(async () => {
  try {
    services.value = await api.listServices();
    if (services.value.length === 1) selectedService.value = services.value[0].name;
  } catch (e) {
    error.value = (e as Error).message;
  }
  try {
    allDestinations.value = (await api.listDestinations()).map((d) => d.name);
  } catch {
    // Best-effort: the stack restore just won't offer a destination choice.
  }
  try {
    projects.value = await api.listProjects();
    if (projects.value.length === 1) projectForm.project = projects.value[0];
  } catch {
    // Best-effort: the full-stack restore panel just won't offer to prefill a project.
  }
});

// One watcher for both inputs, so a service change fetches once. Changing the
// service also resets the copy to the main one; that reset re-runs this
// watcher, which then does the fetch.
watch([selectedService, () => form.from], ([name, from], [oldName]) => {
  if (name !== oldName) {
    success.value = "";
    if (name) form.to = form.to || `/tmp/ballast-restore/${name}`;
    if (from) {
      form.from = "";
      return;
    }
  }
  loadSnapshots();
});

// Each call gets a number. Only the newest call may write the list, so a slow
// answer for an earlier service or copy cannot overwrite the current one.
let snapshotRequest = 0;

async function loadSnapshots() {
  const request = ++snapshotRequest;
  const name = selectedService.value;
  snapshots.value = [];
  form.snapshot = "";
  error.value = "";
  if (!name) {
    loadingSnapshots.value = false;
    return;
  }
  loadingSnapshots.value = true;
  try {
    const list = await api.snapshots(name, form.from || undefined);
    if (request !== snapshotRequest) return;
    snapshots.value = list;
    form.snapshot = sorted.value[0]?.id ?? "";
  } catch (e) {
    if (request !== snapshotRequest) return;
    error.value = (e as Error).message;
  } finally {
    if (request === snapshotRequest) loadingSnapshots.value = false;
  }
}

async function restore() {
  error.value = "";
  success.value = "";
  if (!form.to.startsWith("/")) {
    error.value = "Enter a full folder path starting with /, for example /tmp/restored.";
    return;
  }
  restoring.value = true;
  try {
    const paths = form.paths
      .split("\n")
      .map((p) => p.trim())
      .filter(Boolean);
    await api.restore(selectedService.value, {
      from: form.from || undefined,
      snapshot: form.snapshot || undefined,
      paths: paths.length ? paths : undefined,
      to: form.to,
    });
    success.value = form.to;
  } catch (e) {
    error.value = (e as Error).message;
  } finally {
    restoring.value = false;
  }
}
</script>

<template>
  <header class="page-head">
    <h2>Restore files</h2>
    <p class="lede">
      Get files back from a backup. Ballast never overwrites your live data: it restores into a separate folder, and you
      copy back what you need once you've looked.
    </p>
  </header>

  <p v-if="error" class="notice notice--error" role="alert">{{ error }}</p>
  <div v-if="success" class="notice notice--ok" role="status">
    <p>Restored into <code>{{ success }}</code> on the Ballast server.</p>
  </div>

  <form class="panel" @submit.prevent="restore">
    <div class="field">
      <label for="restore-service">What do you want to restore?</label>
      <input
        v-if="needsTools(services.length)"
        v-model="serviceFilter"
        type="search"
        class="restore-filter"
        placeholder="Type to narrow the list…"
        aria-label="Narrow the list of backups"
        autocomplete="off"
      />
      <select id="restore-service" v-model="selectedService">
        <option value="" disabled>Choose…</option>
        <template v-for="g in serviceGroups" :key="g.key">
          <optgroup v-if="g.label" :label="g.label">
            <option v-for="s in g.items" :key="s.name" :value="s.name">{{ s.name }}</option>
          </optgroup>
          <template v-else>
            <option v-for="s in g.items" :key="s.name" :value="s.name">{{ s.name }}</option>
          </template>
        </template>
      </select>
    </div>

    <div v-if="selectedService && serviceDestinations.length > 1" class="field">
      <label for="restore-from">Restore from which copy?</label>
      <select id="restore-from" v-model="form.from">
        <option value="">{{ serviceDestinations[0] }} (main copy)</option>
        <option v-for="d in serviceDestinations.slice(1)" :key="d" :value="d">{{ d }}</option>
      </select>
      <p class="hint">Restoring from the off-site copy is a good way to prove it works.</p>
    </div>

    <template v-if="selectedService">
      <p v-if="loadingSnapshots" class="loading">Looking for backups…</p>
      <p v-else-if="!snapshots.length" class="notice notice--warn">
        There are no backups of {{ selectedService }} yet. Run one from the Overview first.
      </p>
      <template v-else>
        <div class="field">
          <label for="restore-snapshot">From which backup?</label>
          <select id="restore-snapshot" v-model="form.snapshot">
            <option v-for="(snap, i) in sorted" :key="snap.id" :value="snap.id">
              {{ formatDateTime(snap.time) }} ({{ i === 0 ? "most recent" : formatRelative(snap.time) }})
            </option>
          </select>
        </div>
        <div class="field">
          <label for="restore-to">Put the files in</label>
          <input id="restore-to" v-model="form.to" />
          <p class="hint">A new or empty folder on the Ballast server.</p>
        </div>

        <details class="advanced">
          <summary>Advanced: restore only some files, backup IDs</summary>
          <div class="advanced-body">
            <div class="field">
              <label for="restore-paths">Only these paths (one per line)</label>
              <textarea id="restore-paths" v-model="form.paths" rows="3" placeholder="Leave blank to restore everything"></textarea>
            </div>
            <div class="table-scroll">
              <table class="stack">
                <thead>
                  <tr>
                    <th>Snapshot</th>
                    <th>Taken</th>
                    <th>Host</th>
                    <th>Paths</th>
                  </tr>
                </thead>
                <tbody>
                  <tr v-for="snap in sorted" :key="snap.id">
                    <td data-label="Snapshot"><code>{{ snap.short_id }}</code></td>
                    <td data-label="Taken">{{ formatDateTime(snap.time) }}</td>
                    <td data-label="Host">{{ snap.hostname }}</td>
                    <td data-label="Paths">{{ (snap.paths ?? []).join(", ") }}</td>
                  </tr>
                </tbody>
              </table>
            </div>
          </div>
        </details>

        <div class="actions" style="margin-top: 1rem">
          <button class="btn btn-primary" type="submit" :disabled="restoring">
            {{ restoring ? "Restoring…" : "Restore" }}
          </button>
        </div>
      </template>
    </template>
  </form>

  <template v-if="projects.length">
    <h2 style="margin-top: 2rem">Restore a whole stack</h2>
    <p class="lede">
      If you gave several services the same "project" (Services → Advanced), you can restore its Compose config,
      volumes and database dump together, in the right order: config first, then volumes, then the database.
    </p>

    <p v-if="projectError" class="notice notice--error" role="alert">{{ projectError }}</p>

    <form class="panel" @submit.prevent="restoreProject">
      <div class="field">
        <label for="restore-project">Which project?</label>
        <select id="restore-project" v-model="projectForm.project">
          <option value="" disabled>Choose…</option>
          <option v-for="p in projects" :key="p" :value="p">{{ p }}</option>
        </select>
      </div>
      <div v-if="allDestinations.length > 1" class="field">
        <label for="restore-project-from">Restore from which destination?</label>
        <select id="restore-project-from" v-model="projectForm.from">
          <option value="">Each service's main destination</option>
          <option v-for="d in allDestinations" :key="d" :value="d">{{ d }}</option>
        </select>
        <p class="hint">A service that doesn't back up to the one you pick is skipped and reported.</p>
      </div>
      <div class="field">
        <label for="restore-project-to">Put the config and dump files in</label>
        <input id="restore-project-to" v-model="projectForm.to" />
        <p class="hint">A new or empty folder on the Ballast server. Volumes are restored into new Docker volumes instead.</p>
      </div>
      <details class="advanced">
        <summary>Advanced: restore as of a particular date</summary>
        <div class="advanced-body">
          <div class="field">
            <label for="restore-project-asof">As of</label>
            <input id="restore-project-asof" v-model="projectForm.asOf" type="date" />
            <p class="hint">Leave blank to use each service's most recent backup.</p>
          </div>
        </div>
      </details>
      <div class="actions" style="margin-top: 1rem">
        <button class="btn btn-primary" type="submit" :disabled="projectRestoring || !projectForm.project">
          {{ projectRestoring ? "Restoring…" : "Restore stack" }}
        </button>
      </div>
    </form>

    <div v-if="projectReport" class="panel" style="margin-top: 1rem">
      <h3>Results for {{ projectReport.project }}</h3>
      <ul class="list">
        <li v-for="r in projectReport.results" :key="r.service">
          <div class="item-title">{{ r.service }} <span class="muted">({{ r.kind }})</span></div>
          <p v-if="r.error" class="notice notice--error" role="alert">{{ r.error }}</p>
          <template v-else>
            <p>Restored snapshot <code>{{ r.snapshot_id }}</code> to <code>{{ r.restored_to }}</code>.</p>
            <p v-if="r.volumes?.length">
              New Docker volume(s): <code>{{ r.volumes.join(", ") }}</code>
            </p>
          </template>
        </li>
      </ul>
    </div>
  </template>
</template>

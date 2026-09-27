<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from "vue";

import { api } from "@/api/client";
import type { Service, Snapshot } from "@/api/types";
import { formatDateTime, formatRelative } from "@/lib/format";

const services = ref<Service[]>([]);
const selectedService = ref("");
const snapshots = ref<Snapshot[]>([]);
const loadingSnapshots = ref(false);
const error = ref("");
const success = ref("");
const restoring = ref(false);

const form = reactive({ snapshot: "", paths: "", to: "" });

// Newest first, so the default choice is the most recent backup.
const sorted = computed(() => [...snapshots.value].sort((a, b) => b.time.localeCompare(a.time)));

onMounted(async () => {
  try {
    services.value = await api.listServices();
    if (services.value.length === 1) selectedService.value = services.value[0].name;
  } catch (e) {
    error.value = (e as Error).message;
  }
});

watch(selectedService, async (name) => {
  snapshots.value = [];
  form.snapshot = "";
  success.value = "";
  if (!name) return;
  loadingSnapshots.value = true;
  error.value = "";
  try {
    snapshots.value = await api.snapshots(name);
    form.snapshot = sorted.value[0]?.id ?? "";
    form.to = form.to || `/tmp/ballast-restore/${name}`;
  } catch (e) {
    error.value = (e as Error).message;
  } finally {
    loadingSnapshots.value = false;
  }
});

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
  <h2>Restore files</h2>
  <p class="lede">
    Get files back from a backup. Ballast never overwrites your live data: it restores into a separate folder, and you
    copy back what you need once you've looked.
  </p>

  <p v-if="error" class="notice notice--error" role="alert">{{ error }}</p>
  <div v-if="success" class="notice notice--ok" role="status">
    <p>Restored into <code>{{ success }}</code> on the Ballast server.</p>
  </div>

  <form class="panel" @submit.prevent="restore">
    <div class="field">
      <label for="restore-service">What do you want to restore?</label>
      <select id="restore-service" v-model="selectedService">
        <option value="" disabled>Choose…</option>
        <option v-for="s in services" :key="s.name" :value="s.name">{{ s.name }}</option>
      </select>
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
              <table>
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
                    <td><code>{{ snap.short_id }}</code></td>
                    <td>{{ formatDateTime(snap.time) }}</td>
                    <td>{{ snap.hostname }}</td>
                    <td>{{ (snap.paths ?? []).join(", ") }}</td>
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
</template>

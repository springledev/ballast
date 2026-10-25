<script setup lang="ts">
import { computed, onMounted, reactive, ref } from "vue";

import { api } from "@/api/client";
import type { ServiceStatus } from "@/api/types";
import ListTools from "@/components/ListTools.vue";
import { countLights, emptyQuery, filterEntries, groupEntries, needsTools, type ListEntry, type ListQuery } from "@/lib/listView";
import { describeSchedule, presetFor, schedulePresets } from "@/lib/schedule";
import { worstLight } from "@/lib/status";
import { revealOnError } from "@/lib/revealError";

const services = ref<ServiceStatus[]>([]);
const loading = ref(true);
const error = ref("");
revealOnError(error);
const saved = ref("");
// choice is a preset id, "none", or "custom"; custom holds the cron text.
const choice = reactive<Record<string, string>>({});
const custom = reactive<Record<string, string>>({});
const saving = ref<Set<string>>(new Set());

async function load() {
  loading.value = true;
  error.value = "";
  try {
    services.value = await api.status();
    for (const s of services.value) {
      const preset = presetFor(s.schedule?.cron);
      choice[s.service] = !s.schedule ? "none" : (preset?.id ?? "custom");
      custom[s.service] = s.schedule?.cron ?? "0 3 * * *";
    }
  } catch (e) {
    error.value = (e as Error).message;
  } finally {
    loading.value = false;
  }
}

// apply sets one service's schedule to a preset id, "none" or "custom".
async function apply(s: ServiceStatus, c: string, cronText: string) {
  const name = s.service;
  if (c === "none") {
    if (s.schedule?.enabled) await api.setScheduleEnabled(name, false);
  } else {
    const cron = c === "custom" ? cronText : schedulePresets.find((p) => p.id === c)!.cron;
    await api.setSchedule(name, cron);
    if (s.schedule && !s.schedule.enabled) await api.setScheduleEnabled(name, true);
  }
}

async function save(s: ServiceStatus) {
  const name = s.service;
  saving.value.add(name);
  error.value = "";
  saved.value = "";
  try {
    await apply(s, choice[name], custom[name]);
    saved.value = `Saved the schedule for ${name}.`;
    await load();
  } catch (e) {
    error.value = `${name}: ${(e as Error).message}`;
  } finally {
    saving.value.delete(name);
  }
}

// --- long lists: search, filter, and one schedule for many at once ---
const query = ref<ListQuery>(emptyQuery());
const entry = (s: ServiceStatus): ListEntry => ({ name: s.service, type: s.catalog_entry, destination: s.destination, light: worstLight(s) });
const showTools = computed(() => needsTools(services.value.length));
const shown = computed(() => (showTools.value ? filterEntries(services.value, entry, query.value) : services.value));
const groups = computed(() => groupEntries(shown.value, entry, showTools.value ? query.value.group : "none"));
const counts = computed(() => countLights(services.value, entry));

const selected = ref<Set<string>>(new Set());
const bulkChoice = ref(schedulePresets[0]?.id ?? "none");
const bulkBusy = ref(false);
function toggle(name: string, on: boolean) {
  const next = new Set(selected.value);
  if (on) next.add(name);
  else next.delete(name);
  selected.value = next;
}
async function applyToSelected() {
  bulkBusy.value = true;
  error.value = "";
  saved.value = "";
  const failed: string[] = [];
  const picked = services.value.filter((s) => selected.value.has(s.service));
  for (const s of picked) {
    try {
      await apply(s, bulkChoice.value, custom[s.service]);
    } catch (e) {
      failed.push(`${s.service} (${(e as Error).message})`);
    }
  }
  if (failed.length) error.value = `Couldn't change: ${failed.join(", ")}`;
  saved.value = `Saved the schedule for ${picked.length - failed.length} of ${picked.length}.`;
  selected.value = new Set();
  bulkBusy.value = false;
  await load();
}

onMounted(load);
</script>

<template>
  <header class="page-head">
    <h2>Schedules</h2>
    <p class="lede">How often each backup runs by itself. You can always start one from the Overview too.</p>
  </header>

  <p v-if="error" class="notice notice--error" role="alert">{{ error }}</p>
  <p v-if="saved" class="notice notice--ok" role="status">{{ saved }}</p>
  <p v-if="loading" class="loading">Loading…</p>

  <template v-else>
    <div v-if="services.length === 0" class="empty">
      <p>Nothing to schedule yet.</p>
      <router-link class="btn btn-primary" to="/wizard">Set up a backup</router-link>
    </div>

    <template v-else>
    <ListTools
      v-if="showTools"
      v-model="query"
      :counts="counts"
      :groups="['none', 'status', 'type', 'destination']"
      :shown="shown.length"
      :total="services.length"
      label="backups"
    />
    <div v-if="showTools" class="bulk-bar" :class="{ 'bulk-bar--active': selected.size > 0 }">
      <template v-if="selected.size">
        <strong>{{ selected.size }} selected</strong>
        <select v-model="bulkChoice" style="width: auto" aria-label="New schedule for the selected backups">
          <option v-for="p in schedulePresets" :key="p.id" :value="p.id">{{ p.label }}</option>
          <option value="none">Only when I start it</option>
        </select>
        <button class="btn btn-primary" type="button" :disabled="bulkBusy" @click="applyToSelected">
          {{ bulkBusy ? "Saving…" : `Apply to ${selected.size}` }}
        </button>
        <button class="btn btn-quiet" type="button" @click="selected = new Set()">Clear</button>
      </template>
      <button v-else class="btn btn-quiet" type="button" :disabled="!shown.length" @click="selected = new Set(shown.map((s) => s.service))">
        Select all shown
      </button>
    </div>
    <p v-if="showTools && !shown.length" class="empty">
      Nothing matches. <button class="btn btn-quiet" type="button" @click="query = emptyQuery()">Show everything</button>
    </p>

    <section v-for="g in groups" :key="g.key" class="group" :aria-label="g.label || undefined">
    <h3 v-if="g.label" class="group-head">{{ g.label }} <span class="muted">{{ g.items.length }}</span></h3>
    <ul v-if="g.items.length" class="list">
      <li v-for="s in g.items" :key="s.service">
        <form @submit.prevent="save(s)">
          <div class="item-head">
            <label v-if="showTools" class="row-check">
              <input
                type="checkbox"
                :checked="selected.has(s.service)"
                :aria-label="`Select ${s.service}`"
                @change="toggle(s.service, ($event.target as HTMLInputElement).checked)"
              />
            </label>
            <div style="flex: 1">
              <div class="item-title">{{ s.service }}</div>
              <div class="item-sub">Now: {{ describeSchedule(s.schedule) }}</div>
            </div>
          </div>
          <div class="actions" style="margin-top: 0.75rem">
            <select v-model="choice[s.service]" class="schedule-select" :aria-label="`How often to back up ${s.service}`">
              <option v-for="p in schedulePresets" :key="p.id" :value="p.id">{{ p.label }}</option>
              <option value="none">Only when I start it</option>
              <option value="custom">Custom…</option>
            </select>
            <button class="btn btn-primary" type="submit" :disabled="saving.has(s.service)">
              {{ saving.has(s.service) ? "Saving…" : "Save" }}
            </button>
          </div>
          <details class="advanced" :open="choice[s.service] === 'custom'">
            <summary>Advanced: custom cron expression</summary>
            <div class="advanced-body">
              <div class="field" style="margin-bottom: 0.5rem">
                <label :for="`cron-${s.service}`">Cron expression</label>
                <input
                  :id="`cron-${s.service}`"
                  v-model="custom[s.service]"
                  placeholder="30 2 * * *"
                  @input="choice[s.service] = 'custom'"
                />
              </div>
              <p class="hint">
                Five fields: minute, hour, day of month, month, day of week. Day of month and day of week follow
                classic cron: either can match, unless one starts with <code>*</code>. A new schedule runs once at
                the next check, then follows the expression.
              </p>
            </div>
          </details>
        </form>
      </li>
    </ul>
    </section>
    </template>
  </template>
</template>

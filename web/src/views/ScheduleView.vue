<script setup lang="ts">
import { onMounted, reactive, ref } from "vue";

import { api } from "@/api/client";
import type { ServiceStatus } from "@/api/types";
import { describeSchedule, presetFor, schedulePresets } from "@/lib/schedule";

const services = ref<ServiceStatus[]>([]);
const loading = ref(true);
const error = ref("");
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

async function save(s: ServiceStatus) {
  const name = s.service;
  saving.value.add(name);
  error.value = "";
  saved.value = "";
  try {
    const c = choice[name];
    if (c === "none") {
      if (s.schedule?.enabled) await api.setScheduleEnabled(name, false);
    } else {
      const cron = c === "custom" ? custom[name] : schedulePresets.find((p) => p.id === c)!.cron;
      await api.setSchedule(name, cron);
      if (s.schedule && !s.schedule.enabled) await api.setScheduleEnabled(name, true);
    }
    saved.value = `Saved the schedule for ${name}.`;
    await load();
  } catch (e) {
    error.value = `${name}: ${(e as Error).message}`;
  } finally {
    saving.value.delete(name);
  }
}

onMounted(load);
</script>

<template>
  <h2>Schedules</h2>
  <p class="lede">How often each backup runs by itself. You can always start one from the Overview too.</p>

  <p v-if="error" class="notice notice--error" role="alert">{{ error }}</p>
  <p v-if="saved" class="notice notice--ok" role="status">{{ saved }}</p>
  <p v-if="loading" class="loading">Loading…</p>

  <template v-else>
    <div v-if="services.length === 0" class="empty">
      <p>Nothing to schedule yet.</p>
      <router-link class="btn btn-primary" to="/wizard">Set up a backup</router-link>
    </div>

    <ul v-else class="list">
      <li v-for="s in services" :key="s.service">
        <form @submit.prevent="save(s)">
          <div class="item-head">
            <div>
              <div class="item-title">{{ s.service }}</div>
              <div class="item-sub">Now: {{ describeSchedule(s.schedule) }}</div>
            </div>
          </div>
          <div class="actions" style="margin-top: 0.75rem">
            <select v-model="choice[s.service]" style="max-width: 260px" :aria-label="`How often to back up ${s.service}`">
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
  </template>
</template>

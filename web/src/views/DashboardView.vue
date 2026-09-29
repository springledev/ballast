<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from "vue";
import { useRouter } from "vue-router";

import TrafficLight from "@/components/TrafficLight.vue";
import { api } from "@/api/client";
import type { ServiceStatus } from "@/api/types";
import { formatAgeSeconds, formatDateTime, formatRelative } from "@/lib/format";
import { describeSchedule } from "@/lib/schedule";
import { describeTier, summarize, worstLight } from "@/lib/status";

const router = useRouter();
const services = ref<ServiceStatus[]>([]);
const loading = ref(true);
const error = ref("");
const starting = ref<Set<string>>(new Set());

const summary = computed(() => summarize(services.value));
const anyRunning = computed(() => services.value.some((s) => s.tiers.some((t) => t.light === "running")));

// FIRST_RUN_KEY marks that this browser has already been sent to the
// wizard once. A brand-new install has nothing configured, so send a
// beginner straight to setup — exactly once, so someone who later
// removes every service on purpose isn't forced back into it.
const FIRST_RUN_KEY = "ballast_sent_to_wizard";

async function load(quiet = false) {
  if (!quiet) loading.value = true;
  try {
    services.value = await api.status();
    error.value = "";
    if (services.value.length === 0 && !hasSeenWizardPrompt()) {
      markWizardPrompted();
      router.replace("/wizard");
    }
  } catch (e) {
    error.value = (e as Error).message;
  } finally {
    loading.value = false;
  }
}

function hasSeenWizardPrompt(): boolean {
  try {
    return window.localStorage.getItem(FIRST_RUN_KEY) === "1";
  } catch {
    return false;
  }
}

function markWizardPrompted(): void {
  try {
    window.localStorage.setItem(FIRST_RUN_KEY, "1");
  } catch {
    // No persistent storage: worst case the redirect happens again.
  }
}

async function backupNow(name: string) {
  starting.value.add(name);
  error.value = "";
  try {
    await api.startBackup(name);
    await load(true);
  } catch (e) {
    error.value = `Couldn't start a backup of ${name}: ${(e as Error).message}`;
  } finally {
    starting.value.delete(name);
  }
}

// Refresh while anything is running, so a backup started here (or on
// schedule) turns green or red without a manual reload.
let timer: ReturnType<typeof setInterval> | undefined;
onMounted(() => {
  load();
  timer = setInterval(() => {
    if (anyRunning.value) load(true);
  }, 5000);
});
onUnmounted(() => clearInterval(timer));
</script>

<template>
  <h2>Overview</h2>
  <p class="lede">A backup only counts as safe once Ballast has restored part of it to prove it works.</p>

  <p v-if="error" class="notice notice--error" role="alert">{{ error }}</p>
  <p v-if="loading" class="loading">Loading…</p>

  <template v-else>
    <div v-if="services.length === 0" class="empty">
      <p>Nothing is being backed up yet.</p>
      <router-link class="btn btn-primary" to="/wizard">Set up your first backup</router-link>
    </div>

    <template v-else>
      <div class="summary" :class="`summary--${summary.light}`" role="status">
        <p class="summary-text">{{ summary.text }}</p>
      </div>

      <ul class="list">
        <li v-for="s in services" :key="s.service" class="signal" :class="`signal--${worstLight(s)}`">
          <div class="item-head">
            <div>
              <div class="item-title">{{ s.service }}</div>
              <div class="item-sub">{{ describeSchedule(s.schedule) }}</div>
            </div>
            <div class="actions">
              <TrafficLight :light="worstLight(s)" />
              <button
                class="btn"
                type="button"
                :disabled="starting.has(s.service) || s.tiers.some((t) => t.light === 'running')"
                @click="backupNow(s.service)"
              >
                {{ starting.has(s.service) ? "Starting…" : "Back up now" }}
              </button>
            </div>
          </div>

          <p v-for="t in s.tiers" :key="t.tier" class="status-line">
            <template v-if="s.tiers.length > 1"><strong>{{ t.tier }} copy:</strong> </template>{{ describeTier(t) }}
          </p>
          <p v-for="t in s.tiers.filter((t) => t.light === 'red' && t.last_error)" :key="`err-${t.tier}`" class="notice notice--error" style="margin: 0.6rem 0 0">
            {{ t.last_error }}
          </p>

          <details class="advanced">
            <summary>Details</summary>
            <div class="advanced-body">
              <dl class="kv">
                <dt>Type</dt>
                <dd>{{ s.catalog_entry }}</dd>
                <dt>Stored in</dt>
                <dd>{{ s.destination }}</dd>
                <dt>Schedule</dt>
                <dd>
                  <code v-if="s.schedule">{{ s.schedule.cron }}</code><span v-else>none</span>
                  <span v-if="s.schedule && !s.schedule.enabled"> (paused)</span>
                </dd>
                <dt>Last safe backup</dt>
                <dd>{{ formatAgeSeconds(s.freshness_age_seconds) }}</dd>
                <template v-for="t in s.tiers" :key="`adv-${t.tier}`">
                  <dt>{{ t.tier }}</dt>
                  <dd>
                    {{ t.last_state ?? "no runs" }}<template v-if="t.last_run_id">, run #{{ t.last_run_id }}</template>
                    <template v-if="t.last_finished_at">, finished {{ formatDateTime(t.last_finished_at) }}</template>
                    <template v-if="t.snapshot_id">, snapshot <code>{{ t.snapshot_id.slice(0, 8) }}</code></template>
                    <template v-if="t.last_verified_at">, last checked restore {{ formatRelative(t.last_verified_at) }}</template>
                  </dd>
                </template>
              </dl>
            </div>
          </details>
        </li>
      </ul>
    </template>
  </template>
</template>

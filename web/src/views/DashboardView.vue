<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from "vue";
import { useRouter } from "vue-router";

import ListTools from "@/components/ListTools.vue";
import TrafficLight from "@/components/TrafficLight.vue";
import { api } from "@/api/client";
import type { HealthItem, ServiceStatus } from "@/api/types";
import { anyServiceVerified, checklistComplete, firstRunChecklist } from "@/lib/checklist";
import { describeUpload, formatAgeSeconds, formatDateTime, formatRelative } from "@/lib/format";
import { emptyProgress, parseProgress, scriptSteps, type ScriptProgress } from "@/lib/replaceScript";
import { describeSchedule } from "@/lib/schedule";
import { COLLAPSE_SAFE_FROM, collapsesGroup, countLights, emptyQuery, filterEntries, groupEntries, lightShares, needsTools, type ListEntry, type ListQuery } from "@/lib/listView";
import { usePref } from "@/lib/prefs";
import { askConfirm } from "@/lib/confirm";
import { cancelQuestion, describeTier, isCancelling, lightLabel, shortStatus, summarize, worstLight } from "@/lib/status";

const router = useRouter();
const services = ref<ServiceStatus[]>([]);
const loading = ref(true);
const error = ref("");
const starting = ref<Set<string>>(new Set());

const healthItems = ref<HealthItem[]>([]);
// Counts for the first-run checklist; null until known (or if they can't be read).
const setupCounts = ref<{ destinations: number; channels: number } | null>(null);

const problems = computed(() => healthItems.value.filter((h) => h.status !== "ok"));
const checklist = computed(() =>
  setupCounts.value
    ? firstRunChecklist({
        ...setupCounts.value,
        services: services.value.length,
        anyVerified: anyServiceVerified(services.value),
      })
    : [],
);
const showChecklist = computed(() => checklist.value.length > 0 && !checklistComplete(checklist.value));

// "Replace a script" tracker: ticked by hand, kept in this browser only.
const SCRIPT_KEY = "ballast_replace_script";
const scriptProgress = ref<Record<string, ScriptProgress>>({});
const scriptOpen = ref(true);
try {
  scriptProgress.value = parseProgress(localStorage.getItem(SCRIPT_KEY));
  scriptOpen.value = localStorage.getItem(SCRIPT_KEY + "_hidden") !== "1";
} catch {
  /* private window: start empty */
}

function progressFor(name: string): ScriptProgress {
  return scriptProgress.value[name] ?? emptyProgress();
}

function tickStep(name: string, key: keyof ScriptProgress, value: boolean) {
  scriptProgress.value = { ...scriptProgress.value, [name]: { ...progressFor(name), [key]: value } };
  try {
    localStorage.setItem(SCRIPT_KEY, JSON.stringify(scriptProgress.value));
  } catch {
    /* not saved; still works until the page is closed */
  }
}

function hideScriptPanel() {
  scriptOpen.value = false;
  try {
    localStorage.setItem(SCRIPT_KEY + "_hidden", "1");
  } catch {
    /* ignore */
  }
}

const healthLabels: Record<HealthItem["area"], string> = {
  "self-backup": "Ballast's own backup",
  disk: "Disk space",
  credentials: "Credentials",
  runtime: "How Ballast is running",
};

async function loadSetup() {
  try {
    const [destinations, channels] = await Promise.all([api.listDestinations(), api.listChannels()]);
    setupCounts.value = { destinations: destinations.length, channels: channels.length };
  } catch {
    setupCounts.value = null;
  }
}

async function loadHealth() {
  try {
    healthItems.value = (await api.ballastHealth()).items ?? [];
  } catch {
    healthItems.value = [];
  }
}

const summary = computed(() => summarize(services.value));

// --- long lists: search, filter, group, compact rows, bulk backup ---
const entry = (s: ServiceStatus): ListEntry => ({
  name: s.service,
  type: s.catalog_entry,
  destination: s.destination,
  light: worstLight(s),
});
const COMPACT_FROM = 24;
const query = ref<ListQuery>(emptyQuery());
// A big install opens with problems on top; a small one keeps its order.
let grouped = false;
watch(services, (list) => {
  if (!grouped && list.length > COMPACT_FROM) {
    grouped = true;
    query.value = { ...query.value, group: "status" };
  }
});
// "auto" until someone picks: cards for a handful of backups, compact
// rows once there are too many to scroll through as cards.
const densityPref = usePref("ballast_overview_density", "auto", ["auto", "cards", "compact"] as const);
const density = computed({
  get: () => (densityPref.value === "auto" ? (services.value.length > COMPACT_FROM ? "compact" : "cards") : densityPref.value),
  set: (v: string) => (densityPref.value = v === "compact" ? "compact" : "cards"),
});
const showTools = computed(() => needsTools(services.value.length));
// Small installs always see everything as cards; the tools only apply once shown.
const effectiveQuery = computed(() => (showTools.value ? query.value : emptyQuery()));
const compact = computed(() => showTools.value && density.value === "compact");
const counts = computed(() => countLights(services.value, entry));
const shares = computed(() => lightShares(services.value, entry));
const filtered = computed(() => filterEntries(services.value, entry, effectiveQuery.value));
const groups = computed(() => groupEntries(filtered.value, entry, effectiveQuery.value.group));

// A long healthy group stays folded until asked for.
const safeOpen = ref(false);
const folded = (g: { key: string; items: unknown[] }) => collapsesGroup(g, effectiveQuery.value, safeOpen.value);

function showOnly(light: ListQuery["light"]) {
  if (!showTools.value) return;
  query.value = { ...query.value, light: query.value.light === light ? "all" : light };
}

const selected = ref<Set<string>>(new Set());
function toggleSelected(name: string, on: boolean) {
  const next = new Set(selected.value);
  if (on) next.add(name);
  else next.delete(name);
  selected.value = next;
}
function selectAllShown() {
  selected.value = new Set(filtered.value.filter((s) => !isRunning(s)).map((s) => s.service));
}
const bulkBusy = ref(false);
async function backupSelected() {
  bulkBusy.value = true;
  const names = [...selected.value];
  const failed: string[] = [];
  for (const name of names) {
    try {
      await api.startBackup(name);
    } catch {
      failed.push(name);
    }
  }
  selected.value = new Set();
  error.value = failed.length ? `Couldn't start a backup of ${failed.join(", ")}.` : "";
  bulkBusy.value = false;
  await load(true);
}

function isRunning(s: ServiceStatus): boolean {
  return s.tiers.some((t) => t.light === "running");
}

function targetLine(t: NonNullable<ServiceStatus["targets"]>[number]): string {
  if (t.secondary) {
    return t.light === "green" ? "copied and checked" : t.light === "none" ? "no copy yet" : t.light === "running" ? "waiting for the main backup" : "needs attention";
  }
  return t.light === "green"
    ? "backed up and checked"
    : t.light === "amber"
      ? "backed up, but could not be fully checked"
      : t.light === "red"
        ? "failed"
        : t.light === "running"
          ? "backing up now"
          : "no backup yet";
}
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
    if (!quiet) await Promise.all([loadSetup(), loadHealth()]);
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

// Stopping a running backup asks first (the safe answer is focused),
// then shows "Cancelling…" until the run has tidied up.
const stopping = ref<Set<string>>(new Set());
async function cancelNow(name: string) {
  if (!(await askConfirm(cancelQuestion.message, cancelQuestion.confirmLabel, cancelQuestion.cancelLabel))) return;
  stopping.value.add(name);
  error.value = "";
  try {
    await api.cancelBackup(name);
    await load(true);
  } catch (e) {
    error.value = `Couldn't stop the backup of ${name}: ${(e as Error).message}`;
  } finally {
    stopping.value.delete(name);
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
  <header class="page-head">
    <h2>Overview</h2>
    <p class="lede">A backup only counts as safe once Ballast has restored part of it to prove it works.</p>
  </header>

  <p v-if="error" class="notice notice--error" role="alert">{{ error }}</p>
  <p v-if="loading" class="loading">Loading…</p>

  <template v-else>
    <div v-if="services.length === 0" class="empty empty--hero">
      <p class="empty-title">Nothing is being backed up yet.</p>
      <p>Pick a folder or a Docker app and Ballast will back it up, then prove it can be restored.</p>
      <router-link class="btn btn-primary" to="/wizard">Set up your first backup</router-link>
    </div>

    <section v-else class="hero" :class="`hero--${summary.light}`" aria-label="Backup status">
      <p class="hero-text" role="status">{{ summary.text }}</p>
      <div class="waterline" :class="{ 'waterline--interactive': showTools }">
        <component
          :is="showTools ? 'button' : 'span'"
          v-for="sh in shares"
          :key="sh.light"
          :type="showTools ? 'button' : undefined"
          class="waterline-seg"
          :class="`waterline-seg--${sh.light}`"
          :style="{ flexGrow: sh.count }"
          :aria-label="showTools ? `Show only: ${lightLabel[sh.light]} (${sh.count})` : undefined"
          :aria-pressed="showTools ? query.light === sh.light : undefined"
          @click="showOnly(sh.light)"
        />
      </div>
      <ul class="hero-legend">
        <li v-for="sh in shares" :key="sh.light">
          <span class="lamp" :class="`lamp--${sh.light}`">{{ lightLabel[sh.light] }}</span>
          <span class="hero-count">{{ sh.count }}</span>
        </li>
      </ul>
    </section>

    <div v-if="problems.length" class="notice" :class="problems.some((h) => h.status === 'failed') ? 'notice--error' : 'notice--warn'" role="status">
      <strong>Ballast health</strong>
      <p v-for="h in problems" :key="h.area" class="health-line">
        <strong>{{ healthLabels[h.area] ?? h.area }}:</strong> {{ h.message }}
      </p>
    </div>

    <div v-if="showChecklist" class="panel">
      <h3>Getting started</h3>
      <ul class="checklist">
        <li v-for="item in checklist" :key="item.key">
          <span v-if="item.done" class="muted"><span aria-hidden="true">✓</span> {{ item.label }}</span>
          <router-link v-else :to="item.to">{{ item.label }}</router-link>
        </li>
      </ul>
    </div>

    <template v-if="services.length">
      <ListTools
        v-if="showTools"
        v-model="query"
        v-model:density="density"
        :counts="counts"
        :groups="['none', 'status', 'type', 'destination']"
        :shown="filtered.length"
        :total="services.length"
        label="backups"
      />

      <div v-if="compact && filtered.length" class="bulk-bar" :class="{ 'bulk-bar--active': selected.size > 0 }">
        <template v-if="selected.size">
          <strong>{{ selected.size }} selected</strong>
          <button class="btn btn-primary" type="button" :disabled="bulkBusy" @click="backupSelected">
            {{ bulkBusy ? "Starting…" : `Back up ${selected.size} now` }}
          </button>
          <button class="btn btn-quiet" type="button" @click="selected = new Set()">Clear</button>
        </template>
        <button v-else class="btn btn-quiet" type="button" @click="selectAllShown">Select all shown</button>
      </div>

      <p v-if="!filtered.length" class="empty">Nothing matches. <button class="btn btn-quiet" type="button" @click="query = emptyQuery()">Show everything</button></p>

      <section v-for="g in groups" :key="g.key" class="group" :aria-label="g.label || undefined">
        <h3 v-if="g.label" class="group-head">{{ g.label }} <span class="muted">{{ g.items.length }}</span></h3>
        <p v-if="folded(g)" class="group-folded">
          <button class="btn btn-quiet" type="button" @click="safeOpen = true">Show all {{ g.items.length }} safe backups</button>
        </p>
        <button v-else-if="g.key === 'green' && safeOpen && g.items.length > COLLAPSE_SAFE_FROM" class="btn btn-quiet btn-small group-fold" type="button" @click="safeOpen = false">Hide safe backups</button>

        <ul v-if="!folded(g) && compact" class="rows">
          <li v-for="s in g.items" :key="s.service" class="row signal" :class="`signal--${worstLight(s)}`">
            <label class="row-check">
              <input
                type="checkbox"
                :checked="selected.has(s.service)"
                :disabled="isRunning(s)"
                :aria-label="`Select ${s.service}`"
                @change="toggleSelected(s.service, ($event.target as HTMLInputElement).checked)"
              />
            </label>
            <div class="row-main">
              <router-link class="item-title row-link" :to="{ path: '/services', query: { q: s.service } }">{{ s.service }}</router-link>
              <span class="item-sub">{{ shortStatus(s) }}</span>
            </div>
            <span class="row-meta item-sub">{{ s.catalog_entry }}, {{ s.destination }}</span>
            <router-link class="btn btn-small btn-quiet" :to="`/history/${encodeURIComponent(s.service)}`">History</router-link>
            <button
              v-if="isRunning(s)"
              class="btn btn-small btn-danger cancel-backup"
              type="button"
              :disabled="stopping.has(s.service) || isCancelling(s)"
              @click="cancelNow(s.service)"
            >
              {{ stopping.has(s.service) || isCancelling(s) ? "Cancelling…" : "Cancel" }}
            </button>
            <button v-else class="btn btn-small" type="button" :disabled="starting.has(s.service)" @click="backupNow(s.service)">
              {{ starting.has(s.service) ? "Starting…" : "Back up" }}
            </button>
          </li>
        </ul>

        <ul v-else-if="!folded(g)" class="cards">
          <li v-for="s in g.items" :key="s.service" class="card signal" :class="`signal--${worstLight(s)}`">
            <div class="item-head">
              <div class="item-name">
                <div class="item-title">{{ s.service }}</div>
                <div class="item-sub">{{ describeSchedule(s.schedule) }}</div>
              </div>
              <TrafficLight :light="worstLight(s)" />
            </div>

            <p v-for="t in s.tiers" :key="t.tier" class="status-line">
              <template v-if="s.tiers.length > 1"><strong>{{ t.tier }} copy:</strong> </template>{{ describeTier(t) }}
            </p>
            <template v-for="t in s.targets ?? []" :key="`up-${t.destination}`">
              <div v-if="t.upload && t.upload.total_bytes > 0" class="upload">
                <progress :value="t.upload.percent" max="100" :aria-label="`Upload to ${t.destination}`"></progress>
                <span class="item-sub">Uploading to {{ t.destination }}: {{ describeUpload(t.upload) }}</span>
              </div>
            </template>
            <ul v-if="(s.targets?.length ?? 0) > 1" class="targets">
              <li v-for="t in s.targets" :key="`tg-${t.destination}`">
                <TrafficLight :light="t.light" />
                <span><strong>{{ t.destination }}<template v-if="t.secondary"> (second copy)</template>:</strong>
                  {{ targetLine(t) }}<template v-if="t.last_error && t.light !== 'green'"> — {{ t.last_error }}</template>
                  <router-link v-if="t.sign_in_needed" to="/destinations"> Sign in to Proton Drive again</router-link></span>
              </li>
            </ul>
            <p v-for="t in s.tiers.filter((t) => t.light === 'red' && t.last_error)" :key="`err-${t.tier}`" class="notice notice--error card-error">
              {{ t.last_error }}
            </p>

            <div class="card-foot">
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
              <router-link class="btn btn-quiet" :to="`/history/${encodeURIComponent(s.service)}`">History</router-link>
              <button
                v-if="isRunning(s)"
                class="btn btn-danger cancel-backup"
                type="button"
                :disabled="stopping.has(s.service) || isCancelling(s)"
                @click="cancelNow(s.service)"
              >
                {{ stopping.has(s.service) || isCancelling(s) ? "Cancelling…" : "Cancel backup" }}
              </button>
              <button v-else class="btn" type="button" :disabled="starting.has(s.service)" @click="backupNow(s.service)">
                {{ starting.has(s.service) ? "Starting…" : "Back up now" }}
              </button>
            </div>
          </li>
        </ul>
      </section>
    </template>

    <details v-if="services.length > 0 && scriptOpen" class="panel panel--fold">
      <summary><h3>Replacing a backup script?</h3><span class="muted">A checklist for running Ballast beside it first</span></summary>
      <p class="muted">
        Run Ballast beside your script first. Tick each step as you go; the last one unlocks when the service is green. The guide is in docs/replace-a-script.md.
      </p>
      <div class="script-grid">
        <div v-for="s in services" :key="s.service" class="script-item">
          <strong>{{ s.service }}</strong>
          <ul class="checklist">
            <li v-for="step in scriptSteps(progressFor(s.service), worstLight(s))" :key="step.key">
              <label v-if="step.key !== 'retire'" class="check">
                <input
                  type="checkbox"
                  :checked="step.done"
                  :disabled="!step.enabled"
                  @change="tickStep(s.service, step.key as keyof ScriptProgress, ($event.target as HTMLInputElement).checked)"
                />
                <span>{{ step.label }}</span>
              </label>
              <span v-else :class="step.enabled ? '' : 'muted'">{{ step.enabled ? "✓" : "…" }} {{ step.label }}. {{ step.hint }}</span>
            </li>
          </ul>
        </div>
      </div>
      <button class="btn btn-quiet" type="button" @click="hideScriptPanel">I'm done with this</button>
    </details>
  </template>
</template>

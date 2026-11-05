<script setup lang="ts">
import { computed, nextTick, onMounted, ref, watch } from "vue";
import { useRoute } from "vue-router";

import { api } from "@/api/client";
import type { RunDetail, RunSummary } from "@/api/types";
import TrafficLight from "@/components/TrafficLight.vue";
import { formatDateTime, formatRelative } from "@/lib/format";
import { revealOnError } from "@/lib/revealError";
import {
  checkOutcome,
  failureSentence,
  formatRunDuration,
  groupChecks,
  mergeRuns,
  runStatusText,
  verificationLine,
} from "@/lib/runHistory";
import { describeCheck } from "@/lib/status";

const PAGE = 20;

const route = useRoute();
const service = computed(() => String(route.params.service ?? ""));

const runs = ref<RunSummary[]>([]);
const nextBefore = ref(0);
const loading = ref(false);
const loadingMore = ref(false);
const error = ref("");
revealOnError(error);

const detail = ref<RunDetail | null>(null);
const opening = ref<number | null>(null);
const detailError = ref("");
revealOnError(detailError);
const headingEl = ref<HTMLElement | null>(null);
let lastOpened = 0;

async function loadFirst() {
  loading.value = true;
  error.value = "";
  runs.value = [];
  nextBefore.value = 0;
  detail.value = null;
  try {
    const page = await api.listRuns(service.value, PAGE);
    runs.value = page.runs ?? [];
    nextBefore.value = page.next_before ?? 0;
  } catch (e) {
    error.value = `Couldn't load the history for ${service.value}. ${e instanceof Error ? e.message : String(e)}`;
  } finally {
    loading.value = false;
  }
}

async function loadMore() {
  if (!nextBefore.value || loadingMore.value) return;
  loadingMore.value = true;
  error.value = "";
  try {
    const page = await api.listRuns(service.value, PAGE, nextBefore.value);
    runs.value = mergeRuns(runs.value, page.runs ?? []);
    nextBefore.value = page.next_before ?? 0;
  } catch (e) {
    error.value = `Couldn't load older backups. ${e instanceof Error ? e.message : String(e)}`;
  } finally {
    loadingMore.value = false;
  }
}

async function open(r: RunSummary) {
  opening.value = r.id;
  detailError.value = "";
  lastOpened = r.id;
  try {
    detail.value = await api.getRun(r.id);
    await nextTick();
    headingEl.value?.focus();
  } catch (e) {
    detailError.value = `Couldn't open that backup. ${e instanceof Error ? e.message : String(e)}`;
  } finally {
    opening.value = null;
  }
}

async function close() {
  detail.value = null;
  await nextTick();
  document.getElementById(`run-row-${lastOpened}`)?.focus();
}

const groups = computed(() => (detail.value ? groupChecks(detail.value.verification_results ?? []) : []));
const logText = computed(() => (detail.value?.log_tail ?? []).map((l) => `${formatDateTime(l.at)}  ${l.text}`).join("\n"));
const needsAttention = computed(() => !!detail.value && detail.value.light !== "green");

onMounted(loadFirst);
watch(service, loadFirst);
</script>

<template>
  <header class="page-head">
    <h2>Backup history</h2>
    <p class="lede">
      Past backups of <strong>{{ service }}</strong>, newest first. Open one to see whether it can be restored and what happened.
    </p>
    <router-link class="btn btn-quiet" to="/dashboard">Back to overview</router-link>
  </header>

  <p v-if="error" class="notice notice--error" role="alert">{{ error }}</p>

  <!-- One run, in full. The list stays mounted underneath so closing returns to the same row. -->
  <section v-if="detail" class="panel hist-detail" aria-labelledby="hist-detail-title">
    <button class="btn btn-quiet" type="button" @click="close">Back to the list</button>
    <h3 id="hist-detail-title" ref="headingEl" tabindex="-1">
      <TrafficLight :light="detail.light" /> {{ runStatusText(detail) }}
    </h3>
    <p class="item-sub">
      Started {{ formatDateTime(detail.started_at) }} ({{ formatRelative(detail.started_at) }}). Took
      {{ formatRunDuration(detail.duration_seconds) }}.
    </p>
    <p v-if="failureSentence(detail)" class="notice" :class="detail.light === 'red' ? 'notice--error' : 'notice--warn'">
      {{ failureSentence(detail) }}
    </p>

    <h4>Checks that it can be restored</h4>
    <p>{{ verificationLine(detail.verification) }}</p>
    <div v-for="g in groups" :key="g.title" class="hist-group">
      <h5>{{ g.title }}</h5>
      <ul class="hist-checks">
        <li v-for="(c, i) in g.checks" :key="i">
          <span class="hist-check" :class="`hist-check--${checkOutcome(c).kind}`">{{ checkOutcome(c).text }}</span>
          <span>{{ describeCheck(c.method) }}</span>
          <span v-if="c.warning || (!c.passed && c.detail)" class="item-sub hist-why">{{ c.warning || c.detail }}</span>
        </li>
      </ul>
    </div>

    <template v-if="detail.targets.length">
      <h4>Where it was stored</h4>
      <ul class="hist-targets">
        <li v-for="t in detail.targets" :key="t.destination">
          <TrafficLight :light="t.light" />
          <span>
            <strong>{{ t.destination }}<template v-if="t.secondary"> (second copy)</template>:</strong>
            {{ t.light === "green" ? "stored and checked" : t.light === "amber" ? "stored, not checked" : t.light === "running" ? "still working" : "did not work" }}
            <template v-if="t.detail"> — {{ t.detail }}</template>
          </span>
        </li>
      </ul>
    </template>

    <details class="advanced" :open="needsAttention">
      <summary>Details</summary>
      <div class="advanced-body">
        <dl class="kv">
          <dt>Run</dt>
          <dd>#{{ detail.id }}, {{ detail.state }}</dd>
          <template v-if="detail.error">
            <dt>Full error</dt>
            <dd><code class="hist-raw">{{ detail.error }}</code></dd>
          </template>
          <template v-if="detail.snapshot_id">
            <dt>Snapshot</dt>
            <dd><code>{{ detail.snapshot_id.slice(0, 8) }}</code></dd>
          </template>
        </dl>
        <template v-for="g in groups" :key="`raw-${g.title}`">
          <template v-for="(c, i) in g.checks" :key="i">
            <p v-if="c.output" class="item-sub">{{ g.title }}: {{ c.method }}</p>
            <pre v-if="c.output" class="hist-log" tabindex="0">{{ c.output }}</pre>
          </template>
        </template>
        <h5>End of the log</h5>
        <p v-if="detail.log_truncated" class="item-sub">Only the last part of the log is shown.</p>
        <pre v-if="logText" class="hist-log" tabindex="0" aria-label="End of the log">{{ logText }}</pre>
        <p v-else class="item-sub">No log lines were recorded for this backup.</p>
      </div>
    </details>
  </section>

  <div v-show="!detail">
    <p v-if="detailError" class="notice notice--error" role="alert">{{ detailError }}</p>
    <p v-if="loading" class="muted" role="status">Loading history…</p>
    <div v-else-if="!error && runs.length === 0" class="panel">
      <p>{{ service }} hasn't been backed up yet, so there is no history to show.</p>
    </div>
    <ul v-if="runs.length" class="hist-list">
      <li v-for="r in runs" :key="r.id">
        <button
          :id="`run-row-${r.id}`"
          class="hist-row signal"
          :class="`signal--${r.light}`"
          type="button"
          :disabled="opening === r.id"
          @click="open(r)"
        >
          <span class="hist-top">
            <TrafficLight :light="r.light" />
            <strong>{{ runStatusText(r) }}</strong>
            <span class="item-sub">{{ formatDateTime(r.started_at) }} · {{ formatRelative(r.started_at) }}</span>
          </span>
          <span class="item-sub">Took {{ formatRunDuration(r.duration_seconds) }}. {{ verificationLine(r.verification) }}.</span>
          <span v-if="r.targets.length" class="hist-dests">
            <span v-for="t in r.targets" :key="t.destination" class="hist-dest">
              <TrafficLight :light="t.light" /> {{ t.destination }}
            </span>
          </span>
        </button>
      </li>
    </ul>
    <p v-if="nextBefore" class="hist-more">
      <button class="btn" type="button" :disabled="loadingMore" @click="loadMore">
        {{ loadingMore ? "Loading…" : "Load more" }}
      </button>
    </p>
  </div>
</template>

<style scoped>
.hist-list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: grid;
  gap: 0.5rem;
}
.hist-row {
  display: grid;
  gap: 0.35rem;
  width: 100%;
  min-height: var(--touch);
  padding: 0.75rem 1rem;
  text-align: left;
  font: inherit;
  color: var(--ink);
  background: var(--surface);
  border: 1px solid var(--line);
  border-radius: var(--radius);
  cursor: pointer;
  overflow-wrap: anywhere;
}
.hist-row:hover {
  border-color: var(--line-strong);
}
.hist-row:focus-visible {
  outline: 2px solid var(--focus);
  outline-offset: 2px;
}
.hist-top,
.hist-dests {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 0.25rem 0.75rem;
}
.hist-dest {
  display: inline-flex;
  align-items: center;
  gap: 0.35rem;
}
.hist-more {
  margin-top: 1rem;
}
.hist-more .btn,
.hist-detail .btn {
  min-height: var(--touch);
}
.hist-detail {
  overflow-wrap: anywhere;
}
.hist-checks,
.hist-targets {
  list-style: none;
  margin: 0 0 0.75rem;
  padding: 0;
  display: grid;
  gap: 0.5rem;
}
.hist-checks li {
  display: flex;
  flex-wrap: wrap;
  gap: 0.25rem 0.75rem;
}
.hist-targets li {
  display: flex;
  gap: 0.5rem;
  align-items: flex-start;
}
.hist-why {
  flex-basis: 100%;
}
.hist-check {
  font-weight: 600;
}
.hist-check--pass {
  color: var(--green);
}
.hist-check--warning {
  color: var(--amber);
}
.hist-check--fail {
  color: var(--red);
}
.hist-log {
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: var(--fs-small);
  background: var(--surface-sunk);
  border: 1px solid var(--line);
  border-radius: var(--radius-small);
  padding: 0.75rem;
  max-height: 20rem;
  max-width: 100%;
  overflow: auto;
  white-space: pre;
}
</style>

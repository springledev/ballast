<script setup lang="ts">
import { onMounted, ref } from "vue";

import TrafficLight from "@/components/TrafficLight.vue";
import { api } from "@/api/client";
import type { ServiceStatus } from "@/api/types";
import { formatDateTime, formatDurationMs } from "@/lib/format";
import { describeCheck, worstLight } from "@/lib/status";

const services = ref<ServiceStatus[]>([]);
const loading = ref(true);
const error = ref("");

onMounted(async () => {
  try {
    services.value = await api.status(undefined, true);
  } catch (e) {
    error.value = (e as Error).message;
  } finally {
    loading.value = false;
  }
});
</script>

<template>
  <h2>Restore checks</h2>
  <p class="lede">
    After every backup, Ballast restores part of it to prove it works. These are the checks from each service's latest
    backup. A backup is only marked safe when all of them pass.
  </p>

  <p v-if="error" class="notice notice--error" role="alert">{{ error }}</p>
  <p v-if="loading" class="loading">Loading…</p>

  <template v-else>
    <div v-if="services.length === 0" class="empty">
      <p>Nothing is being backed up yet.</p>
      <router-link class="btn btn-primary" to="/wizard">Set up a backup</router-link>
    </div>

    <ul v-else class="list">
      <li v-for="s in services" :key="s.service" class="signal" :class="`signal--${worstLight(s)}`">
        <div class="item-head">
          <div class="item-title">{{ s.service }}</div>
          <TrafficLight :light="worstLight(s)" />
        </div>
        <p v-if="!s.verification?.length" class="muted status-line">No checks have run yet.</p>
        <ul v-else style="list-style: none; padding: 0; margin: 0.5rem 0 0">
          <li v-for="(v, i) in s.verification" :key="i" style="margin-bottom: 0.35rem">
            <span class="lamp" :class="v.passed ? 'lamp--green' : 'lamp--red'">{{ v.passed ? "Passed" : "Failed" }}</span>
            &nbsp;{{ describeCheck(v.method) }}
            <span v-if="v.warning" class="hint"> ({{ v.warning }})</span>
          </li>
        </ul>

        <details v-if="s.alert_decisions?.length" class="advanced">
          <summary>Recent alerts</summary>
          <div class="advanced-body">
            <ul style="list-style: none; padding: 0; margin: 0">
              <li v-for="(d, i) in s.alert_decisions" :key="i" class="hint">
                {{ new Date(d.at).toLocaleString() }} · {{ d.outcome === "sent" ? "Sent" : d.outcome === "held" ? "Held back" : "Could not send" }}
                {{ d.type.replace(/_/g, " ") }}<template v-if="d.reason"> ({{ d.reason }})</template>
              </li>
            </ul>
          </div>
        </details>

        <details v-if="s.verification?.length" class="advanced">
          <summary>Details</summary>
          <div class="advanced-body">
            <div class="table-scroll">
              <table class="stack">
                <thead>
                  <tr>
                    <th>Method</th>
                    <th>Result</th>
                    <th>Took</th>
                    <th>When</th>
                  </tr>
                </thead>
                <tbody>
                  <template v-for="(v, i) in s.verification" :key="i">
                    <tr>
                      <td data-label="Method"><code>{{ v.method }}</code></td>
                      <td data-label="Result">{{ v.passed ? "passed" : "failed" }}</td>
                      <td data-label="Took">{{ formatDurationMs(v.duration_ms) }}</td>
                      <td data-label="When">{{ formatDateTime(v.recorded_at) }}</td>
                    </tr>
                    <tr v-if="v.detail || v.output">
                      <td colspan="4"><pre class="raw">{{ [v.detail, v.output].filter(Boolean).join("\n") }}</pre></td>
                    </tr>
                  </template>
                </tbody>
              </table>
            </div>
          </div>
        </details>
      </li>
    </ul>
  </template>
</template>

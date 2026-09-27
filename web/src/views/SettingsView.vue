<script setup lang="ts">
import { onMounted, reactive, ref } from "vue";

import { api } from "@/api/client";
import type { NotificationSettings, Service } from "@/api/types";

const services = ref<Service[]>([]);
const settings = reactive<Record<string, NotificationSettings>>({});
const maxAgeInput = reactive<Record<string, string>>({});
const loading = ref(true);
const error = ref("");
const saving = ref<Set<string>>(new Set());
const saved = ref<Set<string>>(new Set());

async function load() {
  loading.value = true;
  error.value = "";
  try {
    services.value = await api.listServices();
    await Promise.all(
      services.value.map(async (s) => {
        const st = await api.getNotificationSettings(s.name);
        settings[s.name] = st;
        maxAgeInput[s.name] = st.max_age_hours ? String(st.max_age_hours) : "";
      }),
    );
  } catch (e) {
    error.value = (e as Error).message;
  } finally {
    loading.value = false;
  }
}

async function save(name: string) {
  saving.value.add(name);
  saved.value.delete(name);
  error.value = "";
  try {
    const raw = maxAgeInput[name].trim();
    const hours = raw === "" ? undefined : Number(raw);
    if (hours !== undefined && (!Number.isInteger(hours) || hours < 1)) {
      throw new Error("The number of hours must be a whole number, 1 or more (or blank).");
    }
    settings[name] = await api.setNotificationSettings(name, {
      enabled: settings[name].enabled,
      heartbeat_enabled: settings[name].heartbeat_enabled,
      max_age_hours: hours,
    });
    saved.value.add(name);
  } catch (e) {
    error.value = `${name}: ${(e as Error).message}`;
  } finally {
    saving.value.delete(name);
  }
}

onMounted(load);
</script>

<template>
  <h2>Notifications</h2>
  <p class="lede">Ballast always tells you when a backup fails. Choose what else you want to hear about.</p>

  <details class="advanced panel" style="margin-top: 0">
    <summary>Set up where messages go (phone app or webhook)</summary>
    <div class="advanced-body">
      <p style="margin-top: 0"><strong>To get alerts on your phone with ntfy</strong> (free, no account needed):</p>
      <ol>
        <li>Install the <a href="https://ntfy.sh/" target="_blank" rel="noopener">ntfy app</a> and subscribe to a hard-to-guess topic name.</li>
        <li>
          On the Ballast server, save the topic's full link (like <code>https://ntfy.sh/my-secret-topic</code>) in a
          file called <code>ballast-ntfy-topic</code> in Ballast's credential folder, readable only by Ballast
          (<code>chmod 600</code>).
        </li>
      </ol>
      <p>That's it: Ballast picks it up on the next message, no restart needed. <code>ballast wizard</code> can do this for you.</p>
      <p style="margin-bottom: 0">
        <strong>Advanced:</strong> use <code>ballast-webhook-url</code> for a generic JSON webhook, and add
        <code>.&lt;service&gt;</code> to either file name (e.g. <code>ballast-ntfy-topic.photos</code>) to send one
        service somewhere else. These links are kept out of the web UI because anyone who has them can read or send
        your alerts.
      </p>
    </div>
  </details>

  <p v-if="error" class="notice notice--error" role="alert">{{ error }}</p>
  <p v-if="loading" class="loading">Loading…</p>

  <template v-else>
    <div v-if="services.length === 0" class="empty">
      <p>Nothing is being backed up yet.</p>
      <router-link class="btn btn-primary" to="/wizard">Set up a backup</router-link>
    </div>

    <ul v-else class="list">
      <li v-for="s in services" :key="s.name">
        <form v-if="settings[s.name]" @submit.prevent="save(s.name)">
          <div class="item-title" style="margin-bottom: 0.6rem">{{ s.name }}</div>
          <label class="check">
            <input v-model="settings[s.name].enabled" type="checkbox" />
            <span class="check-text">
              Tell me if backups stop happening
              <span class="hint">For example if the server was off or the schedule was paused by mistake.</span>
            </span>
          </label>
          <label class="check">
            <input v-model="settings[s.name].heartbeat_enabled" type="checkbox" />
            <span class="check-text">
              Also message me after every good backup
              <span class="hint">So if the messages stop, you know something's wrong.</span>
            </span>
          </label>
          <details class="advanced" :open="!!maxAgeInput[s.name]">
            <summary>Advanced</summary>
            <div class="advanced-body">
              <div class="field" style="max-width: 280px; margin-bottom: 0.4rem">
                <label :for="`max-age-${s.name}`">Hours without a good backup before telling you</label>
                <input :id="`max-age-${s.name}`" v-model="maxAgeInput[s.name]" inputmode="numeric" placeholder="automatic" />
              </div>
              <p class="hint">Leave blank to work it out from the schedule: 2 days for nightly backups, 9 days for weekly.</p>
            </div>
          </details>
          <div class="actions" style="margin-top: 0.85rem">
            <button class="btn btn-primary" type="submit" :disabled="saving.has(s.name)">
              {{ saving.has(s.name) ? "Saving…" : "Save" }}
            </button>
            <span v-if="saved.has(s.name)" class="muted" role="status">Saved.</span>
          </div>
        </form>
      </li>
    </ul>
  </template>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref } from "vue";

import { api } from "@/api/client";
import type { ChannelView, NotificationSettings, Service } from "@/api/types";
import { buildChannelInput, defaultNtfyUrl } from "@/lib/channels";

// --- Notification Channels ---
const channels = ref<ChannelView[]>([]);
const showChannelForm = ref(false);
const channelError = ref("");
const channelLoading = ref(true);
const channelSaving = ref(false);
const channelTesting = ref<Set<string>>(new Set());
const channelTestResult = reactive<Record<string, { ok: boolean; text: string }>>({});
const editingChannelName = ref<string | null>(null);

interface ChannelForm {
  name: string;
  url: string;
  username: string;
  token: string;
  clearToken: boolean;
  applyToAll: boolean;
  services: string[];
}

function blankChannelForm(): ChannelForm {
  return {
    name: "",
    url: defaultNtfyUrl,
    username: "",
    token: "",
    clearToken: false,
    applyToAll: true,
    services: [],
  };
}

const channelForm = reactive<ChannelForm>(blankChannelForm());

// --- Per-Service Notifications ---
const services = ref<Service[]>([]);
const settings = reactive<Record<string, NotificationSettings>>({});
const maxAgeInput = reactive<Record<string, string>>({});
const loading = ref(true);
const error = ref("");
const saving = ref<Set<string>>(new Set());
const saved = ref<Set<string>>(new Set());

// Computed list of service names (for channel form service picker)
const serviceNames = computed(() => services.value.map((s) => s.name));

// --- Channel Methods ---
async function loadChannels() {
  channelLoading.value = true;
  channelError.value = "";
  try {
    channels.value = await api.listChannels();
  } catch (e) {
    channelError.value = (e as Error).message;
  } finally {
    channelLoading.value = false;
  }
}

function openChannelForm() {
  Object.assign(channelForm, blankChannelForm());
  editingChannelName.value = null;
  showChannelForm.value = true;
}

function editChannel(ch: ChannelView) {
  editingChannelName.value = ch.name;
  channelForm.name = ch.name;
  channelForm.url = ch.url_display;
  channelForm.username = ch.username || "";
  channelForm.token = "";
  channelForm.clearToken = false;
  channelForm.applyToAll = ch.apply_to_all;
  channelForm.services = [...ch.services];
  showChannelForm.value = true;
}

function validateChannel(): string {
  if (!channelForm.name.trim()) return "Give this notification channel a name.";
  if (!channelForm.url.trim()) return "Enter the ntfy address.";
  if (!channelForm.applyToAll && channelForm.services.length === 0) {
    return "Select at least one service or turn on 'Apply to all services'.";
  }
  return "";
}

async function submitChannel() {
  channelError.value = "";
  const problem = validateChannel();
  if (problem) {
    channelError.value = problem;
    return;
  }
  channelSaving.value = true;
  try {
    const input = buildChannelInput(
      channelForm.name,
      channelForm.url,
      channelForm.username || undefined,
      channelForm.token || undefined,
      channelForm.clearToken,
      channelForm.applyToAll,
      channelForm.services,
    );

    if (editingChannelName.value) {
      await api.updateChannel(editingChannelName.value, input);
    } else {
      await api.createChannel(input);
    }
    showChannelForm.value = false;
    editingChannelName.value = null;
    await loadChannels();
  } catch (e) {
    channelError.value = (e as Error).message;
  } finally {
    channelSaving.value = false;
  }
}

async function testChannel(name: string) {
  channelTesting.value.add(name);
  delete channelTestResult[name];
  try {
    await api.testChannel(name);
    channelTestResult[name] = { ok: true, text: "Sent — check your phone." };
  } catch (e) {
    channelTestResult[name] = { ok: false, text: (e as Error).message };
  } finally {
    channelTesting.value.delete(name);
  }
}

async function deleteChannel(name: string) {
  if (!confirm(`Delete the "${name}" notification channel? Any service using it will lose notifications.`)) {
    return;
  }
  channelError.value = "";
  try {
    await api.deleteChannel(name);
    await loadChannels();
  } catch (e) {
    channelError.value = (e as Error).message;
  }
}

// --- Per-Service Notification Methods ---
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

onMounted(() => {
  load();
  loadChannels();
});
</script>

<template>
  <h2>Notifications</h2>
  <p class="lede">Ballast always tells you when a backup fails. Choose what else you want to hear about.</p>

  <!-- Notification Channels Section -->
  <section style="margin-bottom: 2rem">
    <h3>Where messages go</h3>
    <p>Set up your notification channels (like ntfy for push notifications). Then pick which services use each one below.</p>

    <p v-if="channelError" class="notice notice--error" role="alert">{{ channelError }}</p>
    <p v-if="channelLoading" class="loading">Loading channels…</p>

    <template v-else>
      <ul v-if="channels.length" class="list">
        <li v-for="ch in channels" :key="ch.name">
          <div class="item-head">
            <div>
              <div class="item-title">{{ ch.name }}</div>
              <div class="item-sub">
                {{ ch.url_display }}
                <span v-if="ch.has_token" class="badge">protected</span>
              </div>
              <div v-if="!ch.apply_to_all && ch.services.length" class="item-sub hint" style="margin-top: 0.3rem">
                {{ ch.services.join(", ") }}
              </div>
              <div v-else-if="ch.apply_to_all" class="item-sub hint" style="margin-top: 0.3rem">All services</div>
            </div>
            <div class="actions">
              <button class="btn" type="button" :disabled="channelTesting.has(ch.name)" @click="testChannel(ch.name)">
                {{ channelTesting.has(ch.name) ? "Sending…" : "Test" }}
              </button>
              <button class="btn" type="button" @click="editChannel(ch)">Edit</button>
              <button class="btn btn-quiet btn-danger" type="button" @click="deleteChannel(ch.name)">Delete</button>
            </div>
          </div>
          <p
            v-if="channelTestResult[ch.name]"
            class="notice"
            :class="channelTestResult[ch.name].ok ? 'notice--ok' : 'notice--error'"
            style="margin: 0.6rem 0 0"
          >
            {{ channelTestResult[ch.name].text }}
          </p>
        </li>
      </ul>

      <button v-if="!showChannelForm" class="btn" type="button" @click="openChannelForm">Add a channel</button>

      <form v-else class="panel" @submit.prevent="submitChannel">
        <h4>{{ editingChannelName ? "Edit channel" : "Add a notification channel" }}</h4>

        <div class="field">
          <label for="ch-name">Name</label>
          <input id="ch-name" v-model="channelForm.name" placeholder="e.g. my-ntfy" :disabled="!!editingChannelName" />
          <p v-if="editingChannelName" class="hint">Channel name cannot be changed.</p>
        </div>

        <div class="field">
          <label for="ch-url">ntfy address</label>
          <input
            id="ch-url"
            v-model="channelForm.url"
            autocomplete="off"
            placeholder="https://ntfy.sh/my-secret-topic"
          />
          <p class="hint">
            Default is <code>https://ntfy.sh/</code>. For self-hosted ntfy, use <code>https://ntfy.example.com/my-topic</code>. The topic acts like a password.
          </p>
        </div>

        <div class="field">
          <label for="ch-user">Username (optional)</label>
          <input id="ch-user" v-model="channelForm.username" autocomplete="off" />
          <p class="hint">If your ntfy instance requires authentication.</p>
        </div>

        <div class="field">
          <label for="ch-token">Access token or password (optional)</label>
          <input id="ch-token" v-model="channelForm.token" type="password" autocomplete="off" />
          <p class="hint">If your ntfy instance requires authentication. Blank when editing = keep existing.</p>
        </div>

        <label v-if="editingChannelName && channelForm.token === ''" class="check">
          <input v-model="channelForm.clearToken" type="checkbox" />
          <span class="check-text">Remove the saved token</span>
        </label>

        <label class="choice" style="margin-top: 0.8rem">
          <input v-model="channelForm.applyToAll" type="radio" :value="true" />
          <span class="check-text">Apply to all services</span>
        </label>

        <label class="choice">
          <input v-model="channelForm.applyToAll" type="radio" :value="false" />
          <span class="check-text">Apply to specific services</span>
        </label>

        <fieldset v-if="!channelForm.applyToAll" style="margin: 0.8rem 0 1rem; border: none; padding: 0">
          <legend class="hint" style="margin-bottom: 0.5rem">Choose services:</legend>
          <label v-for="svc in serviceNames" :key="svc" class="check">
            <input v-model="channelForm.services" type="checkbox" :value="svc" />
            <span class="check-text">{{ svc }}</span>
          </label>
        </fieldset>

        <div class="actions" style="margin-top: 1rem">
          <button class="btn btn-primary" type="submit" :disabled="channelSaving">
            {{ channelSaving ? "Saving…" : editingChannelName ? "Update" : "Add" }}
          </button>
          <button v-if="channels.length || editingChannelName" class="btn btn-quiet" type="button" @click="showChannelForm = false">
            Cancel
          </button>
        </div>
      </form>
    </template>
  </section>

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

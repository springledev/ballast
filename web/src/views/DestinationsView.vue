<script setup lang="ts">
import { computed, onBeforeUnmount, reactive, ref } from "vue";

import { api } from "@/api/client";
import type { Destination, DestinationType, ProtonSignIn } from "@/api/types";
import { buildSettings, credentialKind, destinationTypes, isCopyOnly, typeLabel, type DestinationFormFields } from "@/lib/destinations";
import { describeRetention } from "@/lib/retention";

const destinations = ref<Destination[]>([]);
const loading = ref(true);
const error = ref("");
const saving = ref(false);
const showForm = ref(false);
const showAdvancedType = ref(false);
const created = ref<Destination | null>(null);
const checking = ref<Set<string>>(new Set());
const checkResult = reactive<Record<string, { ok: boolean; text: string }>>({});
const initializing = ref<Set<string>>(new Set());
const initResult = reactive<Record<string, { ok: boolean; text: string }>>({});

// Proton Drive sign-ins in progress, by destination name.
const signIns = reactive<Record<string, ProtonSignIn>>({});
const signInTimers: Record<string, number> = {};

async function signIn(name: string) {
  error.value = "";
  window.clearTimeout(signInTimers[name]);
  try {
    signIns[name] = await api.protonSignIn(name);
    pollSignIn(name);
  } catch (e) {
    error.value = (e as Error).message;
  }
}

function pollSignIn(name: string) {
  signInTimers[name] = window.setTimeout(async () => {
    try {
      signIns[name] = await api.protonSignInStatus(name);
      if (!signIns[name].done) pollSignIn(name);
    } catch (e) {
      signIns[name] = { done: true, error: (e as Error).message };
    }
  }, 1500);
}

onBeforeUnmount(() => Object.values(signInTimers).forEach((t) => window.clearTimeout(t)));

interface Form extends DestinationFormFields {
  name: string;
  type: DestinationType;
  init: boolean;
  keep_last: number;
  keep_daily: number;
  keep_weekly: number;
  keep_monthly: number;
  access_key_id: string;
  secret_access_key: string;
  ssh_private_key: string;
}

function blankForm(): Form {
  return {
    name: "",
    type: "local",
    path: "",
    endpoint: "",
    bucket: "",
    region: "",
    account_id: "",
    host: "",
    port: "",
    user: "",
    url: "",
    max_artifact_mb: "",
    init: true,
    keep_last: 0,
    keep_daily: 7,
    keep_weekly: 4,
    keep_monthly: 6,
    access_key_id: "",
    secret_access_key: "",
    ssh_private_key: "",
  };
}

const form = reactive<Form>(blankForm());

const credKind = computed(() => credentialKind(form.type));

async function load() {
  loading.value = true;
  error.value = "";
  try {
    destinations.value = await api.listDestinations();
    if (destinations.value.length === 0) showForm.value = true;
  } catch (e) {
    error.value = (e as Error).message;
  } finally {
    loading.value = false;
  }
}

function openForm() {
  Object.assign(form, blankForm());
  showAdvancedType.value = false;
  created.value = null;
  showForm.value = true;
}

function validate(): string {
  if (!form.name) return "Give this place a name.";
  switch (form.type) {
    case "local":
      if (!form.path) return "Enter the folder to keep backups in.";
      break;
    case "b2":
      if (!form.endpoint) return "Enter the bucket's S3 endpoint (shown on the bucket's page in Backblaze).";
      if (!form.bucket) return "Enter the bucket name.";
      break;
    case "s3":
      if (!form.endpoint) return "Enter the provider's endpoint.";
      if (!form.bucket) return "Enter the bucket name.";
      break;
    case "r2":
      if (!form.account_id) return "Enter your Cloudflare account ID.";
      if (!form.bucket) return "Enter the bucket name.";
      break;
    case "sftp":
      if (!form.host) return "Enter the server's host name or IP address.";
      if (!form.user) return "Enter the user name to log in as.";
      if (!form.path) return "Enter the folder on the server to store backups in.";
      break;
    case "custom":
      if (!form.url) return "Enter the restic repository string.";
      break;
    case "proton-drive":
      if (form.path && !form.path.startsWith("/")) return "The Proton Drive folder must start with / (for example /my-files/ballast).";
      break;
  }
  if (credKind.value === "keypair" && (form.access_key_id || form.secret_access_key)) {
    if (!form.access_key_id || !form.secret_access_key) return "Enter both the access key ID and the secret access key.";
  }
  return "";
}

async function submit() {
  error.value = "";
  const problem = validate();
  if (problem) {
    error.value = problem;
    return;
  }
  saving.value = true;
  try {
    const settings = buildSettings(form.type, form);
    const credentials =
      credKind.value === "keypair" && form.access_key_id && form.secret_access_key
        ? { access_key_id: form.access_key_id, secret_access_key: form.secret_access_key }
        : credKind.value === "ssh" && form.ssh_private_key
          ? { ssh_private_key: form.ssh_private_key }
          : undefined;
    const saved = await api.createDestination({
      name: form.name,
      type: form.type,
      settings,
      keep_last: form.keep_last,
      keep_daily: form.keep_daily,
      keep_weekly: form.keep_weekly,
      keep_monthly: form.keep_monthly,
      init: form.init && !isCopyOnly(form.type),
      credentials,
    });
    created.value = saved;
    showForm.value = false;
    await load();
  } catch (e) {
    error.value = (e as Error).message;
  } finally {
    saving.value = false;
  }
}

async function check(name: string) {
  checking.value.add(name);
  delete checkResult[name];
  try {
    await api.testDestination(name);
    checkResult[name] = { ok: true, text: "Ballast can reach it and open the backups." };
  } catch (e) {
    checkResult[name] = { ok: false, text: (e as Error).message };
  } finally {
    checking.value.delete(name);
  }
}

async function initRepo(name: string) {
  initializing.value.add(name);
  delete initResult[name];
  try {
    await api.initDestination(name);
    initResult[name] = { ok: true, text: "The repository is ready." };
  } catch (e) {
    initResult[name] = { ok: false, text: (e as Error).message };
  } finally {
    initializing.value.delete(name);
  }
}

async function remove(name: string) {
  if (!confirm(`Forget "${name}"? The backups stored there are not deleted. Anything still backing up to it must be moved first.`)) {
    return;
  }
  error.value = "";
  try {
    await api.deleteDestination(name);
    await load();
  } catch (e) {
    error.value = (e as Error).message;
  }
}

load();
</script>

<template>
  <header class="page-head">
    <h2>Destinations</h2>
    <p class="lede">
      Places Ballast keeps your backups. Everything stored is encrypted. A folder on another disk or a mounted NAS
      share is a good start. The command line calls these "destinations".
    </p>
  </header>

  <p v-if="error" class="notice notice--error" role="alert">{{ error }}</p>
  <div v-if="created" class="notice notice--ok" role="status">
    <p><strong>{{ created.name }}</strong> is ready.</p>
    <p v-if="created.type === 'proton-drive'">
      Next, choose <strong>Sign in to Proton Drive</strong> below. Your Proton password is only ever typed on Proton's
      own page, never into Ballast.
    </p>
    <p v-else-if="created.type === 'local'">
      Its encryption password is saved on the server as <code>{{ created.password_credential }}</code> in Ballast's
      credential folder. <strong>Keep a copy somewhere safe, away from this machine</strong>: without it, nobody can
      restore these backups.
    </p>
  </div>
  <p v-if="loading" class="loading">Loading…</p>

  <template v-else>
    <ul v-if="destinations.length" class="list">
      <li v-for="d in destinations" :key="d.name">
        <div class="item-head">
          <div>
            <div class="item-title">{{ d.name }}</div>
            <div class="item-sub">{{ typeLabel(d.type) }} · {{ d.location }}</div>
          </div>
          <div class="actions">
            <button class="btn" type="button" :disabled="checking.has(d.name)" @click="check(d.name)">
              {{ checking.has(d.name) ? "Checking…" : "Test connection" }}
            </button>
            <button v-if="isCopyOnly(d.type)" class="btn" type="button" @click="signIn(d.name)">
              Sign in to Proton Drive
            </button>
            <button v-else class="btn" type="button" :disabled="initializing.has(d.name)" @click="initRepo(d.name)">
              {{ initializing.has(d.name) ? "Setting up…" : "Set up repository" }}
            </button>
            <button class="btn btn-quiet btn-danger" type="button" @click="remove(d.name)">Forget</button>
          </div>
        </div>
        <p
          v-if="checkResult[d.name]"
          class="notice"
          :class="checkResult[d.name].ok ? 'notice--ok' : 'notice--error'"
          style="margin: 0.6rem 0 0"
        >
          {{ checkResult[d.name].text }}
        </p>
        <div v-if="signIns[d.name]" class="notice" :class="signIns[d.name].error ? 'notice--error' : signIns[d.name].done ? 'notice--ok' : ''" role="status" style="margin: 0.6rem 0 0">
          <p v-if="signIns[d.name].error">{{ signIns[d.name].error }}</p>
          <p v-else-if="signIns[d.name].done">Signed in. Proton Drive is ready to receive copies.</p>
          <template v-else-if="signIns[d.name].link">
            <p>
              Open <a :href="signIns[d.name].link" target="_blank" rel="noopener">this Proton page</a> (on any device) and
              sign in there.
            </p>
            <p v-if="signIns[d.name].code">If it asks for a code, enter <strong>{{ signIns[d.name].code }}</strong>.</p>
            <p class="hint">Ballast waits here and never sees your password or 2FA code.</p>
          </template>
          <p v-else>Starting the Proton sign-in…</p>
        </div>
        <p
          v-if="initResult[d.name]"
          class="notice"
          :class="initResult[d.name].ok ? 'notice--ok' : 'notice--error'"
          style="margin: 0.6rem 0 0"
        >
          {{ initResult[d.name].text }}
        </p>
        <details class="advanced">
          <summary>Details</summary>
          <div class="advanced-body">
            <dl class="kv">
              <dt>Type</dt>
              <dd>{{ typeLabel(d.type) }}</dd>
              <dt v-if="d.password_credential">Password file</dt>
              <dd v-if="d.password_credential"><code>{{ d.password_credential }}</code></dd>
              <dt v-if="d.backend_credential">Backend credential file</dt>
              <dd v-if="d.backend_credential"><code>{{ d.backend_credential }}</code></dd>
              <dt>Old backups kept</dt>
              <dd>{{ describeRetention(d) }}</dd>
            </dl>
            <p v-if="isCopyOnly(d.type)" class="hint">
              This is a second copy of small files (configs and database backups). It never replaces your main backup,
              and a problem here shows as amber without failing the backup.
            </p>
            <p v-else class="hint">
              "Set up repository" is only needed if this destination was added without setting it up, or if the
              storage it points at was emptied since — it's safe to use any time, even if it's already set up.
            </p>
          </div>
        </details>
      </li>
    </ul>

    <button v-if="!showForm" class="btn" type="button" @click="openForm">Add a place</button>

    <form v-else class="panel" @submit.prevent="submit">
      <h3>Add a place for backups</h3>

      <div class="field">
        <label for="dest-name">Name</label>
        <input id="dest-name" v-model="form.name" placeholder="e.g. nas" />
      </div>

      <div class="field">
        <div class="field-label">Where should backups be kept?</div>
        <label v-for="t in destinationTypes.filter((t) => !t.advanced)" :key="t.id" class="choice">
          <input v-model="form.type" type="radio" name="dest-type" :value="t.id" />
          <span class="check-text"><strong>{{ t.label }}</strong></span>
        </label>
        <details class="advanced" :open="showAdvancedType || form.type === 'custom'">
          <summary>Advanced</summary>
          <div class="advanced-body">
            <label v-for="t in destinationTypes.filter((t) => t.advanced)" :key="t.id" class="choice">
              <input v-model="form.type" type="radio" name="dest-type" :value="t.id" />
              <span class="check-text">
                <strong>{{ t.label }}</strong>
                <span v-if="t.hint" class="hint">{{ t.hint }}</span>
              </span>
            </label>
          </div>
        </details>
      </div>

      <!-- Local -->
      <template v-if="form.type === 'local'">
        <div class="field">
          <label for="dest-path">Folder</label>
          <input id="dest-path" v-model="form.path" placeholder="/mnt/backups" autocomplete="off" />
          <p class="hint">A full path on the Ballast server. It's created if it doesn't exist.</p>
        </div>
        <p class="hint" style="margin-bottom: 1rem">
          Ballast creates the encryption password for you. You'll be shown where it's saved.
        </p>
      </template>

      <!-- B2 -->
      <template v-else-if="form.type === 'b2'">
        <div class="field">
          <label for="dest-endpoint">Bucket endpoint</label>
          <input id="dest-endpoint" v-model="form.endpoint" placeholder="s3.us-west-004.backblazeb2.com" autocomplete="off" />
          <p class="hint">Shown on the bucket's page in the Backblaze web console.</p>
        </div>
        <div class="field">
          <label for="dest-bucket">Bucket name</label>
          <input id="dest-bucket" v-model="form.bucket" autocomplete="off" />
        </div>
        <div class="field">
          <label for="dest-path-opt">Folder in bucket (optional)</label>
          <input id="dest-path-opt" v-model="form.path" autocomplete="off" />
        </div>
      </template>

      <!-- S3 -->
      <template v-else-if="form.type === 's3'">
        <div class="field">
          <label for="dest-endpoint">Endpoint</label>
          <input id="dest-endpoint" v-model="form.endpoint" placeholder="s3.amazonaws.com or https://minio.lan:9000" autocomplete="off" />
        </div>
        <div class="field">
          <label for="dest-bucket">Bucket name</label>
          <input id="dest-bucket" v-model="form.bucket" autocomplete="off" />
        </div>
        <div class="field">
          <label for="dest-region">Region (optional)</label>
          <input id="dest-region" v-model="form.region" placeholder="us-east-1" autocomplete="off" />
        </div>
        <div class="field">
          <label for="dest-path-opt">Folder in bucket (optional)</label>
          <input id="dest-path-opt" v-model="form.path" autocomplete="off" />
        </div>
      </template>

      <!-- R2 -->
      <template v-else-if="form.type === 'r2'">
        <div class="field">
          <label for="dest-account">Cloudflare account ID</label>
          <input id="dest-account" v-model="form.account_id" autocomplete="off" />
          <p class="hint">Shown on the R2 overview page.</p>
        </div>
        <div class="field">
          <label for="dest-bucket">Bucket name</label>
          <input id="dest-bucket" v-model="form.bucket" autocomplete="off" />
        </div>
        <div class="field">
          <label for="dest-path-opt">Folder in bucket (optional)</label>
          <input id="dest-path-opt" v-model="form.path" autocomplete="off" />
        </div>
      </template>

      <!-- SFTP -->
      <template v-else-if="form.type === 'sftp'">
        <div class="field">
          <label for="dest-host">Server host name or IP</label>
          <input id="dest-host" v-model="form.host" autocomplete="off" />
        </div>
        <div class="field">
          <label for="dest-port">Port (optional)</label>
          <input id="dest-port" v-model="form.port" placeholder="22" autocomplete="off" />
        </div>
        <div class="field">
          <label for="dest-user">User name</label>
          <input id="dest-user" v-model="form.user" autocomplete="off" />
        </div>
        <div class="field">
          <label for="dest-path">Folder on the server</label>
          <input id="dest-path" v-model="form.path" placeholder="/srv/backups/ballast" autocomplete="off" />
        </div>
        <p class="hint" style="margin-bottom: 1rem">
          Add the matching public key to that user's <code>~/.ssh/authorized_keys</code> on the server.
        </p>
      </template>

      <!-- Custom -->
      <template v-else-if="form.type === 'custom'">
        <div class="field">
          <label for="dest-url">Restic repository string</label>
          <input id="dest-url" v-model="form.url" placeholder="rest:https://backup.example.com/ballast" autocomplete="off" />
          <p class="hint">
            For setups Ballast doesn't manage credentials for. See the
            <a href="https://restic.readthedocs.io/en/stable/030_preparing_a_new_repo.html" target="_blank" rel="noopener"
              >restic docs</a
            >
            for the format. Credentials must be supplied another way (e.g. environment already set for Ballast).
          </p>
        </div>
      </template>

      <!-- Proton Drive -->
      <template v-else-if="form.type === 'proton-drive'">
        <div class="field">
          <label for="dest-proton-folder">Folder in Proton Drive</label>
          <input id="dest-proton-folder" v-model="form.path" placeholder="/my-files/ballast" autocomplete="off" />
          <p class="hint">
            Ballast only ever adds and removes its own dated folders inside this one. You'll sign in to Proton on the next
            step.
          </p>
        </div>
        <div class="field">
          <label for="dest-proton-max">Largest file to send, in MB (optional)</label>
          <input id="dest-proton-max" v-model="form.max_artifact_mb" placeholder="1024" autocomplete="off" />
          <p class="hint">Bigger files are skipped and listed, so photos and large dumps stay out of Proton Drive.</p>
        </div>
        <p class="hint" style="margin-bottom: 1rem">
          Needs Proton's <code>proton-drive</code> command-line tool on this machine.
          <a href="https://proton.me/drive/download" target="_blank" rel="noopener">Download it from Proton</a>.
        </p>
      </template>

      <!-- Credentials -->
      <template v-if="credKind === 'keypair'">
        <div class="field">
          <label for="dest-key-id">Access key ID</label>
          <input id="dest-key-id" v-model="form.access_key_id" type="password" autocomplete="off" />
          <p v-if="form.type === 'b2'" class="hint">B2 calls this the "keyID".</p>
          <p v-else-if="form.type === 'r2'" class="hint">From an R2 API token.</p>
        </div>
        <div class="field">
          <label for="dest-secret">Secret access key</label>
          <input id="dest-secret" v-model="form.secret_access_key" type="password" autocomplete="off" />
          <p v-if="form.type === 'b2'" class="hint">B2 calls this the "applicationKey".</p>
        </div>
      </template>
      <div v-else-if="credKind === 'ssh'" class="field">
        <label for="dest-ssh-key">SSH private key</label>
        <textarea
          id="dest-ssh-key"
          v-model="form.ssh_private_key"
          rows="6"
          autocomplete="off"
          placeholder="-----BEGIN OPENSSH PRIVATE KEY-----&#10;...&#10;-----END OPENSSH PRIVATE KEY-----"
          style="width: 100%; font-family: monospace"
        ></textarea>
        <p class="hint">Paste the full unencrypted private key. Ballast runs unattended, so it can't hold a passphrase.</p>
      </div>

      <label v-if="!isCopyOnly(form.type)" class="choice" style="margin-top: 0.5rem">
        <input v-model="form.init" type="checkbox" />
        <span class="check-text">Set up the storage now (creates the backup repository if it doesn't exist yet)</span>
      </label>

      <details class="advanced">
        <summary>Advanced: password file, how long to keep backups</summary>
        <div class="advanced-body">
          <div class="field-label">Old backups to keep</div>
          <div class="inline-fields">
            <div class="field">
              <label for="dest-last" class="hint">Newest (any age)</label>
              <input id="dest-last" v-model.number="form.keep_last" type="number" min="0" />
            </div>
            <div v-if="!isCopyOnly(form.type)" class="field">
              <label for="dest-daily" class="hint">Daily</label>
              <input id="dest-daily" v-model.number="form.keep_daily" type="number" min="0" />
            </div>
            <div v-if="!isCopyOnly(form.type)" class="field">
              <label for="dest-weekly" class="hint">Weekly</label>
              <input id="dest-weekly" v-model.number="form.keep_weekly" type="number" min="0" />
            </div>
            <div v-if="!isCopyOnly(form.type)" class="field">
              <label for="dest-monthly" class="hint">Monthly</label>
              <input id="dest-monthly" v-model.number="form.keep_monthly" type="number" min="0" />
            </div>
          </div>
        </div>
      </details>

      <div class="actions" style="margin-top: 1rem">
        <button class="btn btn-primary" type="submit" :disabled="saving">{{ saving ? "Setting up…" : "Add" }}</button>
        <button v-if="destinations.length" class="btn btn-quiet" type="button" @click="showForm = false">Cancel</button>
      </div>
    </form>
  </template>
</template>

<script setup lang="ts">
import { reactive, ref } from "vue";

import { api } from "@/api/client";
import type { ProxmoxConnection, ProxmoxGuest } from "@/api/types";
import { askConfirm } from "@/lib/confirm";
import { revealOnError } from "@/lib/revealError";

const connection = ref<ProxmoxConnection | null>(null);
const loading = ref(true);
const error = ref("");
revealOnError(error);
const saving = ref(false);
const testResult = ref("");
const testing = ref(false);

const guests = ref<ProxmoxGuest[]>([]);
const guestsError = ref("");
const guestsLoading = ref(false);

interface Form {
  host: string;
  token_id: string;
  token_secret: string;
  insecure: boolean;
  skip_test: boolean;
}

function blankForm(): Form {
  return { host: "", token_id: "", token_secret: "", insecure: false, skip_test: false };
}

const form = reactive<Form>(blankForm());
const created = ref("");

async function loadGuests() {
  guestsLoading.value = true;
  guestsError.value = "";
  try {
    guests.value = await api.listProxmoxGuests();
  } catch (e) {
    guestsError.value = (e as Error).message;
  } finally {
    guestsLoading.value = false;
  }
}

async function load() {
  loading.value = true;
  error.value = "";
  guests.value = [];
  guestsError.value = "";
  try {
    connection.value = await api.getProxmoxConnection();
    await loadGuests();
  } catch {
    // No connection configured yet — that's a normal state, not an error.
    connection.value = null;
  } finally {
    loading.value = false;
  }
}

async function submit() {
  error.value = "";
  if (!form.host) {
    error.value = "Enter the Proxmox host address.";
    return;
  }
  if (!form.token_id) {
    error.value = "Enter the API token ID.";
    return;
  }
  saving.value = true;
  try {
    const res = await api.setupProxmoxConnection({
      host: form.host,
      token_id: form.token_id,
      insecure: form.insecure,
      credentials: form.token_secret ? { token_secret: form.token_secret } : undefined,
      skip_test: form.skip_test,
    });
    connection.value = res.connection;
    created.value = res.test?.version ? `Connected. Proxmox VE version ${res.test.version}.` : "Saved.";
    // Never keep the pasted token secret around once it's saved.
    form.token_secret = "";
    Object.assign(form, blankForm());
    await loadGuests();
  } catch (e) {
    error.value = (e as Error).message;
  } finally {
    saving.value = false;
  }
}

async function test() {
  testing.value = true;
  testResult.value = "";
  error.value = "";
  try {
    const res = await api.testProxmoxConnection();
    testResult.value = `Connected. Proxmox VE version ${res.version}.`;
  } catch (e) {
    testResult.value = "";
    error.value = (e as Error).message;
  } finally {
    testing.value = false;
  }
}

async function remove() {
  if (!(await askConfirm("Remove the Proxmox connection? Any service attached to a guest will keep its attachment, but Ballast won't be able to reach the host anymore.", "Remove connection"))) {
    return;
  }
  error.value = "";
  try {
    await api.deleteProxmoxConnection();
    connection.value = null;
    guests.value = [];
    created.value = "";
    testResult.value = "";
  } catch (e) {
    error.value = (e as Error).message;
  }
}

load();
</script>

<template>
  <h2>Proxmox</h2>
  <p class="lede">
    Connect Ballast to a Proxmox VE host so it can see the VMs and LXC containers running there. This is the first
    step toward backing up whole guests, not just the containers inside them — but that backup orchestration isn't
    built yet. For now, this page only sets up the connection and shows what's on the host.
  </p>

  <p v-if="error" class="notice notice--error" role="alert">{{ error }}</p>
  <p v-if="loading" class="loading">Loading…</p>

  <template v-else>
    <div v-if="!connection" class="panel">
      <div v-if="created" class="notice notice--ok" role="status">{{ created }}</div>
      <h3>Connect a Proxmox host</h3>

      <details class="advanced">
        <summary>How do I get a token?</summary>
        <div class="advanced-body">
          <ol style="margin: 0; padding-left: 1.2rem">
            <li>In the Proxmox web UI, go to <strong>Datacenter → Permissions → API Tokens</strong>.</li>
            <li>
              Click <strong>Add</strong>, pick (or create) a user like <code>ballast@pve</code>, and give the token
              an ID such as <code>backup</code>. Untick "Privilege Separation" only if you want the token to inherit
              the user's full permissions — otherwise grant the role below to the token itself.
            </li>
            <li>
              Copy the token secret shown once — Proxmox won't show it again — and paste it below.
            </li>
            <li>
              Under <strong>Datacenter → Permissions</strong>, add a permission for that user/token on path
              <code>/vms</code> with the <code>VM.Backup</code> role (or a custom role containing that privilege).
            </li>
          </ol>
        </div>
      </details>

      <div class="field">
        <label for="pve-host">Host address</label>
        <input id="pve-host" v-model="form.host" placeholder="192.168.1.10 or https://192.168.1.10:8006" autocomplete="off" />
        <p class="hint">The IP address or hostname of your Proxmox VE server. The port defaults to 8006.</p>
      </div>

      <div class="field">
        <label for="pve-token-id">Token ID</label>
        <input id="pve-token-id" v-model="form.token_id" placeholder="ballast@pve!backup" autocomplete="off" />
        <p class="hint">In the form <code>user@realm!tokenname</code>, as shown next to the token in Proxmox.</p>
      </div>

      <div class="field">
        <label for="pve-token-secret">Token secret</label>
        <input id="pve-token-secret" v-model="form.token_secret" type="password" autocomplete="off" />
        <p class="hint">Shown once by Proxmox when the token is created. Ballast stores it in a root-only file, never in the database.</p>
      </div>

      <details class="advanced">
        <summary>Advanced</summary>
        <div class="advanced-body">
          <label class="choice">
            <input v-model="form.insecure" type="checkbox" />
            <span class="check-text">
              <strong>Skip TLS certificate verification</strong>
              <span class="hint">
                Proxmox ships with a self-signed certificate by default, which will fail verification. The safer fix
                is to install a proper certificate (e.g. via Let's Encrypt) on the Proxmox host; only tick this if
                you can't do that and you trust your network.
              </span>
            </span>
          </label>
          <label class="choice">
            <input v-model="form.skip_test" type="checkbox" />
            <span class="check-text">Save without testing the connection first</span>
          </label>
        </div>
      </details>

      <div class="actions" style="margin-top: 1rem">
        <button class="btn btn-primary" type="button" :disabled="saving" @click="submit">
          {{ saving ? "Connecting…" : "Connect" }}
        </button>
      </div>
    </div>

    <template v-else>
      <div class="panel">
        <div class="item-head">
          <div>
            <div class="item-title">{{ connection.host }}</div>
            <div class="item-sub">Token {{ connection.token_id }}<template v-if="connection.insecure"> · TLS verification skipped</template></div>
          </div>
          <div class="actions">
            <button class="btn" type="button" :disabled="testing" @click="test">
              {{ testing ? "Checking…" : "Test connection" }}
            </button>
            <button class="btn btn-quiet btn-danger" type="button" @click="remove">Remove</button>
          </div>
        </div>
        <p v-if="testResult" class="notice notice--ok" role="status" style="margin: 0.6rem 0 0">{{ testResult }}</p>
      </div>

      <h3>Guests on this host</h3>
      <p v-if="guestsLoading" class="loading">Loading guests…</p>
      <p v-else-if="guestsError" class="notice notice--error" role="alert">
        Couldn't list guests: {{ guestsError }}
      </p>
      <p v-else-if="!guests.length">No VMs or LXC containers found on this host.</p>
      <div v-else class="table-scroll">
        <table class="stack">
          <thead>
            <tr>
              <th>Name</th>
              <th>Type</th>
              <th>VMID</th>
              <th>Node</th>
              <th>Status</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="g in guests" :key="`${g.node}-${g.vmid}`">
              <td data-label="Name">{{ g.name }}</td>
              <td data-label="Type">{{ g.type === "lxc" ? "LXC container" : "VM" }}</td>
              <td data-label="VMID">{{ g.vmid }}</td>
              <td data-label="Node">{{ g.node }}</td>
              <td data-label="Status">{{ g.status }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </template>
  </template>
</template>

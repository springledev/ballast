<script setup lang="ts">
import { computed, reactive, ref } from "vue";

import { api } from "@/api/client";
import type { DockerHost, DockerHostKind } from "@/api/types";
import { addressHelp, addressPlaceholder, buildCredentials, credentialKind, dockerHostKinds, kindLabel, type DockerHostFormFields } from "@/lib/dockerhosts";
import { askConfirm } from "@/lib/confirm";
import { revealOnError } from "@/lib/revealError";

const hosts = ref<DockerHost[]>([]);
const loading = ref(true);
const error = ref("");
revealOnError(error);
const saving = ref(false);
const showForm = ref(false);
const created = ref<DockerHost | null>(null);
const createdVersion = ref("");
const checking = ref<Set<string>>(new Set());
const checkResult = reactive<Record<string, { ok: boolean; text: string }>>({});

interface Form extends DockerHostFormFields {
  name: string;
  kind: DockerHostKind;
  address: string;
  skip_test: boolean;
}

function blankForm(): Form {
  return {
    name: "",
    kind: "remote-ssh",
    address: "",
    ssh_private_key: "",
    ca_cert: "",
    client_cert: "",
    client_key: "",
    skip_test: false,
  };
}

const form = reactive<Form>(blankForm());

const credKind = computed(() => credentialKind(form.kind));

async function load() {
  loading.value = true;
  error.value = "";
  try {
    hosts.value = await api.listDockerHosts();
  } catch (e) {
    error.value = (e as Error).message;
  } finally {
    loading.value = false;
  }
}

function openForm() {
  Object.assign(form, blankForm());
  created.value = null;
  showForm.value = true;
}

function validate(): string {
  if (!form.name) return "Give this machine a name.";
  if (form.kind === "remote-ssh" && !form.address) return "Enter the user and host to connect to.";
  if (form.kind === "remote-tls" && !form.address) return "Enter the host name or IP address.";
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
    const credentials = buildCredentials(form.kind, form);
    const res = await api.createDockerHost({
      name: form.name,
      kind: form.kind,
      address: form.address,
      credentials,
      skip_test: form.skip_test,
    });
    created.value = res.host;
    createdVersion.value = res.test?.server_version ?? "";
    showForm.value = false;
    // Never keep pasted key material around once it's saved.
    form.ssh_private_key = "";
    form.ca_cert = "";
    form.client_cert = "";
    form.client_key = "";
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
    const res = await api.testDockerHost(name);
    checkResult[name] = { ok: true, text: `Connected. Docker version ${res.server_version}.` };
  } catch (e) {
    checkResult[name] = { ok: false, text: (e as Error).message };
  } finally {
    checking.value.delete(name);
  }
}

async function remove(name: string) {
  if (!(await askConfirm(`Forget "${name}"? Any backup still using it must be moved to another Docker host first.`, "Forget"))) {
    return;
  }
  error.value = "";
  try {
    await api.deleteDockerHost(name);
    await load();
  } catch (e) {
    error.value = (e as Error).message;
  }
}

load();
</script>

<template>
  <h2>Docker hosts</h2>
  <p class="lede">
    The machines Ballast can find containers on. "This machine" is always available. Add another machine here if
    Docker containers you want to back up run somewhere else on your network.
  </p>

  <p v-if="error" class="notice notice--error" role="alert">{{ error }}</p>
  <div v-if="created" class="notice notice--ok" role="status">
    <p><strong>{{ created.name }}</strong> is ready.</p>
    <p v-if="createdVersion">Connected. Docker version {{ createdVersion }}.</p>
  </div>
  <p v-if="loading" class="loading">Loading…</p>

  <template v-else>
    <ul v-if="hosts.length" class="list">
      <li v-for="h in hosts" :key="h.name">
        <div class="item-head">
          <div>
            <div class="item-title">{{ h.name }}</div>
            <div class="item-sub">{{ kindLabel(h.kind) }}<template v-if="h.address"> · {{ h.address }}</template></div>
          </div>
          <div class="actions">
            <button class="btn" type="button" :disabled="checking.has(h.name)" @click="check(h.name)">
              {{ checking.has(h.name) ? "Checking…" : "Test connection" }}
            </button>
            <button v-if="h.kind !== 'local'" class="btn btn-quiet btn-danger" type="button" @click="remove(h.name)">
              Forget
            </button>
          </div>
        </div>
        <p
          v-if="checkResult[h.name]"
          class="notice"
          :class="checkResult[h.name].ok ? 'notice--ok' : 'notice--error'"
          style="margin: 0.6rem 0 0"
        >
          {{ checkResult[h.name].text }}
        </p>
      </li>
    </ul>

    <button v-if="!showForm" class="btn" type="button" @click="openForm">Add a machine</button>

    <form v-else class="panel" @submit.prevent="submit">
      <h3>Add a Docker host</h3>

      <div class="field">
        <label for="host-name">Name</label>
        <input id="host-name" v-model="form.name" placeholder="e.g. nas-server" autocomplete="off" />
      </div>

      <div class="field">
        <div class="field-label">How does Ballast reach it?</div>
        <label v-for="k in dockerHostKinds" :key="k.id" class="choice">
          <input v-model="form.kind" type="radio" name="host-kind" :value="k.id" />
          <span class="check-text">
            <strong>{{ k.label }}</strong>
            <span v-if="k.hint" class="hint">{{ k.hint }}</span>
          </span>
        </label>
      </div>

      <div class="field">
        <label for="host-address">Address</label>
        <input id="host-address" v-model="form.address" :placeholder="addressPlaceholder(form.kind)" autocomplete="off" />
        <p class="hint">{{ addressHelp(form.kind) }}</p>
      </div>

      <!-- SSH -->
      <template v-if="credKind === 'ssh'">
        <details class="advanced">
          <summary>How do I set this up?</summary>
          <div class="advanced-body">
            <ol style="margin: 0; padding-left: 1.2rem">
              <li>
                On the Ballast server, create a dedicated key:
                <code>ssh-keygen -t ed25519 -N '' -f ballast_key</code>
              </li>
              <li>
                Add <code>ballast_key.pub</code> to that user's <code>~/.ssh/authorized_keys</code> on the Docker
                machine.
              </li>
              <li>
                Make sure that user can run Docker: <code>sudo usermod -aG docker USER</code> (then log them out and
                back in).
              </li>
            </ol>
          </div>
        </details>
        <div class="field">
          <label for="host-ssh-key">SSH private key (optional)</label>
          <textarea
            id="host-ssh-key"
            v-model="form.ssh_private_key"
            rows="6"
            autocomplete="off"
            placeholder="-----BEGIN OPENSSH PRIVATE KEY-----&#10;...&#10;-----END OPENSSH PRIVATE KEY-----"
            style="width: 100%; font-family: monospace"
          ></textarea>
          <p class="hint">
            Leave empty to use the Ballast user's own SSH keys (the ones already set up for its account). Only paste
            a key here if Ballast should use a different one just for this host.
          </p>
        </div>
      </template>

      <!-- TLS -->
      <template v-else-if="credKind === 'tls'">
        <div class="field">
          <label for="host-ca">CA certificate</label>
          <textarea
            id="host-ca"
            v-model="form.ca_cert"
            rows="5"
            autocomplete="off"
            placeholder="-----BEGIN CERTIFICATE-----"
            style="width: 100%; font-family: monospace"
          ></textarea>
        </div>
        <div class="field">
          <label for="host-cert">Client certificate</label>
          <textarea
            id="host-cert"
            v-model="form.client_cert"
            rows="5"
            autocomplete="off"
            placeholder="-----BEGIN CERTIFICATE-----"
            style="width: 100%; font-family: monospace"
          ></textarea>
        </div>
        <div class="field">
          <label for="host-key">Client key</label>
          <textarea
            id="host-key"
            v-model="form.client_key"
            rows="5"
            autocomplete="off"
            placeholder="-----BEGIN PRIVATE KEY-----"
            style="width: 100%; font-family: monospace"
          ></textarea>
          <p class="hint">These three PEM files come from setting up the Docker daemon on that machine for TLS.</p>
        </div>
      </template>

      <details class="advanced">
        <summary>Advanced</summary>
        <div class="advanced-body">
          <label class="choice">
            <input v-model="form.skip_test" type="checkbox" />
            <span class="check-text">Save without testing the connection first</span>
          </label>
        </div>
      </details>

      <div class="actions" style="margin-top: 1rem">
        <button class="btn btn-primary" type="submit" :disabled="saving">{{ saving ? "Connecting…" : "Add" }}</button>
        <button v-if="hosts.length" class="btn btn-quiet" type="button" @click="showForm = false">Cancel</button>
      </div>
    </form>
  </template>
</template>

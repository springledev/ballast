<script setup lang="ts">
import { computed, onMounted, reactive, ref } from "vue";

import { api } from "@/api/client";
import type { CatalogSummary, Destination, Service } from "@/api/types";

const services = ref<Service[]>([]);
const catalog = ref<CatalogSummary[]>([]);
const destinations = ref<Destination[]>([]);
const loading = ref(true);
const error = ref("");
const saved = ref("");
const saving = ref(false);
const showForm = ref(false);

const editingName = ref<string | null>(null);
const form = reactive({
  name: "",
  catalog_entry: "",
  destination: "",
  params: {} as Record<string, string>,
});

const selectedEntry = computed(() => catalog.value.find((c) => c.service === form.catalog_entry));

function entryName(id: string): string {
  return catalog.value.find((c) => c.service === id)?.name ?? id;
}

async function load() {
  loading.value = true;
  error.value = "";
  try {
    [services.value, catalog.value, destinations.value] = await Promise.all([
      api.listServices(),
      api.catalog(),
      api.listDestinations(),
    ]);
  } catch (e) {
    error.value = (e as Error).message;
  } finally {
    loading.value = false;
  }
}

function openAdd() {
  editingName.value = null;
  form.name = "";
  form.catalog_entry = "";
  form.destination = destinations.value[0]?.name ?? "";
  form.params = {};
  saved.value = "";
  showForm.value = true;
}

function startEdit(s: Service) {
  editingName.value = s.name;
  form.name = s.name;
  form.catalog_entry = s.catalog_entry;
  form.destination = s.destination;
  form.params = { ...(s.params ?? {}) };
  saved.value = "";
  showForm.value = true;
}

function closeForm() {
  showForm.value = false;
  editingName.value = null;
}

function onEntryChange() {
  const next: Record<string, string> = {};
  for (const p of selectedEntry.value?.parameters ?? []) {
    next[p.name] = form.params[p.name] ?? p.default ?? "";
  }
  form.params = next;
  if (!editingName.value && !form.name) form.name = form.catalog_entry;
}

async function submit() {
  error.value = "";
  if (!form.catalog_entry) return void (error.value = "Choose what you want to back up.");
  if (!form.name) return void (error.value = "Give it a name.");
  if (!form.destination) return void (error.value = "Choose where the backups should go.");
  for (const p of selectedEntry.value?.parameters ?? []) {
    if (p.required && !form.params[p.name]) return void (error.value = `Fill in "${p.prompt || p.name}".`);
  }
  saving.value = true;
  const payload: Service = {
    name: form.name,
    catalog_entry: form.catalog_entry,
    destination: form.destination,
    params: { ...form.params },
  };
  try {
    if (editingName.value) {
      await api.updateService(editingName.value, payload);
      saved.value = `Saved changes to ${form.name}.`;
    } else {
      await api.createService(payload);
      saved.value = `Added ${form.name}. Set when it runs on the "When it runs" page, or back it up now from the Overview.`;
    }
    closeForm();
    await load();
  } catch (e) {
    error.value = (e as Error).message;
  } finally {
    saving.value = false;
  }
}

async function remove(name: string) {
  if (!confirm(`Stop backing up "${name}"? Backups already made are kept; Ballast just stops making new ones.`)) {
    return;
  }
  error.value = "";
  try {
    await api.deleteService(name);
    saved.value = `Stopped backing up ${name}.`;
    await load();
  } catch (e) {
    error.value = (e as Error).message;
  }
}

onMounted(load);
</script>

<template>
  <h2>Backups</h2>
  <p class="lede">Each entry is one app or folder Ballast backs up. The command line calls these "services".</p>

  <p v-if="error" class="notice notice--error" role="alert">{{ error }}</p>
  <p v-if="saved" class="notice notice--ok" role="status">{{ saved }}</p>
  <p v-if="loading" class="loading">Loading…</p>

  <template v-else>
    <div v-if="!services.length && !showForm" class="empty">
      <p>Nothing is being backed up yet.</p>
      <div class="actions" style="justify-content: center">
        <router-link class="btn btn-primary" to="/wizard">Set up with the guide</router-link>
        <button class="btn" type="button" @click="openAdd">Add one here</button>
      </div>
    </div>

    <ul v-if="services.length" class="list">
      <li v-for="s in services" :key="s.name">
        <div class="item-head">
          <div>
            <div class="item-title">{{ s.name }}</div>
            <div class="item-sub">{{ entryName(s.catalog_entry) }}, stored in {{ s.destination }}</div>
          </div>
          <div class="actions">
            <button class="btn" type="button" @click="startEdit(s)">Edit</button>
            <button class="btn btn-quiet btn-danger" type="button" @click="remove(s.name)">Stop backing up</button>
          </div>
        </div>
        <details v-if="s.params && Object.keys(s.params).length" class="advanced">
          <summary>Details</summary>
          <div class="advanced-body">
            <dl class="kv">
              <dt>Catalog entry</dt>
              <dd><code>{{ s.catalog_entry }}</code></dd>
              <template v-for="(v, k) in s.params" :key="k">
                <dt>{{ k }}</dt>
                <dd>{{ v }}</dd>
              </template>
            </dl>
          </div>
        </details>
      </li>
    </ul>

    <button v-if="services.length && !showForm" class="btn" type="button" @click="openAdd">Add another</button>

    <form v-if="showForm" class="panel" @submit.prevent="submit">
      <h3>{{ editingName ? `Edit ${editingName}` : "Back up something new" }}</h3>

      <div class="field">
        <label for="svc-entry">What is it?</label>
        <select id="svc-entry" v-model="form.catalog_entry" @change="onEntryChange">
          <option value="" disabled>Choose…</option>
          <option v-for="c in catalog" :key="c.service" :value="c.service">{{ c.name }}</option>
        </select>
        <p v-if="selectedEntry" class="hint">{{ selectedEntry.description }}</p>
      </div>

      <template v-if="selectedEntry">
        <div class="field">
          <label for="svc-name">Name</label>
          <input id="svc-name" v-model="form.name" :disabled="!!editingName" />
          <p class="hint">Lowercase letters, numbers and dashes, e.g. <code>photos</code>.</p>
        </div>
        <div v-for="p in selectedEntry.parameters ?? []" :key="p.name" class="field">
          <label :for="`param-${p.name}`">{{ p.prompt || p.name }}<span v-if="!p.required" class="muted"> (optional)</span></label>
          <input :id="`param-${p.name}`" v-model="form.params[p.name]" />
        </div>
        <div class="field">
          <label for="svc-dest">Where should backups go?</label>
          <select id="svc-dest" v-model="form.destination">
            <option value="" disabled>Choose…</option>
            <option v-for="d in destinations" :key="d.name" :value="d.name">{{ d.name }} ({{ d.location }})</option>
          </select>
          <p v-if="!destinations.length" class="hint">
            You haven't set up anywhere to keep backups yet. Add a place on
            <router-link to="/destinations">Where backups go</router-link> first.
          </p>
        </div>
      </template>

      <div class="actions">
        <button class="btn btn-primary" type="submit" :disabled="saving">
          {{ saving ? "Saving…" : editingName ? "Save changes" : "Add" }}
        </button>
        <button class="btn btn-quiet" type="button" @click="closeForm">Cancel</button>
      </div>
    </form>
  </template>
</template>

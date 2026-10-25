<script setup lang="ts">
import { onBeforeUnmount, ref, watch } from "vue";

import { api } from "@/api/client";
import type { CatalogInstallResult, CatalogTemplate, CatalogValidation } from "@/api/types";
import { revealOnError } from "@/lib/revealError";

const templates = ref<CatalogTemplate[]>([]);
const chosen = ref("");
const text = ref("");
const check = ref<CatalogValidation | null>(null);
const checking = ref(false);
const installing = ref(false);
const installed = ref<CatalogInstallResult | null>(null);
const error = ref("");
revealOnError(error);

async function loadTemplates() {
  try {
    templates.value = await api.catalogTemplates();
  } catch (e) {
    error.value = (e as Error).message;
  }
}

function pick(t: CatalogTemplate) {
  chosen.value = t.id;
  text.value = t.yaml;
  installed.value = null;
}

// Check the text a moment after the person stops typing, using the
// same rules as `ballast catalog validate`.
let timer: ReturnType<typeof setTimeout> | undefined;
let seq = 0;
watch(text, (value) => {
  clearTimeout(timer);
  installed.value = null;
  if (!value.trim()) {
    check.value = null;
    return;
  }
  checking.value = true;
  timer = setTimeout(async () => {
    const mine = ++seq;
    try {
      const res = await api.validateCatalog(value);
      if (mine === seq) check.value = res;
    } catch (e) {
      if (mine === seq) check.value = { valid: false, error: (e as Error).message };
    } finally {
      if (mine === seq) checking.value = false;
    }
  }, 400);
});
onBeforeUnmount(() => clearTimeout(timer));

async function install() {
  error.value = "";
  installing.value = true;
  try {
    installed.value = await api.installCatalog(text.value);
  } catch (e) {
    error.value = (e as Error).message;
  } finally {
    installing.value = false;
  }
}

loadTemplates();
</script>

<template>
  <h2>Custom services</h2>
  <p class="lede">
    Teach Ballast about an app it does not know yet. Start from a template, adjust it, and Ballast checks it as you
    type. Once installed, it shows up in the list of things you can back up.
  </p>

  <p v-if="error" class="notice notice--error" role="alert">{{ error }}</p>

  <div class="panel">
    <h3>1. Pick a starting point</h3>
    <label v-for="t in templates" :key="t.id" class="choice">
      <input type="radio" name="template" :checked="chosen === t.id" @change="pick(t)" />
      <span class="check-text">
        <strong>{{ t.label }}</strong>
        <span class="hint">{{ t.description }}</span>
      </span>
    </label>
  </div>

  <div v-if="chosen" class="panel">
    <h3>2. Adjust it</h3>
    <div class="field">
      <label for="custom-yaml">Service definition</label>
      <textarea
        id="custom-yaml"
        v-model="text"
        rows="24"
        spellcheck="false"
        autocomplete="off"
        style="width: 100%; font-family: monospace"
      ></textarea>
    </div>

    <p v-if="checking" class="hint">Checking…</p>
    <p v-else-if="check && check.valid" class="notice notice--ok" role="status">
      Looks good: <strong>{{ check.service }}</strong> is a valid service.
      <template v-if="check.notice"> {{ check.notice }}</template>
    </p>
    <p v-else-if="check" class="notice notice--error" role="alert" style="white-space: pre-wrap">{{ check.error }}</p>

    <details v-if="chosen === 'script'" class="advanced">
      <summary>About the check script</summary>
      <div class="advanced-body">
        <p>
          After your backup script runs, Ballast runs the check script with <code>BALLAST_ARTIFACT_DIR</code> set to
          the folder holding the backup files. Exit 0 means "this backup can be restored". If you leave the check
          script out, the service stays amber: it was backed up, but Ballast cannot prove it.
        </p>
      </div>
    </details>

    <div class="actions">
      <button class="btn" type="button" :disabled="installing || !check || !check.valid" @click="install">
        {{ installing ? "Installing…" : "Install this service" }}
      </button>
    </div>
    <p v-if="installed" class="notice notice--ok" role="status">
      <strong>{{ installed.service }}</strong> is installed. Add it from
      <router-link to="/wizard">Set up a backup</router-link>.
    </p>
  </div>
</template>

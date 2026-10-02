<script setup lang="ts">
import { ref } from "vue";

import { api, ApiError } from "@/api/client";
import { freshKey } from "@/api/auth";

const emit = defineEmits<{ (e: "signed-in"): void }>();
const input = ref(freshKey.value);
const checking = ref(false);
const error = ref("");

async function connect() {
  const token = input.value.trim();
  if (!token) {
    error.value = "Paste your access key to continue.";
    return;
  }
  checking.value = true;
  error.value = "";
  try {
    await api.login(token);
    input.value = "";
    freshKey.value = "";
    emit("signed-in");
  } catch (e) {
    error.value =
      e instanceof ApiError && e.status === 401
        ? "That access key didn't work. Check you copied all of it and try again."
        : e instanceof ApiError && e.status === 429
          ? "Too many tries. Wait a minute and try again."
          : `Couldn't reach Ballast: ${(e as Error).message}`;
  } finally {
    checking.value = false;
  }
}
</script>

<template>
  <div class="center-screen">
    <form class="panel" style="max-width: 460px; width: 100%" @submit.prevent="connect">
      <h2>Sign in to Ballast</h2>
      <div v-if="freshKey" class="notice notice--ok" role="status">
        <p style="margin-top: 0">Your new access key. Save it now — it won't be shown again.</p>
        <code>{{ freshKey }}</code>
      </div>
      <p class="lede" style="margin-bottom: 1rem">Paste the access key from your Ballast server.</p>
      <div class="field">
        <label for="token">Access key</label>
        <input id="token" v-model="input" type="password" autocomplete="off" />
      </div>
      <p v-if="error" class="notice notice--error" role="alert">{{ error }}</p>
      <button class="btn btn-primary" type="submit" :disabled="checking">
        {{ checking ? "Signing in…" : "Sign in" }}
      </button>

      <details class="advanced">
        <summary>Where do I find the access key?</summary>
        <div class="advanced-body">
          <p style="margin-top: 0">
            Ballast prints it once the first time <code>ballast serve</code> starts, and keeps it in the file
            <code>ballast-api-token</code> in its credential folder on the server. Lost it? Run
            <code>ballast token rotate</code> on the server to make a new one.
          </p>
          <p style="margin-bottom: 0">
            Ballast does not store the key in your browser; it gives you a sign-in cookie instead. "Sign out" ends it.
          </p>
        </div>
      </details>
    </form>
  </div>
</template>

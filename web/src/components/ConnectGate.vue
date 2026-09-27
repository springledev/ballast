<script setup lang="ts">
import { ref } from "vue";

import { api, ApiError } from "@/api/client";
import { setApiToken } from "@/api/auth";

const input = ref("");
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
  setApiToken(token);
  try {
    // Any authenticated endpoint works as a connectivity check; the
    // catalog is always present (bundled entries ship in the binary).
    await api.catalog();
  } catch (e) {
    setApiToken("");
    error.value =
      e instanceof ApiError && e.status === 401
        ? "That access key didn't work. Check you copied all of it and try again."
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
            It's the contents of the file <code>ballast-api-token</code> in Ballast's credential folder on the
            server. Running <code>ballast serve</code> without one prints the exact path it expects. Under systemd,
            provide it with <code>LoadCredential=ballast-api-token:/path/to/file</code>.
          </p>
          <p style="margin-bottom: 0">
            The key is kept in this browser only and sent only to this Ballast server. "Sign out" forgets it.
          </p>
        </div>
      </details>
    </form>
  </div>
</template>

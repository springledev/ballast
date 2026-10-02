<script setup lang="ts">
import { ref } from "vue";

import { api } from "@/api/client";
import { freshKey, markSignedOut } from "@/api/auth";

const busy = ref(false);
const error = ref("");

async function rotate() {
  busy.value = true;
  error.value = "";
  try {
    freshKey.value = (await api.rotateKey()).token;
    // Rotation signs this browser out too; the sign-in screen shows the new key.
    markSignedOut();
  } catch (e) {
    error.value = (e as Error).message;
  } finally {
    busy.value = false;
  }
}

async function signOutEverywhere() {
  busy.value = true;
  error.value = "";
  try {
    await api.signOutEverywhere();
    markSignedOut();
  } catch (e) {
    error.value = (e as Error).message;
    busy.value = false;
  }
}
</script>

<template>
  <section class="panel">
    <h2>Security</h2>
    <p class="lede">Ballast has one access key. Anyone who has it can manage your backups.</p>
    <p v-if="error" class="notice notice--error" role="alert">{{ error }}</p>

    <h3>Sign out everywhere</h3>
    <p>Ends every signed-in browser, including this one. The access key stays the same.</p>
    <button class="btn" type="button" :disabled="busy" @click="signOutEverywhere">Sign out everywhere</button>

    <h3>Make a new access key</h3>
    <p>
      Use this if the key leaked or was shared. The old key stops working immediately and every browser is signed
      out. Scripts and the command line on the server pick up the new key automatically.
    </p>
    <button class="btn btn-primary" type="button" :disabled="busy" @click="rotate">Make a new key</button>
  </section>
</template>

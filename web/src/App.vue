<script setup lang="ts">
import { onMounted, ref } from "vue";

import ConnectGate from "@/components/ConnectGate.vue";
import { api } from "@/api/client";
import { markSignedIn, markSignedOut, session } from "@/api/auth";

const insecure = ref(false);

async function checkSession() {
  try {
    insecure.value = (await api.session()).insecure_exposure;
    markSignedIn();
  } catch {
    markSignedOut();
  }
}

async function signOut() {
  try {
    await api.logout();
  } finally {
    markSignedOut();
  }
}

onMounted(checkSession);
</script>

<template>
  <div v-if="session === 'checking'" class="center-screen" aria-busy="true" />
  <ConnectGate v-else-if="session === 'out'" @signed-in="checkSession" />
  <div v-else class="app-shell">
    <nav class="sidebar" aria-label="Main">
      <div class="brand">
        <svg width="22" height="22" viewBox="0 0 24 24" fill="none" aria-hidden="true">
          <path d="M3 14h18l-2.5 5.5h-13L3 14Z" fill="currentColor" />
          <path d="M12 3v11M12 3l5 7h-5" stroke="currentColor" stroke-width="1.8" stroke-linejoin="round" />
        </svg>
        Ballast
      </div>
      <router-link class="nav-cta" to="/wizard">Set up a backup</router-link>
      <router-link to="/dashboard">Overview</router-link>
      <router-link to="/restore">Restore files</router-link>

      <div class="nav-group">Settings</div>
      <router-link to="/services">Backups</router-link>
      <router-link to="/custom-services">Custom services</router-link>
      <router-link to="/destinations">Destinations</router-link>
      <router-link to="/docker-hosts">Docker hosts</router-link>
      <router-link to="/proxmox">Proxmox</router-link>
      <router-link to="/schedule">Schedules</router-link>
      <router-link to="/settings">Notifications</router-link>
      <router-link to="/verification">Restore checks</router-link>
      <router-link to="/security">Security</router-link>

      <div class="spacer" />
      <button class="sign-out" type="button" @click="signOut">Sign out</button>
    </nav>
    <main class="main">
      <p v-if="insecure" class="notice notice--warn" role="alert">
        Ballast is reachable from your network over plain HTTP, so your access key can be read by anyone on it.
        Put Ballast behind a reverse proxy that provides HTTPS (or use a VPN/tailnet).
      </p>
      <router-view />
    </main>
  </div>
</template>

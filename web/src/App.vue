<script setup lang="ts">
import { computed } from "vue";

import ConnectGate from "@/components/ConnectGate.vue";
import { apiToken, clearApiToken } from "@/api/auth";

const connected = computed(() => apiToken.value !== "");
</script>

<template>
  <ConnectGate v-if="!connected" />
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
      <router-link to="/destinations">Destinations</router-link>
      <router-link to="/docker-hosts">Docker hosts</router-link>
      <router-link to="/schedule">Schedules</router-link>
      <router-link to="/settings">Notifications</router-link>
      <router-link to="/verification">Restore checks</router-link>

      <div class="spacer" />
      <button class="sign-out" type="button" @click="clearApiToken">Sign out</button>
    </nav>
    <main class="main">
      <router-view />
    </main>
  </div>
</template>

<script setup lang="ts">
import { onMounted, ref, watch } from "vue";
import { useRoute } from "vue-router";

import AppIcon from "@/components/AppIcon.vue";
import ConfirmDialog from "@/components/ConfirmDialog.vue";
import ConnectGate from "@/components/ConnectGate.vue";
import { api } from "@/api/client";
import { markSignedIn, markSignedOut, session } from "@/api/auth";
import { nextTheme, theme, themeLabel } from "@/lib/prefs";

const insecure = ref(false);
const buildVersion = ref("");
const moreOpen = ref(false);
const route = useRoute();

// The four screens people use most sit in the phone's bottom bar; the
// rest of the setup pages live under "More" (on a phone) or in the
// sidebar (on a wider screen).
const primary = [
  { to: "/dashboard", label: "Overview", icon: "overview" },
  { to: "/services", label: "Backups", icon: "backups" },
  { to: "/restore", label: "Restore", icon: "restore" },
  { to: "/schedule", label: "Schedules", icon: "schedule" },
];
const setup = [
  { to: "/destinations", label: "Destinations" },
  { to: "/docker-hosts", label: "Docker hosts" },
  { to: "/proxmox", label: "Proxmox" },
  { to: "/custom-services", label: "Custom services" },
  { to: "/settings", label: "Notifications" },
  { to: "/verification", label: "Restore checks" },
  { to: "/security", label: "Security" },
];

const moreActive = () => setup.some((s) => route.path.startsWith(s.to)) || route.path === "/wizard";

watch(
  () => route.fullPath,
  () => (moreOpen.value = false),
);

function onKey(e: KeyboardEvent) {
  if (e.key === "Escape") moreOpen.value = false;
}

async function checkSession() {
  try {
    insecure.value = (await api.session()).insecure_exposure;
    markSignedIn();
    loadVersion();
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

// The footer shows the build (spec §21.5); a failed read just hides it.
async function loadVersion() {
  try {
    buildVersion.value = (await api.version()).version;
  } catch {
    buildVersion.value = "";
  }
}

onMounted(checkSession);
</script>

<template>
  <div v-if="session === 'checking'" class="center-screen" aria-busy="true" />
  <ConnectGate v-else-if="session === 'out'" @signed-in="checkSession" />
  <div v-else class="app-shell" @keydown="onKey">
    <header class="topbar">
      <router-link to="/dashboard" class="brand" aria-label="Ballast overview">
        <svg width="24" height="24" viewBox="0 0 24 24" fill="none" aria-hidden="true">
          <path d="M3 14h18l-2.5 5.5h-13L3 14Z" fill="currentColor" />
          <path d="M12 3v11M12 3l5 7h-5" stroke="currentColor" stroke-width="1.8" stroke-linejoin="round" />
        </svg>
        Ballast
      </router-link>
      <router-link class="btn btn-primary btn-small" to="/wizard">
        <AppIcon name="plus" :size="18" /> Set up a backup
      </router-link>
    </header>

    <nav class="sidebar" aria-label="Main">
      <router-link to="/dashboard" class="brand">
        <svg width="24" height="24" viewBox="0 0 24 24" fill="none" aria-hidden="true">
          <path d="M3 14h18l-2.5 5.5h-13L3 14Z" fill="currentColor" />
          <path d="M12 3v11M12 3l5 7h-5" stroke="currentColor" stroke-width="1.8" stroke-linejoin="round" />
        </svg>
        Ballast
      </router-link>
      <router-link class="nav-cta" to="/wizard"><AppIcon name="plus" :size="18" /> Set up a backup</router-link>
      <router-link v-for="p in primary" :key="p.to" :to="p.to" class="nav-link">
        <AppIcon :name="p.icon" :size="20" /> {{ p.label }}
      </router-link>

      <div class="nav-group">Setup</div>
      <router-link v-for="s in setup" :key="s.to" :to="s.to" class="nav-link nav-link--plain">{{ s.label }}</router-link>

      <div class="spacer" />
      <button class="nav-quiet" type="button" :aria-label="`Colour theme: ${themeLabel[theme]}. Change`" @click="theme = nextTheme(theme)">
        <AppIcon name="theme" :size="18" /> Theme: {{ themeLabel[theme] }}
      </button>
      <button class="nav-quiet" type="button" @click="signOut"><AppIcon name="signout" :size="18" /> Sign out</button>
    </nav>

    <ConfirmDialog />

    <main class="main">
      <div class="page">
        <p v-if="insecure" class="notice notice--warn" role="alert">
          Ballast is reachable from your network over plain HTTP, so your access key can be read by anyone on it.
          Put Ballast behind a reverse proxy that provides HTTPS (or use a VPN/tailnet).
        </p>
        <router-view />
        <footer v-if="buildVersion" class="app-foot">Ballast {{ buildVersion }}</footer>
      </div>
    </main>

    <nav class="tabbar" aria-label="Main">
      <router-link v-for="p in primary" :key="p.to" :to="p.to" class="tab">
        <AppIcon :name="p.icon" />
        <span>{{ p.label }}</span>
      </router-link>
      <button
        class="tab"
        :class="{ 'router-link-active': moreActive() || moreOpen }"
        type="button"
        aria-haspopup="dialog"
        :aria-expanded="moreOpen"
        @click="moreOpen = !moreOpen"
      >
        <AppIcon name="more" />
        <span>More</span>
      </button>
    </nav>

    <div v-if="moreOpen" class="sheet-backdrop" @click="moreOpen = false" />
    <div v-if="moreOpen" class="sheet" role="dialog" aria-modal="true" aria-label="More pages">
      <div class="sheet-head">
        <strong>More</strong>
        <button class="btn btn-quiet btn-icon" type="button" aria-label="Close" @click="moreOpen = false">
          <AppIcon name="close" />
        </button>
      </div>
      <div class="sheet-grid">
        <router-link v-for="s in setup" :key="s.to" :to="s.to" class="sheet-link">{{ s.label }}</router-link>
      </div>
      <div class="sheet-foot">
        <button class="btn" type="button" @click="theme = nextTheme(theme)">
          <AppIcon name="theme" :size="18" /> Theme: {{ themeLabel[theme] }}
        </button>
        <button class="btn btn-quiet" type="button" @click="signOut"><AppIcon name="signout" :size="18" /> Sign out</button>
      </div>
    </div>
  </div>
</template>

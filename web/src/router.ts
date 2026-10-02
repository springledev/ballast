import { createRouter, createWebHashHistory } from "vue-router";

// Hash history (spec §10's GUI is served as static files under
// whatever path `ballast serve` is reverse-proxied at, with no
// server-side routing) so a deep link or page refresh never depends
// on the server rewriting unknown paths back to index.html.
const router = createRouter({
  history: createWebHashHistory(),
  routes: [
    { path: "/", redirect: "/dashboard" },
    { path: "/dashboard", name: "dashboard", component: () => import("./views/DashboardView.vue") },
    { path: "/services", name: "services", component: () => import("./views/ServicesView.vue") },
    { path: "/destinations", name: "destinations", component: () => import("./views/DestinationsView.vue") },
    { path: "/docker-hosts", name: "docker-hosts", component: () => import("./views/DockerHostsView.vue") },
    { path: "/proxmox", name: "proxmox", component: () => import("./views/ProxmoxView.vue") },
    { path: "/schedule", name: "schedule", component: () => import("./views/ScheduleView.vue") },
    { path: "/restore", name: "restore", component: () => import("./views/RestoreView.vue") },
    { path: "/verification", name: "verification", component: () => import("./views/VerificationView.vue") },
    { path: "/settings", name: "settings", component: () => import("./views/SettingsView.vue") },
    { path: "/security", name: "security", component: () => import("./views/SecurityView.vue") },
    { path: "/wizard", name: "wizard", component: () => import("./views/WizardView.vue") },
  ],
});

export default router;

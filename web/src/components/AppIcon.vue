<script setup lang="ts">
// Small line icons for navigation and buttons. Drawn inline so the UI
// works on machines with no internet access.
defineProps<{ name: string; size?: number }>();
</script>

<script lang="ts">
const paths = {
  overview: "M3 13h4l2-5 4 10 2-5h6",
  backups: "M4 6.5C4 5 8 4 12 4s8 1 8 2.5S16 9 12 9 4 8 4 6.5ZM4 6.5v5C4 13 8 14 12 14s8-1 8-2.5v-5M4 11.5v5C4 18 8 19 12 19s8-1 8-2.5v-5",
  restore: "M4 12a8 8 0 1 0 2.4-5.7M4 4v4h4M12 8v4l3 2",
  schedule: "M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18ZM12 7v5l3 3",
  more: "M5 12h.01M12 12h.01M19 12h.01",
  plus: "M12 5v14M5 12h14",
  search: "M11 18a7 7 0 1 0 0-14 7 7 0 0 0 0 14ZM20 20l-4-4",
  close: "M6 6l12 12M18 6 6 18",
  theme: "M12 21a9 9 0 0 1 0-18v18ZM12 3a9 9 0 0 1 0 18",
  signout: "M15 4h4v16h-4M10 8l-4 4 4 4M6 12h10",
} as const;
</script>

<template>
  <svg
    class="icon"
    :width="size ?? 22"
    :height="size ?? 22"
    viewBox="0 0 24 24"
    fill="none"
    stroke="currentColor"
    :stroke-width="name === 'more' ? 3 : 1.8"
    stroke-linecap="round"
    stroke-linejoin="round"
    aria-hidden="true"
  >
    <path :d="paths[name as keyof typeof paths]" />
  </svg>
</template>

<script setup lang="ts">
// The Ballast logo: a sailboat held upright by the weight in its keel.
// It is drawn inline rather than loaded from favicon.svg so it never
// flashes in late. Keep the shapes in step with web/public/favicon.svg.
withDefaults(defineProps<{ size?: number }>(), { size: 28 });
</script>

<template>
  <svg class="brand-mark" :width="size" :height="size" viewBox="0 0 64 64" aria-hidden="true">
    <rect width="64" height="64" rx="14" fill="#143448" />
    <path d="M0 37h64v13a14 14 0 0 1-14 14H14A14 14 0 0 1 0 50Z" fill="#1f5a80" />
    <path d="M31 7v22H14Z" fill="#fff" />
    <path d="M35 12v17h12Z" fill="#fff" opacity=".75" />
    <path d="M7 32h50c-1 6-5 10-11 10H19C13 42 8 38 7 32Z" fill="#fff" />
    <path d="M29 41h6v9h-6Z" fill="#fff" />
    <rect x="21" y="47" width="22" height="9" rx="4.5" fill="#4cc38a" />
  </svg>
</template>

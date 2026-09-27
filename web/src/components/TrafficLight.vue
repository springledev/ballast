<script setup lang="ts">
import { lightLabel, type Light } from "@/lib/status";

defineProps<{ light: Light }>();
</script>

<template>
  <span class="lamp" :class="`lamp--${light}`">{{ lightLabel[light] }}</span>
</template>

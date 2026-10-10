<script setup lang="ts">
// Search, status filter, grouping and density for long lists. Views
// only show it once a list is long enough to need it, so a small
// install never sees it.
import { computed } from "vue";

import AppIcon from "@/components/AppIcon.vue";
import type { GroupBy, LightFilter, ListQuery } from "@/lib/listView";

const props = defineProps<{
  modelValue: ListQuery;
  /** Counts per status chip; leave out to hide the status chips. */
  counts?: Record<LightFilter, number>;
  /** Grouping choices to offer. */
  groups: GroupBy[];
  /** "cards" or "compact"; leave out to hide the density switch. */
  density?: string;
  shown: number;
  total: number;
  label: string;
}>();
const emit = defineEmits<{
  (e: "update:modelValue", v: ListQuery): void;
  (e: "update:density", v: string): void;
}>();

const groupLabels: Record<GroupBy, string> = {
  none: "No grouping",
  status: "Group by status",
  type: "Group by kind",
  destination: "Group by destination",
};

const chips = computed(() =>
  props.counts
    ? ([
        { id: "all", label: "All" },
        { id: "attention", label: "Needs attention" },
        { id: "running", label: "Backing up" },
        { id: "green", label: "Safe" },
      ] as { id: LightFilter; label: string }[]).filter((c) => c.id === "all" || c.id === "attention" || props.counts![c.id] > 0)
    : [],
);

function set(patch: Partial<ListQuery>) {
  emit("update:modelValue", { ...props.modelValue, ...patch });
}
</script>

<template>
  <div class="list-tools" role="search" :aria-label="`Find ${label}`">
    <label class="search">
      <AppIcon name="search" :size="18" />
      <span class="visually-hidden">Search {{ label }}</span>
      <input
        type="search"
        :value="modelValue.text"
        :placeholder="`Search ${total} ${label}…`"
        autocomplete="off"
        spellcheck="false"
        @input="set({ text: ($event.target as HTMLInputElement).value })"
      />
    </label>

    <div v-if="chips.length" class="chips" role="group" aria-label="Show">
      <button
        v-for="c in chips"
        :key="c.id"
        type="button"
        class="chip"
        :class="{ 'chip--attention': c.id === 'attention' && counts![c.id] > 0 }"
        :aria-pressed="modelValue.light === c.id"
        @click="set({ light: c.id })"
      >
        {{ c.label }} <span class="chip-count">{{ counts![c.id] }}</span>
      </button>
    </div>

    <div class="tools-row">
      <label class="tool-select">
        <span class="visually-hidden">Group</span>
        <select :value="modelValue.group" @change="set({ group: ($event.target as HTMLSelectElement).value as GroupBy })">
          <option v-for="g in groups" :key="g" :value="g">{{ groupLabels[g] }}</option>
        </select>
      </label>
      <div v-if="density" class="segmented" role="group" aria-label="Layout">
        <button type="button" :aria-pressed="density === 'cards'" @click="emit('update:density', 'cards')">Cards</button>
        <button type="button" :aria-pressed="density === 'compact'" @click="emit('update:density', 'compact')">Compact</button>
      </div>
      <span class="tools-count muted" aria-live="polite">
        <template v-if="shown === total">{{ total }} {{ label }}</template>
        <template v-else>{{ shown }} of {{ total }} shown</template>
      </span>
    </div>
  </div>
</template>

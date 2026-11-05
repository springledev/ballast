<script setup lang="ts">
import { nextTick, ref, watch } from "vue";

import { answerConfirm, pending } from "@/lib/confirm";

const dialog = ref<HTMLDialogElement | null>(null);
const cancelButton = ref<HTMLButtonElement | null>(null);

// <dialog> gives a focus trap, Esc to close and a backdrop for free.
watch(pending, async (request) => {
  const el = dialog.value;
  if (!el) return;
  if (request && !el.open) {
    el.showModal();
    await nextTick();
    // Land on the safe choice so a stray Enter never confirms.
    cancelButton.value?.focus();
  } else if (!request && el.open) {
    el.close();
  }
});

// Esc and clicking the backdrop both mean "no".
function onCancel(e: Event) {
  e.preventDefault();
  answerConfirm(false);
}

function onBackdrop(e: MouseEvent) {
  if (e.target === dialog.value) answerConfirm(false);
}
</script>

<template>
  <dialog ref="dialog" class="confirm" aria-labelledby="confirm-message" @cancel="onCancel" @click="onBackdrop">
    <form v-if="pending" method="dialog" @submit.prevent="answerConfirm(true)">
      <p id="confirm-message" class="confirm-message">{{ pending.message }}</p>
      <div class="actions">
        <button ref="cancelButton" class="btn btn-quiet" type="button" @click="answerConfirm(false)">{{ pending.cancelLabel }}</button>
        <button class="btn btn-danger" type="submit">{{ pending.confirmLabel }}</button>
      </div>
    </form>
  </dialog>
</template>

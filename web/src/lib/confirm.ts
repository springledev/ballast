// A styled stand-in for window.confirm(): the browser's own popup can't be
// themed, reads differently in every browser, and freezes the whole page
// (and any automated test) until it is answered.
import { ref } from "vue";

/** A question waiting for an answer; the dialog in App.vue shows it. */
export interface ConfirmRequest {
  message: string;
  confirmLabel: string;
  /** The safe choice's label, focused first; "Cancel" unless the question needs another word. */
  cancelLabel: string;
  resolve: (ok: boolean) => void;
  /** The element that had focus when the question was asked; focus returns to it afterwards. */
  opener: HTMLElement | null;
}

/** The question currently on screen, if any. */
export const pending = ref<ConfirmRequest | null>(null);

/**
 * Asks a yes/no question and resolves true only if the person chooses the
 * confirm button. Asking again while a question is open answers the first
 * one "no".
 */
export function askConfirm(message: string, confirmLabel = "Yes, continue", cancelLabel = "Cancel"): Promise<boolean> {
  pending.value?.resolve(false);
  const active = typeof document !== "undefined" ? document.activeElement : null;
  const opener = active instanceof HTMLElement && active !== document.body ? active : null;
  return new Promise((resolve) => {
    pending.value = { message, confirmLabel, cancelLabel, resolve, opener };
  });
}

/** Closes the open question with the person's answer. */
export function answerConfirm(ok: boolean): void {
  const request = pending.value;
  pending.value = null;
  request?.resolve(ok);
}

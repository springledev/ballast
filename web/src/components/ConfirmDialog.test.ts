// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { createApp, nextTick } from "vue";

import { answerConfirm, askConfirm, pending } from "@/lib/confirm";
import ConfirmDialog from "./ConfirmDialog.vue";

// jsdom has no <dialog> modal support, so give it the small part we use.
const proto = HTMLDialogElement.prototype as unknown as Record<string, unknown>;
proto.showModal = function (this: HTMLDialogElement) {
  this.setAttribute("open", "");
};
proto.close = function (this: HTMLDialogElement) {
  this.removeAttribute("open");
};

async function flush() {
  for (let i = 0; i < 4; i++) await new Promise((r) => setTimeout(r, 0));
  await nextTick();
}

describe("ConfirmDialog focus", () => {
  afterEach(() => {
    pending.value = null;
    document.body.innerHTML = "";
  });

  const cases = [
    { name: "Esc (cancel event)", close: (d: HTMLDialogElement) => d.dispatchEvent(new Event("cancel", { cancelable: true })) },
    { name: "the Keep running button", close: (d: HTMLDialogElement) => d.querySelector<HTMLButtonElement>(".btn-quiet")!.click() },
    { name: "answering from code", close: () => answerConfirm(false) },
  ];

  for (const c of cases) {
    it(`returns focus to the opening button after ${c.name}`, async () => {
      const host = document.createElement("div");
      document.body.appendChild(host);
      const opener = document.createElement("button");
      opener.textContent = "Cancel backup";
      document.body.appendChild(opener);
      createApp(ConfirmDialog).mount(host);

      opener.focus();
      expect(document.activeElement).toBe(opener);
      const answer = askConfirm("Stop this backup?", "Stop backup", "Keep running");
      await flush();
      const dialog = host.querySelector("dialog")!;
      expect(dialog.hasAttribute("open")).toBe(true);
      expect(document.activeElement).not.toBe(opener);

      c.close(dialog);
      await expect(answer).resolves.toBe(false);
      await flush();
      expect(dialog.hasAttribute("open")).toBe(false);
      expect(document.activeElement).toBe(opener);
    });
  }
});

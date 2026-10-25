// Long forms put their one error notice at the top of the page, so a
// mistake made at the bottom looked like a button that did nothing.
import { nextTick, watch, type Ref } from "vue";

/**
 * Scrolls the error notice into view each time `error` is given a message.
 * Runs synchronously so a view that clears the message and sets the same
 * one again still gets scrolled.
 */
export function revealOnError(error: Ref<string>): void {
  watch(
    error,
    async (message) => {
      if (!message) return;
      await nextTick();
      const notice = [...document.querySelectorAll<HTMLElement>('.notice--error[role="alert"]')].find((el) =>
        el.textContent?.includes(message),
      );
      if (!notice || typeof notice.scrollIntoView !== "function") return;
      const reduce = window.matchMedia?.("(prefers-reduced-motion: reduce)").matches;
      notice.scrollIntoView({ block: "center", behavior: reduce ? "auto" : "smooth" });
    },
    { flush: "sync" },
  );
}

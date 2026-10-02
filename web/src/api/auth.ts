import { ref } from "vue";

// The web UI signs in with the access key once; the server answers with
// an HttpOnly session cookie, so the key itself is never kept in the
// browser (not in localStorage, not in memory after sign-in). This
// module only tracks whether the browser currently has a live session.
export type SessionState = "checking" | "in" | "out";

export const session = ref<SessionState>("checking");

// freshKey holds a just-rotated access key (in memory only) so the
// sign-in screen can show it once after rotation signs everyone out.
export const freshKey = ref("");

export function markSignedIn(): void {
  session.value = "in";
}

export function markSignedOut(): void {
  session.value = "out";
}

// Older versions kept the key in localStorage; remove any leftover copy.
try {
  window.localStorage.removeItem("ballast_api_token");
} catch {
  // Storage blocked: nothing to clean up.
}

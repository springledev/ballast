import { ref } from "vue";

// Ballast's API requires `Authorization: Bearer <token>` on every
// request except the trigger endpoint (spec rule 2; internal/api's
// withAuth). The web UI has no login/session system of its own — it
// is a thin layer over the same token an operator would put in a
// CLI script or a ballast.yaml-adjacent secret — so the token is
// entered once and kept in this browser's localStorage only. It is
// never sent anywhere but this Ballast server's own /api/v1 routes.
const STORAGE_KEY = "ballast_api_token";

function readStoredToken(): string {
  try {
    return window.localStorage.getItem(STORAGE_KEY) ?? "";
  } catch {
    // Private browsing / blocked storage: fall back to an in-memory
    // token for this page load rather than crashing the app.
    return "";
  }
}

export const apiToken = ref<string>(readStoredToken());

export function setApiToken(token: string): void {
  apiToken.value = token;
  try {
    if (token) {
      window.localStorage.setItem(STORAGE_KEY, token);
    } else {
      window.localStorage.removeItem(STORAGE_KEY);
    }
  } catch {
    // Ignore: the in-memory ref still works for this page load.
  }
}

export function clearApiToken(): void {
  setApiToken("");
}

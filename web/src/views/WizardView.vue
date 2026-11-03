<script setup lang="ts">
import { computed, onMounted, onUnmounted, reactive, ref, watch } from "vue";

import { api } from "@/api/client";
import type { CatalogSummary, EnvSourceProposal, Destination, DetectResult, DockerComposeProject, DockerHost, DockerVolume, PreflightResult, ServiceStatus } from "@/api/types";
import { schedulePresets } from "@/lib/schedule";
import { describeCheck, describeTier } from "@/lib/status";
import { checkParam, warningsNote } from "@/lib/params";
import { revealOnError } from "@/lib/revealError";

type Step = "what" | "where" | "when" | "review" | "running" | "done";
const stepOrder: { id: Step; label: string }[] = [
  { id: "what", label: "What" },
  { id: "where", label: "Where" },
  { id: "when", label: "When" },
  { id: "review", label: "Check and start" },
];

const step = ref<Step>("what");
const error = ref("");
/** Plain-English warning from saving the service, e.g. a folder that does not exist yet. */
const saveWarning = ref("");
revealOnError(error);
const loading = ref(true);

const preflight = ref<PreflightResult | null>(null);
const catalog = ref<CatalogSummary[]>([]);
const detected = ref<DetectResult>({ containers: [], suggestions: [] });
const destinations = ref<Destination[]>([]);
const dockerHosts = ref<DockerHost[]>([]);
const chosenHost = ref("local");

const chosenEntry = ref("");
const serviceName = ref("");
const params = reactive<Record<string, string>>({});
// One-path setup from the app's .env (spec §18.2).
const envDir = ref("");
const envProposal = ref<EnvSourceProposal | null>(null);
const envError = ref("");
const envSecrets = ref<Record<string, string>>({});

async function readEnv() {
  envError.value = "";
  envProposal.value = null;
  try {
    envProposal.value = await api.readEnvSource(chosenEntry.value, envDir.value);
  } catch (e) {
    envError.value = e instanceof Error ? e.message : String(e);
  }
}

function useEnvSettings() {
  const prop = envProposal.value;
  if (!prop || prop.missing?.length) return;
  Object.assign(params, prop.params);
  envSecrets.value = { ...prop.secrets };
  envProposal.value = null;
}

const destMode = ref<"existing" | "new">("new");
const destExisting = ref("");
const destName = ref("local");
const destLocation = ref("");
const createdPasswordFile = ref("");
// Set once the service is saved, so a retry after a later failure
// updates it instead of trying to create it twice.
const serviceSaved = ref("");

const scheduleChoice = ref("nightly");

const starting = ref(false);
const result = ref<ServiceStatus | null>(null);
const timedOut = ref(false);
let cancelled = false;
onUnmounted(() => (cancelled = true));

// Most people only ever have "local"; don't show a picker for a
// choice that isn't really a choice yet.
const showHostPicker = computed(() => dockerHosts.value.length > 1);
const selectedEntry = computed(() => catalog.value.find((c) => c.service === chosenEntry.value));
const suggestion = computed(() => detected.value.suggestions[0]);
const resticBlocked = computed(() => !!preflight.value?.restic_error);
const stepIndex = computed(() => stepOrder.findIndex((s) => s.id === step.value));
const destLabel = computed(() => (destMode.value === "existing" ? destExisting.value : destName.value));
const scheduleLabel = computed(
  () => schedulePresets.find((p) => p.id === scheduleChoice.value)?.label ?? "Only when I start it",
);

onMounted(async () => {
  try {
    [preflight.value, catalog.value, destinations.value, dockerHosts.value] = await Promise.all([
      api.preflight(),
      api.catalog(),
      api.listDestinations(),
      api.listDockerHosts(),
    ]);
    try {
      detected.value = await api.detect(chosenHost.value);
    } catch {
      // Detection is best-effort (Docker may not be reachable); the
      // wizard still works with a manual pick.
    }
    chooseEntry(suggestion.value?.service || "folder");
    if (destinations.value.length > 0) {
      destMode.value = "existing";
      destExisting.value = destinations.value[0].name;
    }
  } catch (e) {
    error.value = (e as Error).message;
  } finally {
    loading.value = false;
  }
});

async function onHostChange() {
  try {
    detected.value = await api.detect(chosenHost.value);
  } catch {
    detected.value = { containers: [], suggestions: [] };
  }
  chooseEntry(suggestion.value?.service || "folder");
}

function chooseEntry(id: string) {
  chosenEntry.value = id;
  serviceName.value = id;
  for (const k of Object.keys(params)) delete params[k];
  envDir.value = "";
  envProposal.value = null;
  envError.value = "";
  envSecrets.value = {};
  for (const p of selectedEntry.value?.parameters ?? []) params[p.name] = p.default ?? "";
}

// --- Docker volume picker (parameters with picker: "docker-volume") ---

const volumeOptions = ref<DockerVolume[]>([]);
const volumeError = ref("");
const hasVolumePicker = computed(() => (selectedEntry.value?.parameters ?? []).some((p) => p.picker === "docker-volume"));

/** Loads the volumes on chosenHost, for the datalist of any
 * "docker-volume" parameter. Best-effort: on failure the picker just
 * falls back to a plain text box. */
async function loadVolumes() {
  if (!hasVolumePicker.value || !chosenHost.value) {
    volumeOptions.value = [];
    volumeError.value = "";
    return;
  }
  volumeError.value = "";
  const host = chosenHost.value;
  try {
    const vols = await api.listDockerHostVolumes(host);
    if (chosenHost.value !== host) return; // the host changed meanwhile
    volumeOptions.value = vols;
  } catch {
    if (chosenHost.value !== host) return;
    volumeOptions.value = [];
    volumeError.value = "Couldn't list volumes on this host — type the volume name.";
  }
}

function volumeLabel(v: DockerVolume): string {
  return v.compose_project ? `${v.name} (${v.compose_project})` : v.name;
}

// --- Compose project picker (parameters with picker: "compose-project") ---

const composeProjectOptions = ref<DockerComposeProject[]>([]);
const composeProjectError = ref("");
const hasComposeProjectPicker = computed(() =>
  (selectedEntry.value?.parameters ?? []).some((p) => p.picker === "compose-project"),
);

/** Loads the Compose projects on chosenHost, for the datalist of any
 * "compose-project" parameter. Best-effort: on failure the picker just
 * falls back to a plain text box. */
async function loadComposeProjects() {
  if (!hasComposeProjectPicker.value || !chosenHost.value) {
    composeProjectOptions.value = [];
    composeProjectError.value = "";
    return;
  }
  composeProjectError.value = "";
  const host = chosenHost.value;
  try {
    const projects = await api.listDockerHostComposeProjects(host);
    if (chosenHost.value !== host) return; // the host changed meanwhile
    composeProjectOptions.value = projects;
  } catch {
    if (chosenHost.value !== host) return;
    composeProjectOptions.value = [];
    composeProjectError.value = "Couldn't list Compose projects on this host — type the project name.";
  }
}

watch(() => [chosenEntry.value, chosenHost.value], () => {
  loadVolumes();
  loadComposeProjects();
});

function next() {
  error.value = "";
  if (step.value === "what") {
    if (!/^[a-z0-9][a-z0-9_-]{0,62}$/.test(serviceName.value)) {
      error.value = "Use lowercase letters, numbers and dashes for the name, like photos or my-app.";
      return;
    }
    for (const p of selectedEntry.value?.parameters ?? []) {
      const problem = checkParam(p, params[p.name]);
      if (problem) {
        error.value = problem;
        return;
      }
    }
    step.value = "where";
  } else if (step.value === "where") {
    if (destMode.value === "new" && (!destName.value || !destLocation.value.startsWith("/"))) {
      error.value = "Give the place a name and a full folder path starting with /.";
      return;
    }
    step.value = "when";
  } else if (step.value === "when") {
    step.value = "review";
  }
}

function back() {
  error.value = "";
  const i = stepIndex.value;
  if (i > 0) step.value = stepOrder[i - 1].id;
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

async function start() {
  starting.value = true;
  error.value = "";
  try {
    let dest = destExisting.value;
    if (destMode.value === "new") {
      const cred = `ballast-restic-password.${destName.value}`;
      await api.createDestination({
        name: destName.value,
        type: "local",
        settings: { path: destLocation.value },
        keep_daily: 7,
        keep_weekly: 4,
        keep_monthly: 6,
        init: true,
      });
      createdPasswordFile.value = cred;
      destMode.value = "existing";
      destExisting.value = destName.value;
      dest = destName.value;
    }
    const svc = {
      name: serviceName.value,
      catalog_entry: chosenEntry.value,
      destination: dest,
      docker_host: chosenHost.value,
      params: { ...params },
      secrets: Object.keys(envSecrets.value).length ? { ...envSecrets.value } : undefined,
    };
    if (serviceSaved.value) {
      saveWarning.value = warningsNote((await api.updateService(serviceSaved.value, svc)).warnings);
    } else {
      saveWarning.value = warningsNote((await api.createService(svc)).warnings);
      serviceSaved.value = serviceName.value;
    }
    const preset = schedulePresets.find((p) => p.id === scheduleChoice.value);
    if (preset) await api.setSchedule(serviceName.value, preset.cron);

    await api.startBackup(serviceName.value);
    step.value = "running";
    await waitForResult();
    step.value = "done";
  } catch (e) {
    error.value = (e as Error).message;
  } finally {
    starting.value = false;
  }
}

// Wait until the first run has actually finished: it must exist
// (last_run_id set) and no longer be running. A large backup can take
// a long time, so after 30 minutes stop waiting and say it's still
// going, rather than guess at a result.
async function waitForResult() {
  const deadline = Date.now() + 30 * 60 * 1000;
  while (!cancelled && Date.now() < deadline) {
    await sleep(2000);
    const [st] = await api.status(serviceName.value, true);
    const t = st?.tiers[0];
    if (t?.last_run_id && st.tiers.every((x) => x.light !== "running")) {
      result.value = st;
      return;
    }
  }
  timedOut.value = true;
}

const resultLight = computed(() => {
  const tiers = result.value?.tiers ?? [];
  if (tiers.some((t) => t.light === "red")) return "red";
  if (tiers.some((t) => t.light === "amber")) return "amber";
  return tiers.length && tiers.every((t) => t.light === "green") ? "green" : "none";
});
</script>

<template>
  <h2>Set up a backup</h2>
  <p class="lede">A few questions, then Ballast runs your first backup and restores part of it to prove it works.</p>

  <p v-if="loading" class="loading">Loading…</p>

  <template v-else>
    <div v-if="resticBlocked" class="notice notice--error" role="alert">
      <p><strong>Ballast can't make backups on this server yet.</strong></p>
      <p>{{ preflight?.restic_error }}</p>
      <p>
        Ballast stores backups with a free tool called restic.
        <a href="https://restic.readthedocs.io/en/stable/020_installation.html" target="_blank" rel="noopener">Install restic</a>
        on the server, then reload this page.
      </p>
    </div>
    <p v-else-if="preflight?.restic_warning" class="notice notice--warn">{{ preflight.restic_warning }}</p>

    <ol v-if="stepIndex >= 0" class="steps" aria-label="Progress">
      <li
        v-for="(s, i) in stepOrder"
        :key="s.id"
        :class="{ done: i < stepIndex }"
        :aria-current="s.id === step ? 'step' : undefined"
      >
        {{ i + 1 }}. {{ s.label }}
      </li>
    </ol>

    <p v-if="error" class="notice notice--error" role="alert">{{ error }}</p>
    <p v-if="saveWarning" class="notice notice--warn" role="status">{{ saveWarning }}</p>

    <!-- 1. What -->
    <form v-if="step === 'what'" class="panel" @submit.prevent="next">
      <h3>What do you want to back up?</h3>
      <div v-if="showHostPicker" class="field">
        <label for="wiz-host">Docker host</label>
        <select id="wiz-host" v-model="chosenHost" @change="onHostChange">
          <option v-for="h in dockerHosts" :key="h.name" :value="h.name">{{ h.name }}</option>
        </select>
        <p class="hint">
          Which machine to look for containers on. Add more machines on
          <router-link to="/docker-hosts">Docker hosts</router-link>.
        </p>
      </div>
      <p v-if="suggestion" class="notice">
        Found <strong>{{ suggestion.containers.map((c) => c.name).join(", ") }}</strong> running on this server. It's
        selected below.
      </p>
      <label v-for="c in catalog" :key="c.service" class="choice">
        <input type="radio" name="entry" :value="c.service" :checked="chosenEntry === c.service" @change="chooseEntry(c.service)" />
        <span class="check-text">
          <strong>{{ c.name }}<span v-if="c.service === suggestion?.service" class="muted"> (found on this server)</span></strong>
          <span class="hint">{{ c.description }}</span>
        </span>
      </label>
      <p v-if="preflight && !preflight.docker_installed" class="hint" style="margin: 0.5rem 0 1rem">
        Docker isn't available on this server, so only plain folders can be backed up for now.
      </p>

      <div v-if="selectedEntry?.env_source" class="field" style="margin-top: 1rem">
        <label for="wiz-env-dir">{{ selectedEntry.env_source.prompt || "Path to the folder holding the app's .env file" }}</label>
        <input id="wiz-env-dir" v-model="envDir" placeholder="/opt/immich" autocomplete="off" />
        <p class="hint">Optional — Ballast reads the settings it needs from that folder's .env file, so you can skip the boxes below.</p>
        <button type="button" class="secondary" :disabled="!envDir" @click="readEnv">Look for settings</button>
        <p v-if="envError" class="hint" role="alert">{{ envError }}</p>
        <div v-if="envProposal" style="margin-top: 0.5rem">
          <p class="hint">Found {{ envProposal.env_file }}:</p>
          <ul>
            <li v-for="k in envProposal.keys" :key="k.key">
              <code>{{ k.key }}</code>:
              <span v-if="!k.found">not found</span>
              <span v-else-if="k.secret">found (stays in that file, never copied)</span>
              <span v-else>{{ k.value }}</span>
            </li>
          </ul>
          <p v-if="envProposal.missing?.length" class="hint" role="alert">
            Missing from that file: {{ envProposal.missing.join(", ") }}. Fill in the boxes below by hand instead.
          </p>
          <button v-else type="button" @click="useEnvSettings">Use these settings</button>
        </div>
        <p v-else-if="Object.keys(envSecrets).length" class="hint">Using the settings from your .env file. You can still change any box below.</p>
      </div>

      <div v-for="p in selectedEntry?.parameters ?? []" :key="p.name" class="field" style="margin-top: 1rem">
        <label :for="`wiz-param-${p.name}`">{{ p.prompt || p.name }}<span v-if="!p.required" class="muted"> (optional)</span></label>
        <template v-if="p.picker === 'docker-volume'">
          <input :id="`wiz-param-${p.name}`" v-model="params[p.name]" list="wiz-docker-volume-options" autocomplete="off" />
          <p v-if="volumeError" class="hint">{{ volumeError }}</p>
        </template>
        <template v-else-if="p.picker === 'compose-project'">
          <input :id="`wiz-param-${p.name}`" v-model="params[p.name]" list="wiz-compose-project-options" autocomplete="off" />
          <p v-if="composeProjectError" class="hint">{{ composeProjectError }}</p>
        </template>
        <input v-else :id="`wiz-param-${p.name}`" v-model="params[p.name]" />
      </div>
      <datalist v-if="hasVolumePicker" id="wiz-docker-volume-options">
        <option v-for="v in volumeOptions" :key="v.name" :value="v.name">{{ volumeLabel(v) }}</option>
      </datalist>
      <datalist v-if="hasComposeProjectPicker" id="wiz-compose-project-options">
        <option v-for="cp in composeProjectOptions" :key="cp.name" :value="cp.name">{{ cp.name }}</option>
      </datalist>
      <div class="field">
        <label for="wiz-name">Name</label>
        <input id="wiz-name" v-model="serviceName" />
        <p class="hint">How it shows up in Ballast. Lowercase letters, numbers and dashes.</p>
      </div>
      <div class="actions">
        <button class="btn btn-primary" type="submit" :disabled="resticBlocked">Next</button>
      </div>
    </form>

    <!-- 2. Where -->
    <form v-else-if="step === 'where'" class="panel" @submit.prevent="next">
      <h3>Where should the backups be kept?</h3>
      <template v-if="destinations.length">
        <label v-for="d in destinations" :key="d.name" class="choice">
          <input v-model="destExisting" type="radio" name="dest" :value="d.name" @change="destMode = 'existing'" />
          <span class="check-text">
            <strong>{{ d.name }}</strong>
            <span class="hint">{{ d.location }}</span>
          </span>
        </label>
        <label class="choice">
          <input type="radio" name="dest" :checked="destMode === 'new'" @change="destMode = 'new'" />
          <span class="check-text"><strong>Somewhere new</strong></span>
        </label>
      </template>

      <template v-if="destMode === 'new'">
        <div class="field" :style="destinations.length ? 'margin-top: 1rem' : ''">
          <label for="wiz-dest-loc">Folder</label>
          <input id="wiz-dest-loc" v-model="destLocation" placeholder="/mnt/backups" />
          <p class="hint">
            A full path on this server. A different disk or a mounted NAS share protects you if this disk fails.
            Backups are encrypted, and Ballast creates the password for you.
          </p>
        </div>
        <details class="advanced">
          <summary>Advanced</summary>
          <div class="advanced-body">
            <div class="field" style="margin-bottom: 0.4rem">
              <label for="wiz-dest-name">Name for this place</label>
              <input id="wiz-dest-name" v-model="destName" />
            </div>
            <p class="hint">
              For cloud storage (B2, S3, SFTP) or custom retention, add it on
              <router-link to="/destinations">Where backups go</router-link> first, then pick it here.
            </p>
          </div>
        </details>
      </template>
      <div class="actions" style="margin-top: 1rem">
        <button class="btn btn-quiet" type="button" @click="back">Back</button>
        <button class="btn btn-primary" type="submit">Next</button>
      </div>
    </form>

    <!-- 3. When -->
    <form v-else-if="step === 'when'" class="panel" @submit.prevent="next">
      <h3>How often should it back up?</h3>
      <label v-for="p in schedulePresets.slice(0, 2)" :key="p.id" class="choice">
        <input v-model="scheduleChoice" type="radio" name="sched" :value="p.id" />
        <span class="check-text">
          <strong>{{ p.label }}</strong>
          <span v-if="p.id === 'nightly'" class="hint">Recommended for most things.</span>
        </span>
      </label>
      <label class="choice">
        <input v-model="scheduleChoice" type="radio" name="sched" value="none" />
        <span class="check-text"><strong>Only when I start it</strong></span>
      </label>
      <p class="hint">Other times, including custom cron expressions, can be set later on <router-link to="/schedule">When it runs</router-link>.</p>
      <div class="actions" style="margin-top: 1rem">
        <button class="btn btn-quiet" type="button" @click="back">Back</button>
        <button class="btn btn-primary" type="submit">Next</button>
      </div>
    </form>

    <!-- 4. Review -->
    <div v-else-if="step === 'review'" class="panel">
      <h3>Ready to start</h3>
      <dl class="kv" style="margin-bottom: 1.25rem">
        <dt>Back up</dt>
        <dd>{{ selectedEntry?.name }}, as <strong>{{ serviceName }}</strong></dd>
        <template v-for="p in selectedEntry?.parameters ?? []" :key="p.name">
          <template v-if="params[p.name]">
            <dt style="text-transform: capitalize">{{ p.name.replace(/[_-]/g, " ") }}</dt>
            <dd>{{ params[p.name] }}</dd>
          </template>
        </template>
        <dt>Keep backups in</dt>
        <dd>{{ destMode === "new" ? destLocation : destLabel }}</dd>
        <dt>Runs</dt>
        <dd>{{ scheduleLabel }}</dd>
      </dl>
      <div class="actions">
        <button class="btn btn-quiet" type="button" :disabled="starting" @click="back">Back</button>
        <button class="btn btn-primary" type="button" :disabled="starting" @click="start">
          {{ starting ? "Setting up…" : "Save and run the first backup" }}
        </button>
      </div>
    </div>

    <!-- Running -->
    <div v-else-if="step === 'running'" class="summary summary--running" role="status">
      <p class="summary-text">
        <span class="lamp lamp--running" aria-hidden="true"></span>
        Backing up {{ serviceName }} and checking it can be restored…
      </p>
    </div>

    <!-- Done -->
    <template v-else-if="step === 'done'">
      <div v-if="timedOut" class="notice notice--warn" role="status">
        <p><strong>Still running.</strong> Big first backups can take a while. Check the Overview later to see the result.</p>
      </div>
      <template v-else-if="result">
        <div class="summary" :class="`summary--${resultLight}`" role="status">
          <p class="summary-text">
            <template v-if="resultLight === 'green'">Your first backup is safe.</template>
            <template v-else-if="resultLight === 'amber'">Backed up, but not checked.</template>
            <template v-else>The first backup failed.</template>
          </p>
        </div>
        <div class="panel">
          <p v-for="t in result.tiers" :key="t.tier" style="margin-top: 0">
            <template v-if="result.tiers.length > 1"><strong>{{ t.tier }} copy:</strong> </template>{{ describeTier(t) }}
          </p>
          <p v-for="t in result.tiers.filter((t) => t.last_error)" :key="`e-${t.tier}`" class="notice notice--error">
            {{ t.last_error }}
          </p>
          <ul v-if="result.verification?.length" style="list-style: none; padding: 0">
            <li v-for="(v, i) in result.verification" :key="i">
              <span class="lamp" :class="v.passed ? 'lamp--green' : 'lamp--red'">{{ v.passed ? "Passed" : "Failed" }}</span>
              &nbsp;{{ describeCheck(v.method) }}
            </li>
          </ul>
        </div>
      </template>

      <div v-if="createdPasswordFile" class="notice notice--warn">
        <p>
          <strong>Keep a copy of your encryption password.</strong> It's saved on the server as
          <code>{{ createdPasswordFile }}</code> in Ballast's credential folder. Copy that file somewhere safe, away
          from this server: without it, nobody can restore these backups.
        </p>
      </div>

      <div class="actions">
        <router-link class="btn btn-primary" to="/dashboard">Go to the Overview</router-link>
        <router-link class="btn" to="/settings">Set up notifications</router-link>
      </div>
    </template>
  </template>
</template>

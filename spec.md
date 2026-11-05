# Ballast — Project Specification (v1.0)



 > **One-line pitch:** A self-hosted, service-aware backup tool for servers on bare metal, Docker and Proxmox: it knows how to correctly back up the apps people actually self-host (Immich, Vaultwarden, Postgres, …), *proves every backup restores*, and ships them to any restic-compatible destination (B2, S3, SFTP, local disk), with rclone as a secondary tier planned for Phase 2 (§12).



 - **Status:** Ready for implementation

 - **Engine:** Go 1.25 (single static binary, Vue UI embedded via `go:embed`)

 - **Name:** **Ballast** — binary `ballast`, Go module `github.com/<org>/ballast`

 - **Design reference:** The author's working production pipeline (restic/B2  systemd timers  ntfy verification) is the reference implementation; everything it does is the v0.1 feature list.



 ---



 ## 1. Problem statement



 Existing OSS backup tools (Backrest, restic-browser, etc.) treat backup sources as *paths on disk*. They don't know that an Immich backup requires a consistent `pg_dumpall` plus the library directory, or that Vaultwarden needs a stop→tar→start cycle. Nobody verifies that backups actually restore. **Ballast's moat is the service catalog  restore verification.**



 ## 2. Non-goals



 - **Do not rebuild restic orchestration.** Shell out to the `restic` binary; interop with Backrest repos where possible.

 - No Kubernetes support, no auto-discovery-of-everything in v0.1.

 - Not a sync/sharing tool — backup and restore only.

 - **No phone backup in v0.1** (deferred to Phase 2 — but the plugin interface must support it; see §4.2 and §12). Using the web UI from a phone is a different thing and is in scope (§20.5).

 - No telemetry of any kind.



 ## 3. Core design principles



 1. **Service catalog, pluggable providers.** Each service = a data definition (YAML): dump method, consistency strategy, verification method, file paths. Community contributions are data PRs, not code.

 2. **A backup isn't "done" until it's verified restorable.** Verification is built-in, not optional.

 3. **Fail loudly but non-fatally.** Per-service, per-tier reporting; a flaky secondary tier must never mask a primary-tier failure — and vice versa. (v0.1 ships the restic primary tier only; the secondary tier arrives in Phase 2, §12.)

 4. **CLI-first, GUI on top.** Every operation scriptable. Headless systemd/cron is a first-class citizen.

 5. **Trust by design.** Read-only-by-default, audit-friendly logs, secrets never in plaintext in the DB.



 ---



 ## 4. Architecture



 ```

 ┌─────────────┐ ┌──────────────────┐ ┌────────────────────┐

 │ Web GUI │──▶│ Scheduler/Engine │──▶│ Service strategies │

 │ (Vue, embed) │ │ (internal or │ │ (catalog-driven: │

 └─────────────┘ │ run-due via │ │ dump, stop-start, │

 │ systemd/cron) │ │ exec, verify hooks)│

 └────────┬─────────┘ └────────────────────┘

 │

 ┌────────────▼────────────┐

 │ restic (primary tier) │──▶ B2 / S3 / SFTP / local

 │ rclone (secondary tier) │──▶ Proton etc. (report-only; Phase 2)

 └────────────┬────────────┘

 ▼

 Notifications (ntfy/webhook/email)  status DB (SQLite)

 ```



 **Implementation:**

 - Single Go binary: `ballast serve` (API  embedded GUI), `ballast run-due`, `ballast backup|verify|restore|status|trigger`, `ballast wizard`.

 - SQLite via `modernc.org/sqlite` (pure Go, no cgo — preserves the static-binary property). Migrations are embedded, numbered SQL files (`internal/store/migrations/NNN_name.sql`) applied in order by a small built-in runner, each in its own transaction, tracked in a `schema_migrations` table; forward-only (fix a bad migration with a new one, never by editing an applied one).

 - restic (and, from Phase 2, rclone) invoked via `os/exec` — **never reimplemented**. Version-pin check at startup; warn loudly on mismatch.

 - Scheduler: built-in scheduler (cron expressions, per-job) **and** `ballast run-due` designed for systemd timers/cron for headless-first users.



 ### 4.1 Job state machine (normative)



 Every job run MUST transition through exactly this state machine, persisted in the DB:



 ```

 PENDING → PREPARING → DUMPING → UPLOADING → VERIFYING → SUCCESS

 │ │

 ▼ ▼

 FAILED FAILED

 ```



 - Each transition is recorded with timestamp, duration, and structured log lines.

 - **VERIFYING is mandatory.** A run that never reaches VERIFYING cannot be marked SUCCESS. If verification is impossible (e.g. destination temporarily unreadable), the run is AMBER (`success_unverified`), never green.

 - Cancellation: any state may transition to CANCELLED; dumps/stopped containers MUST be cleaned up (restart stopped containers, remove throwaway containers) in a `defer`-guaranteed cleanup path.



 ### 4.2 Service strategies (plugin interface)



 Each catalog entry selects one strategy:



 | Strategy | Behavior |

 |---|---|

 | `dump` | Run a dump command (typically `docker exec`) to a stdout/file artifact |

 | `stop-start` | Stop container → tar data dir(s) → guaranteed restart (defer) |

 | `files` | Backup filesystem paths directly via restic |

 | `exec` | **External contract:** run a user-supplied script; exit 0 = dump artifact produced; verification defined by a paired verify script. This is the Phase-2 mobile escape hatch — `pymobiledevice3` gets wrapped, never imported. |



 Every strategy pairs with a **verification hook** (§7) and declares its artifact paths.

 **Execution transports (v0.2 addition — see §17):** the `dump` strategy runs its command through a named transport: `docker-exec` (default; current v0.1 behavior), or `pct-exec` (Proxmox API `pct exec`, for bare-LXC guests with no Docker daemon). A catalog entry's `dumps[].transport` selects it; omitted defaults to `docker-exec`. `pct-exec` is read/dump-only — used for nothing but producing a backup artifact inside the guest (§17.4). This is separate from the `exec` strategy above, which remains the Phase-2 mobile escape hatch (§12) and is unaffected by transports.



 ---



 ## 5. MVP scope (v0.1)



 ### 5.1 Services (bundled catalog)



 | Service | Backup method | Consistency | Verification |

 |---|---|---|---|

 | **Immich** | `pg_dumpall` via `docker exec`  library/upload dirs | Postgres dump consistent; library path via restic directly | Restore into throwaway `ghcr.io/immich-app/postgres` container; assert no real psql ERRORs (filter benign `already exists`) |

 | **Vaultwarden** | Stop container → tar data dir → restart | Stop/start = guaranteed consistent | tar index test  container restart  login smoke test |

 | **Generic Postgres/MySQL/MariaDB** | `docker exec` dump | Dump consistent | Throwaway-container restore, error-filtered |

 | **Gitea** | `gitea dump` via `docker exec` | Official dump tool | Archive integrity  `gitea dump --type files` read-back |

 | **CouchDB** | Stop → tar → restart | Stop/start | tar index test  restart |

 | **Generic Docker volume** | Stop or snapshot → tar | Optional stop/start | tar integrity |

 | **docker-compose config snapshot** | Copy compose files  `docker inspect` export | N/A (static files) | Parse validation |

 | **Any folder / any DB you point at** | `files` / `dump` with user parameters | User-chosen | Archive integrity (mandatory minimum) |



 ### 5.2 Destinations



 - **Primary tier (restic backends):** B2, any S3-compatible (incl. Cloudflare R2), SFTP, local disk. One repo or per-service repos.

 - **Secondary tier (rclone remotes) — Phase 2, not in v0.1 (§12):** Proton Drive etc. — **report-only status, never blocks, never marks a job failed on its own.**

 - Retention policy per destination (default mirroring reference: 7d/4w/6m).



 ### 5.3 Key features



 1. **Web GUI:** pick services → pick destinations → schedule (per-service, incl. weekly-only) → traffic-light status (green / amber = unverified / red) per service per tier. Also: guided restore browsing.

 2. **Automatic verification jobs** (weekly, and after every backup by default): DB restores into throwaway containers, archive integrity, restic `ls`/`dump` read-backs.

 3. **Freshness alerting:** per-service max-age (nightly = 48h, weekly = 9 days); ntfy/webhook/email on failure AND heartbeat on success.

 4. **Restore UI  CLI:** browse snapshots, restore a file/volume/DB dump.

 5. **Secrets handling:** per-service password files (root-only, 0600), systemd `LoadCredential=`-compatible credential dir, never in plaintext in the DB or logs.

 6. **First-run wizard:** a brand-new homelaber goes `ballast wizard` → detected Docker services → pick one (Immich suggested) → pick local folder destination → first verified backup → success notification, **within 15 minutes** (this is an acceptance-tested requirement, §15).



 ---



 ## 6. Service catalog format



 Catalog entries are YAML files, validated against a versioned JSON Schema (`schema/catalog-v1.json`). Bundled entries live in `catalog/` (embedded in the binary); user entries in `~/.config/ballast/catalog.d/`. **Catalog as data, not code** — community flywheel.



 ### 6.1 Example (Immich)



 ```yaml

 # catalog/immich.yaml

 apiVersion: ballast.dev/v1

 service: immich

 version: 1

 display:

 name: Immich

 description: Photos & videos (Postgres  library)

 detect:

 docker:

 imagePattern: "ghcr.io/immich-app/immich-server"

 orEnv:

 IMMICH_VERSION: "*"

 parameters:

 - name: library_dir

 required: true

 prompt: "Path to your Immich upload/library directory"

 backup:

 strategy: dump

 dumps:

 - name: database

 container: "{{ .Services.database }}" # detected pg container

 command: ["pg_dumpall", "-U", "postgres"]

 format: sql

 files:

 - path: "{{ .Params.library_dir }}"

 consistency:

 notes: "pg_dumpall is transactionally consistent; library is static files"

 verify:

 method: container-restore

 image: "ghcr.io/immich-app/postgres:16" # MUST match service's extensions (pgvector/vchord)

 filterStderr:

 - "already exists" # benign during restore

 restartPolicy: never # dump strategy never stops containers

 notifications:

 failureVerbosity: full

 ```



 **Schema rules (normative):**

 - Unknown keys rejected (strict schema).

 - `{{ .Params.* }}` / `{{ .Services.* }}` templating limited to string substitution; no arbitrary expressions.

 - Every catalog entry MUST define at least the mandatory-minimum verification (archive integrity).

 **Discovery (v0.2 addition — see §17):** catalog entries MAY declare a `discovery` field per referenced container/guest, resolved at job-run time instead of hardcoding a container name:

 ```yaml

 discovery:

   database:

     type: docker-label

     match: "com.docker.compose.service=db"

     fallback: "{{ .Params.db_container }}"

 ```

 Resolved values are exposed to the entry as `{{ .Discovery.<name> }}`. This supersedes hardcoding (e.g. `immich.yaml`'s `db_container` parameter, noted as a deviation in §14 step 13) — §14 step 16 migrates bundled entries to this mechanism. Resolution failure MUST fail the job loudly; Ballast MUST NOT silently fall back past the entry's own declared `fallback`.



 ---



 ## 7. Verification framework (the moat)



 Every job's verification phase MUST run one or more of:



 1. **Archive integrity:** `gzip -t` / `tar -tf` / `unzip -t` equivalent on every artifact.

 2. **Throwaway container restore (DBs):** run dump through a fresh container of a *matching* image; parse stderr; assert zero non-benign ERRORs; destroy container. Key lessons (normative MUSTs):

 - Postgres image MUST match the service's extension set (pgvector/vchord for Immich) — the catalog encodes this, and Ballast MUST warn if the running container's image differs.

 - Postgres entrypoint only starts a server when the command is `postgres` — verification harness must override the entrypoint explicitly.

 - Error filtering is whitelist-style: only catalog-declared benign patterns are ignored; anything else fails verification.

 3. **Read-back:** `restic ls` and `restic dump` of a sampled file per path-set (also proves snapshot freshness).

 4. **Restart smoke tests** (stop-start services): container comes back healthy (`docker inspect` health status or restart  response check).



 Verification results are first-class records: per-check pass/fail with captured output, shown in GUI and `ballast status --verify`.



 ---



 ## 8. Hard-won lessons encoded as MUSTs



 These come from the reference implementation and are non-negotiable:



 - **Pipefail/SIGPIPE:** all shell pipelines in exec contexts must set `pipefail`; Go code must check both exit status and stderr, and treat EPIPE-closed readers distinctly from command failure.

 - **Credential dir differences:** paths differ under systemd vs sudo/interactive. Ballast MUST resolve the credential directory at runtime (respect `LoadCredential=` semantics via `$CREDENTIALS_DIRECTORY`) and log *which* dir was used, never the secret contents.

 - **restic output parsing:** `--compact` has no Paths column; `--path --latest` groups by path-set. The restic wrapper MUST pin to a tested output format version and fail safe (not silently) on parse surprises.

 - **(Phase 2) Proton (rclone secondary) requires a user keyring session** — secondary tier runs as a user unit context or documents the limitation; it is report-only regardless.

 - **Never trust a green light without verification** — §4.1.



 ---



 ## 9. CLI surface (v0.1)



 ```

 ballast wizard # guided first-run setup

 ballast serve # API  web GUI (single binary)

 ballast run-due # run all due jobs; exit non-zero on failure (systemd/cron)

 ballast backup <service> [--now]

 ballast verify <service> | --all

 ballast restore <service> [--snapshot <id>] [--file <path>] [--to <dir>]

 ballast status [--service X] [--json]

 ballast trigger <job> --token <t> # externally-triggered jobs (Phase-2 mobile: udev → systemd)

 ballast catalog list | validate | install <file>

 ballast destination add | test <name>

 ```



 All operations are also full REST API endpoints (`/api/v1/...`) with token auth; the GUI uses exactly this API (no privileged GUI-only paths).



 ---



 ## 10. Web GUI



 - **Stack:** Vue 3  TypeScript, built at release time and embedded via `go:embed`. No node requirement for users.

 - **Pages:** Dashboard (traffic-light per service per tier), Services (add from catalog, parameters), Destinations, Schedule, Restore (snapshot browser), Settings (notifications), Verification history (per-check detail).

 - **Beginner requirement:** all terminology surfaced with plain-English help text; never assume the user knows what "restic" or "repository" means. "Add destination" wizard includes B2/S3/R2 with copy-paste key fields and a `Test connection` button.

 - **GUI is a convenience layer** — every GUI action must be doable via CLI.



 ---



 ## 11. Notifications & alerting



 - Channels: ntfy (primary), generic webhook, email (SMTP).

 - Events: job failure (immediate), verification failure (immediate, distinct severity), freshness breach (nightly sweep), success heartbeat (configurable per-service).

 - Freshness defaults: nightly → max age 48h; weekly → 9 days.

 - **Per-tier isolation (from Phase 2, when the secondary tier lands):** a secondary-tier failure notification MUST NOT be worded or ordered such that it masks a primary-tier failure, and vice versa (test asserts notification content separation).



 ## 12. Phase 2 (post-v0.1, interface reserved now)



 **Scope note (v0.5):** "mobile" in this section means *backing up phones*. A mobile-friendly web UI (using Ballast from a phone) is in scope and is specified in §20.5.



 - **Phone backup:** iOS via `pymobiledevice3 backup2 backup` (wrapped through the `exec` strategy, encryption password as a credential file), Android via adb/seedvault-style local push. USB-attach trigger: udev rule → systemd unit → `ballast trigger <job> --token`.

 - **v0.1 MUST ship, and test with a stub script:** (a) the `exec` strategy end-to-end, (b) the `trigger` command with token auth. No mobile code ships in v0.1.

 - **Secondary rclone tier** (moved out of v0.1): the secondary tier itself arrives in v0.4 (§19.8) with Proton Drive (official CLI) as its first provider; rclone remains a Phase 2 *provider* for that tier — rclone remotes as a report-only tier that never blocks and never marks a job failed on its own (§5.2), with per-tier notification isolation tests (§11) and an acceptance check that a secondary-tier outage produces a report but never a primary job failure or masked notification.



 - Proton Drive as a *verified* tier: **delivered in v0.4** (§19.8) through Proton's official Proton Drive CLI with a file-based session store, which is the reliable headless path this item was waiting for.



 ---



 ## 13. Testing requirements



 **Every feature ships with tests.** Coverage target: ≥90% on engine, catalog, strategies, and verification packages. CI (GitHub Actions): lint (`golangci-lint`), vet, unit tests, integration tests with Docker (testcontainers-go or docker-in-docker), cross-build (linux/amd64, linux/arm64).



 | Layer | Approach |

 |---|---|

 | Unit | Engine logic, state machine, freshness rules, notification formatting, restic output parsing (golden fixtures from real restic output) |

 | Catalog | Every bundled catalog entry validated against schema; parameter templating round-trips |

 | Integration (Docker) | Real Postgres container: dump → throwaway restore → assert; stop-start strategy against a real container; cleanup-on-failure (kill mid-job, assert container restarted) |

 | restic | Test repo on local disk  SeaweedFS (S3-compatible; replaced MinIO, whose community edition is no longer maintained) for backend coverage; snapshot read-back assertions |

 | E2E | **No E2E test may pass without a job reaching `VERIFYING → SUCCESS`.** Wizard flow test: fresh state → configured Immich-ish service (fixture container) → verified backup < 15 min (relaxed in CI, strict locally) |

 | Security | Test that secrets never appear in DB, logs, or API responses; credential-dir resolution under simulated systemd env |

 | API | Golden-file REST contract tests (request/response pairs) so GUI and CLI stay in sync |



 ## 14. Implementation order (for the implementing agent)



 Each step lands with tests green and `ballast --help` working. Do not start a step until the previous one is complete. Progress is tracked by the checkboxes and step notes in this section.



 1. Repo scaffolding: Go module, `cmd/ballast`, Makefile, CI (build, vet, lint, test), `make build` → static binary embedding a stub page.


 2. Config loading (incl. `$CREDENTIALS_DIRECTORY`) and SQLite store with embedded migrations.


 3. Catalog schema and loader (JSON Schema validation; unknown keys rejected).


 4. Job state machine (PENDING → … → VERIFYING → SUCCESS / SUCCESS_UNVERIFIED / FAILED), persisted transitions, unit tests for every legal/illegal transition.


 5. Subprocess runner: pipefail semantics, SIGPIPE handling, credential-dir env (§8).


 6. Strategy plugins: `files`, `dump` (Postgres/MySQL catalog entries), `stop-start` (guaranteed restart, cleanup-on-failure tests), `exec` (stub-script tests only — Phase-2 interface reservation, §12).


 7. restic wrapper: init/backup/snapshots/ls/dump/restore, retention, read-back, against a local test repo; output parsing with golden fixtures.


 8. Verification runners: archive integrity and the throwaway-container restore harness (normative MUSTs from §7.2 enforced by tests).


 9. Scheduler, `ballast run-due`, and `ballast trigger` with token auth (stub-runner tests only — Phase-2 interface reservation, §12).


 10. Notifications (ntfy/webhook) and freshness alerting (fake-clock tests).


 11. REST API and CLI commands (status, snapshots, restore), wiring the real backup pipeline (catalog → strategy → restic → verify) behind the scheduler. Must include: a `files` backup end-to-end to a local restic repo recorded `SUCCESS` only after VERIFYING; a `dump` integration test against a real Postgres container; structured logging with secret-redaction tests.


 12. Vue web UI, built on the now-stable API: traffic-light dashboard, services, destinations, schedule, restore browser, wizard.


 13. Docs: quickstart (15-min wizard → verified backup) and catalog contribution guide, shipped together with the bundled Immich, Vaultwarden, and CouchDB catalog entries (§15) as its worked examples.


 **v0.1 is complete as of step 13. Steps 14+ below are the v0.2 extension (§17: multi-host Docker, Proxmox, remote destinations) — this list is the normative, session-by-session order `/implement` follows.**


 14. v0.1 parity — CLI commands: `ballast verify <service>|--all`, `ballast catalog validate|install <file>` (thin wrappers over existing engine/catalog logic, §9), plus a catalog data fix so the MySQL entry's `imagePattern` also matches `mariadb` images.


 15. v0.1 parity — secret references: a credential store (root-only files, `LoadCredential=`-compatible, §8) and `{{ .Secrets.<name> }}` catalog templating, so a password-protected DB can be dumped without a plaintext secret anywhere in config or DB. Blocks real-world DB backups until this lands — highest priority of the v0.2 extension.


 16. v0.1 parity — catalog discovery + transports: the `discovery` field (§6.1) and `pct-exec` transport (§4.2); migrate bundled catalog entries off hardcoded container-name parameters onto `{{ .Discovery.* }}`.


 17. Destination model refactor: backend-typed destinations (B2, any S3/R2, SFTP — restic backend strings generated from the credential store, §17.1) with credential forms and a connection-test button; migrate existing local-only config.


 18. DockerHost resources (§17.2): local-socket, remote-SSH, and remote-TLS Docker contexts; every Docker operation (inspect, exec, volume tar) takes a host parameter; per-host connection tests; UI host picker.


 19. Repo bootstrap + retention: `restic init` per destination, `forget --prune` per per-service retention policy (keep_daily/weekly/monthly), snapshot list in the UI.


 20. Docker volume backup: per-host volume picker, stop→tar→start and live-snapshot strategies, via `docker run --rm -v vol:/data alpine tar` so no host filesystem access is needed on remote hosts (§17.5).


 21. Verification method registry (§17.6): pluggable, catalog-referenced verification methods — `archive-integrity`, `db-restore-drill`, `restart-smoke` (container up + TCP/HTTP health check) — each enforced VERIFYING → SUCCESS per method, never green on an unimplemented or skipped method.


 22. Compose config snapshots: discover compose projects per Docker host via labels; snapshot `docker-compose.yml`/`.env`/labels, versioned; `.env` secrets redacted in the UI and never logged.


 23. Full-stack restore wizard v1: pick a date → restore compose config, volumes, and DB dumps for one project in dependency order.


 24. Proxmox: connection + inventory (§17.3) — API token setup guide, guest list, per-guest catalog-service attach (bare LXC → `pct-exec` transport, Docker guest → its DockerHost).


 25. Proxmox: vzdump orchestration — mode select, `UPID` polling, per-guest retention and dump pruning.


 26. Proxmox: verification drills — LXC config restore and in-guest DB restore, end-to-end, both layers verified independently (§7, §17.4).


 27. Freshness dashboard + per-tier notification routing upgrade: per-service, per-tier traffic lights; "last verified restore" age; secondary/remote-tier failures never mask primary-tier failures (§11).


 28. `ballast doctor` (Docker socket/context access per host, restic version, Proxmox API reachability and token perms, destination credentials, disk space for drills) and self-backup (Ballast's own catalog, config, and status DB).


 29. Docs & launch pass: 15-minute quickstart, Proxmox guide (incl. Ballast's own LXC/VM requirements, §17.7), Docker-host setup guide, migration page from v0.1.




 **v0.3 extension (steps 30–40, §18: friction removal + compose robustness) — this list is the normative order.**


 30. Notification channels (v0.3, §18.1): reusable `NotificationChannel` model (ntfy, full base URL — `https://ntfy.sh/` only the default, self-hosted first-class — with optional username/token from the credential store), test-send button on every channel through the same code path as job notifications, Settings UI page, `ballast notify channel …` CLI parity; existing per-service notification settings and the ntfy credential-file path migrate onto channels, not dropped.


 31. Email + webhook channels and event routing (§18.1): SMTP (host/port/TLS/STARTTLS/from/to, credentials from the store) and webhook (URL, optional HMAC secret, JSON payload, HTTP status shown on test-send); per-service, per-tier event routing (failure, verification failure, freshness, success heartbeat); recovery ("back to normal") notice and alert de-duplication/cool-down; notifications sent from the engine's success/fail/freshness paths so CLI-triggered and scheduled jobs notify identically (tested both ways).


 32. `.env` credential source (§18.2): third secret-store source `env-file` (`{path, key}`) with a real dotenv parser (quotes, comments, `export`, empty values, CRLF); resolved at job-run time, only path+key ever stored; loud PREPARING failure naming the exact file/key; UI/CLI "test this source" listing key names only; tests assert resolved values never reach DB, logs or API responses and that a rotated password is picked up with no re-setup.


 33. `.env` one-path setup (§18.2): Immich wizard becomes "path to your Immich directory" → `.env` found, `DB_USERNAME`/`DB_PASSWORD`/`DB_DATABASE_NAME` read automatically, confirmation screen only; catalog-generic mechanism (a catalog entry declares its documented `.env` keys) so any compose-based app gets the same setup.


 34. Compose discovery hardening (§18.4): label-driven discovery is primary (`project`, `project.working_dir`, `project.config_files` — trust the absolute paths, use the label's project name over the directory name), default-filename filesystem scan for stacks that are defined but not running (lookup order `compose.yaml` → `compose.yml` → `docker-compose.yaml` → `docker-compose.yml`), and explicit project-directory registration for label-less stacks; all per DockerHost, remote scans through the helper-container mechanism (§17.5).


 35. Compose snapshot completeness (§18.4): base file + auto-merged override files in Docker's merge order (recorded in snapshot metadata) + project `.env` + every `env_file:` target found by parsing the YAML; values fully present in the restic repo, fully redacted in every UI/API response; `docker compose config` parse check recorded as a verification check; headline CI test: the captured file set for every stack shape is sufficient to `docker compose up` from scratch.


 36. Daemon mode (§18.3): `listen` key in `ballast.yaml` and `--listen` flag (`--addr` kept as an alias) with the config file as the single source of truth; CLI commands become thin clients of the daemon's HTTP API (`--url` / `BALLAST_URL`), with safe single-shot fallback (`backup`, `verify`) and a hint when no daemon is running; `ballast config check`. The default listen address is NOT changed in this step (see step 37).


 37. Auth layer (§18.3, release blocker): token auto-generated at first start when none exists (printed once, written root-only; existing `ballast-api-token` installs untouched), CLI reads it from the credential dir automatically, login screen + `HttpOnly` session cookie for the UI, rate-limited login attempts, constant-time comparison, no token in logs, `ballast token rotate` + UI rotate/sign-out-everywhere, non-loopback/no-TLS exposure warning; a test enumerates every router route and asserts 401 without credentials. In the same change the default listen address becomes `0.0.0.0:8080`.


 38. Scheduler-in-daemon + `run-due` parity (§18.3): the daemon's scheduler is the default execution path and `ballast run-due` the headless fallback, both through the same `Coordinator`; tests prove a job run either way yields identical DB state transitions, notifications and verification results; fix any divergence found.


 39. systemd unit + docs (§18.3): shipped unit (`Restart=on-failure`, `LoadCredential=`, `ProtectSystem=strict`, `PrivateTmp`, `NoNewPrivileges`, with the `ReadWritePaths=`/Docker-socket access a real backup needs) passing `systemd-analyze verify`, documented `LoadCredential` smoke test that includes a backup under the hardened unit; docs networking section opens with the do-not-port-forward warning (reverse proxy with TLS or tailnet/VPN); quickstart updated for daemon-first setup, UI notifications, `.env` sources and compose coverage.


 40. Usability pass (§18.5): first-run checklist on the dashboard, weekly digest notification, Ballast health banner (last self-backup age, low disk, missing credentials from the doctor checks), `doctor` exposure check; final v0.3 acceptance sweep (§18.6).


 The secondary tier is not part of v0.1, v0.2 or v0.3; it arrives in v0.4 (step 49) with Proton Drive as its first provider, and rclone stays Phase 2 — see §12.



 **v0.4 extension (steps 41–51, §19: replace the scripts) — this list is the normative order.**


 41. Install + runtime (§19.1): `ballast install` (root) installs the shipped unit, creates `/etc/ballast` and `/var/lib/ballast`, moves an existing XDG install (invoking user's or root's) into them without loss, enables and starts the service, prints the UI URL and access key; idempotent, refuses to overwrite a different existing install. Root without `$HOME` falls back to those directories instead of failing. UI banner and `doctor` check when `serve` runs from a terminal rather than as a service. restic 0.18 output supported alongside 0.17.3 (golden fixtures for both); the version warning fires only outside the tested range.


 42. Remote host folders (§19.2, blocker): `backup.hostPaths` — absolute paths on the service's Docker host, archived by a read-only helper container (same plumbing as §17.5), live or with stop-start; `files` keeps meaning "on the Ballast machine". Paths validated on the service's host at `service add` and at PREPARING, before any container is stopped. Path auto-fill from the container's `docker inspect` mounts; the Vaultwarden and CouchDB entries switch to container mount points resolved at run time. Restore writes back through the helper, to a new folder by default. Tests use a second Docker daemon as the remote host.


 43. Excludes + keep-last (§19.3): `exclude` patterns on `files`, `hostPaths` and the folder service (restic `--exclude` or helper `tar --exclude`), UI "Skip these" list with a preview of what is skipped; `keep-last` retention at destination, service and target level.


 44. Destination targets (§19.4): a service has 1..N targets `{destination, parts, retention override}` of any destination type; existing services migrate to one target with all parts. One run produces artifacts once (one stop/start) and fans out: each target uploads, verifies and reports on its own; one target failing never hides another's result; the service light is the worst primary target. Parts selection per target (e.g. Immich library to B2 only), "Also send to…" in the add-service form, initial-upload progress, B2/S3 cost hint.


 45. Catalog completeness (§19.5): Gitea (`gitea dump --file - --type zip`), single-database MySQL/MariaDB as an app user with `MYSQL_PWD` secret and `mariadb-dump`/`mysqldump` probing, Postgres `db_user` parameter (prefilled from `POSTGRES_USER`), `host-config` multi-path entry; optional `dumps[].user` (`docker exec -u`); restore-drill dialect probes `mariadb`/`mariadb-admin` before `mysql`/`mysqladmin`. Each entry runs green against real images.


 46. Proxmox guest jobs (§19.6): service kind `proxmox-guest` (any guest; node, VMID, storage, mode, keep-last) through the shared engine — schedule, state machine, VERIFYING (guest-config drill + dump presence/size), light, notifications, freshness. Keep-last prunes only dump volumes Ballast recorded creating; the UI warns about shared storage and can create a dedicated `dir` storage. Optional copy of the dump to a restic destination as a target (step 44).


 47. Proxmox config snapshot (§19.6): built-in service reading via the API — cluster and node firewall rules, every guest config, storage definitions, pools, users/ACLs without secrets — stored as JSON artifacts, verified by parse-back and guest coverage; view/download only.


 48. Repository health (§19.7): scheduled per-destination `restic check` (default weekly, `--read-data-subset` configurable) with its own light and alerts; stale-lock handling — `restic unlock` (never `--remove-all`) only for a lock from this host, older than the run timeout, with no live Ballast process; anything else amber with an explanation.


 49. Second copy: Proton Drive (§19.8, amends §12): destination type `proton-drive` wrapping Proton's official CLI with a file-based session store in a `0700` folder under the credential directory and `ERROR` log level; sign-in from the UI (browser link shown, password/2FA never seen by Ballast); upload of a run's verified artifacts to `<folder>/<service>/<run date>/`, read-back verification (size check + rotating checksum download), keep-last via trash then delete of Ballast-named run folders only, per-artifact size cap (default 1 GB, skipped artifacts listed), per-destination lock, "sign in again" amber state on an expired session, CLI presence/version check in `doctor`. Report-only tier: never fails the job, never masks or is masked by the primary tier. Tests use a fake CLI replaying recorded fixtures.


 50. Custom entries in the UI (§19.9): catalog page creates custom entries from templates (database dump, stop-and-copy folder, volume, script) in a YAML editor with live schema validation identical to `ballast catalog validate`, installed into `catalog.d`; `exec-script` verifier registered (runs `verifyScript`; `exec` services without one stay amber).


 51. Preview + migration (§19.9): per-service preview (UI and `ballast backup --preview`) — parts, where each goes, estimated size, which containers stop and for how long, excludes applied, what reaches the second copy; nothing runs. "Replace a script" guide and dashboard checklist (parallel run, 7 green days, one UI test restore per service, then retire the script) with a script-idiom map in the docs; final v0.4 acceptance sweep (§19.11) reproducing both reference scripts.


 **v0.5 extension (steps 52–65, §20: findings from the first real migration) — this list is the normative order.**

 52. Repository locking that cannot starve verification (H1, §20.1): a pending prune must never block new read-backs on the same repository (retention after the batch of verifications, skipped-and-retried when the repo is busy, or restic's own lock as the only lock); "waiting for repository lock (held by <service>)" in the log and run status. Failing test first: held shared lock + pending retention + new read-back that must still complete.

 53. `service list` guard and job-creation behaviour (H2, H3, §20.1): `ballast service list` no longer panics for destination-less Proxmox guest jobs ("runs on Proxmox, no restic destination"); creating or editing a schedule or Proxmox job never runs it by surprise — the last successful run of any kind is the schedule baseline, "never backed up at all" stays the only always-due case and is said so; a separate explicit "run now".

 54. Edit Proxmox jobs and the main destination (H4, §20.2): `ballast proxmox job edit` (schedule, keep-last, copy-to) and `ballast service targets --primary NAME`, with API and UI parity, keeping run history and verification record.

 55. Drop-a-part cleanup with space preview (H5, §20.2): when a destination's parts change, offer to also remove old snapshots of the dropped part from that destination, show the space freed first, then `forget --prune` for exactly those snapshot ids.

 56. Disk projection in `doctor` and preview (H8, H9, §20.3): add up what services will put on each destination disk (last run size × retention) and warn above ~80% projected use, checked before a vzdump starts; `doctor` lists guests and reads one storage with the Proxmox token and names the missing role/path.

 57. Remote host files over SSH (H6, §20.2): a read-only "remote machine files" source with an allow-list (secrets such as Tailscale's node key opt-in), no install on the target. Design first: spec text for it is written before code.

 58. Per-job copy retention; compression guidance (H7, §20.1): a Proxmox job's `--keep-last` also applies to its copies (or the copy's retention is asked for), both policies printed after creation, the job card shows where each copy lives; docs on uncompressed dumps deduping better, per-job choice if vzdump allows. Confirm current behaviour with a test first.

 59. Docs: side-by-side exclusions and Proxmox token roles (H9 docs, H10, §20.3): `docs/replace-a-script.md` "Things to watch for" (exclude Ballast's folders from the old job first); `docs/proxmox.md` states the role needed for guest listing and settings snapshots and that a privilege-separated token needs its own ACL.

 60. Status shows phase (H11, §20.4): `ballast status` shows the phase and elapsed time of a running job; `ballast backup` says the daemon runs it and how to follow it. Shares one run-status field with steps 52 and 61.

 61. Upload progress in the web UI and `status` (R1, §20.4): latest restic progress per run and per destination kept in memory, added to the status the UI polls, a bar with "36.9 of 84.1 GiB" on the service card (percentage and bytes first, time left secondary); numbers and destination names only, blank after a daemon restart until the next report. Test first.

 62. Mobile-first redesign of the web UI (R2, §20.5): dashboard, services, destinations, schedule and restore work on a phone first and scale up; touch-sized controls, stacked cards on narrow screens, dark/light themes, no API changes. Prerequisite: the §12/AGENTS.md clarification that phone *backup* is Phase 2 while a mobile-friendly web UI is in scope (landed with the v0.5 spec text). Last in the order, after 61; built by Opus 5.5 or Fable 5.1 (operator's choice); screenshots at 375, 768 and 1280 px before and after; must not touch `internal/`.

 63. Green means every configured copy exists (H12, H13, H14, §20.1): a job with a copy target and no copy snapshot cannot be green and the light shows the copy's own result (H12 first, test first); the guest-copy read-back is logged and shown as its own phase ("verifying copy: downloading it back from B2"), with a full or bounded-sample choice for large single-file snapshots (H13); the Immich restore drill uses the live container's exact image so the warning goes away (H14).

 64. Restore drill readiness (H15, §20.1): the MySQL/MariaDB drill probes over TCP so it cannot start restoring on the image's temporary init server (same idea for `mysqladmin`, `pg_isready -h 127.0.0.1` for Postgres); failures say which phase failed. Test with a fake server that answers on the socket for 1 s, goes away, then answers on TCP.

 65. CouchDB behind a login (H16, §20.1): the built-in entry accepts 200 or 401 (or a catalog parameter for the expected status) and the failure message says a 401 probably means a password-protected server.

 **v0.6 extension (steps 66–78, §21: findings from running v0.5 in production) — this list is the normative order.**

 66. Alert decision log (J6, §21.1): one log line for every alert decision — sent (`sent verification_failure for utility-tracker to ballast`), held back (`alerted 23h ago, cool-down 24h`) or failed (with the reason) — and the last few decisions per service kept in the status DB (time, type, outcome, reason; never a topic, URL or token) and shown in the service detail view and `ballast status --json`. First, so step 67 can be seen working. Test: a held, a sent and a failed delivery each log and store one decision, and no secret reaches either.

 67. Alert reminders per scheduled run (J5, §21.1): a job that keeps failing gets one reminder per scheduled failure, not "one per 24 hours plus luck". The effective cool-down never exceeds 90% of the service's schedule gap (24h when unscheduled), so a daily job failing at 05:00:15 and then 05:00:10 still reminds. Failing test first with a fake clock; the decision log from step 66 shows why each alert was sent or held.

 68. CLI output hygiene (J1, J2, J3, §21.2): results go to stdout, notices and errors to stderr (`SetOut` once, plus a test that runs every list/show command with the two streams captured separately); `ballast status <service>` takes the service positionally (`--service` stays as an alias) and every command that takes no positional arguments rejects extras with a message that says what to use instead; the "using credential directory" notice appears only with `--verbose`, in `doctor`, or when the directory is not the default.

 69. Repository health in the CLI (J7, §21.3): `destination list` and `ballast status` show each destination's health light and when it was last checked and last OK; new read-only `ballast destination health [--json]` reads the same data as `GET /api/v1/destinations/health`; a destination never checked says so instead of showing nothing.

 70. Restore from a chosen destination (J11, §21.3): `--from <destination>` on `ballast restore` and `ballast restore-project`, the same choice in the API and a picker in the web UI; it must be one of the service's configured targets, and the snapshot is resolved inside that repository under the same service-tag guard. The default stays the main destination. Test with two local destinations holding different snapshots, and a destination that is not the service's target refused.

 71. Scheduled self-backup with retention (J8, J14, §21.4): `ballast self-backup` gets a schedule (default daily after the nightly batch, to every destination, opt-out), its own light and freshness alert on the dashboard like any service, and retention — `forget --prune` scoped to the `ballast-self` tag, same default policy as a service, only after a verified self-backup. Creating the schedule never runs it by surprise (v0.5 D2). Test: a scheduled run reaches verified, prune keeps only the policy's snapshots, and a failing self-backup alerts without touching any service's light.

 72. Self-backup includes the scripts custom entries run (J12, §21.4): every local script that an `exec` strategy or an `exec-script` verifier points to is saved under `scripts/` in the self-backup and listed in its output; `doctor` warns when a referenced script is missing or covered by no backup. Credential files stay excluded (rule 2) and the docs say not to put secrets inside scripts. Test with a custom entry that names a script.

 73. Version stamping and release tags (J9, §21.5): the Makefile's default `VERSION` becomes `git describe --tags --always --dirty`; the version is shown by `ballast version`, in `doctor`, in the API and in the web UI footer; `docs/upgrading.md` (step 74) names the tagging step (`v0.5.0`, tagging is the maintainer's action, not code). Test: a build with no tag still reports the commit, never `dev`.

 74. Upgrade guide and idle check (J10, §21.5): `ballast wait-idle [--timeout]` exits 0 when no run is in progress and non-zero on timeout, and `ballast status --running` prints and exits with whether anything is running; `docs/upgrading.md` gives the manual procedure (wait idle, copy `ballast.db`, keep the old binary, swap, restart, `doctor`) and how to roll back; every migration states whether it can be rolled back, and `doctor` shows the schema version. `ballast install --upgrade` stays out until the guide has been used in earnest.

 75. System-install CLI (J4, §21.6): `ballast install` also puts `ballast` on `PATH` (`/usr/local/bin/ballast`); when `/etc/ballast` exists and the caller is root, or the daemon is reachable, the CLI uses the system config and data paths without environment variables; a non-root user is told to run with `sudo` rather than getting an empty or mismatched view; commands go through the daemon's API where it is running. Confirm the inferred cause with a failing test first.

 76. Status exit codes for scripts (J13, §21.2): `ballast status --check` exits 0 when every light is green, 1 for amber or stale, 2 for red and 3 when status cannot be read, and prints one summary line; plain `ballast status` keeps exiting 0. For watchdogs and cron mail without parsing text. Depends on step 68.

 77. Heartbeat to an external monitor (J15, §21.7): optional credential file `ballast-heartbeat-url`; while the daemon's scheduler loop is alive it requests that URL on an interval (default 5 minutes) so a dead daemon, a stuck loop or a powered-off machine is noticed by something that is not Ballast. The URL is a secret (rule 2: root-only file, stripped from errors and logs); a failed ping is logged and never affects a backup; `doctor` reports whether a heartbeat is configured and when it last succeeded. Test with a stub server and a stopped loop.

 78. Support bundle (J16, §21.7): `ballast support-bundle [--output file]` writes one archive for bug reports and for asking someone else for help — version, `doctor --json`, status, recent alert decisions, schema version, restic/docker versions and catalog entry names — with every secret absent (the existing redaction harness in `cmd/ballast/redaction_test.go` is extended with seeded secrets across the new files). Nothing is sent anywhere.

 **v0.7 extension (steps 79–87, §22: findings from a QA and UX review of v0.6) — this list is the normative order. Its step A (commit the fixes from a seeded-instance QA pass, listed in §22.1) is already done in commit 833984b and has no number here.**

 79. Server-side folder path validation (K3, §22.2): the service layer, not only the web UI, trims the path, requires it to be absolute and refuses a relative one; a path that does not exist is saved with a warning, not refused, because the folder may be on a mount that is not up yet. The CLI and API get the same behaviour. Test: a relative path and a path with a trailing space (trimmed) through the API and the CLI, and a missing absolute path saved with a warning.

 80. Fast `GET /api/v1/status` (K5, §22.3): the cause is not known (indexes exist), so profile first. Build a 60-service, 3,200-run fixture and a `go test -bench` benchmark that fails above 200 ms, confirm where the time goes, then fix that cost. Observed: about 1.0 to 1.2 s before the fix.

 81. Saving a schedule keeps its paused state (K7, §22.4): saving never re-enables a paused schedule unless the user turns it on; the API returns `next_run`, and the Schedules page confirms "Saved. Next run: Tue 03:00." and shows the next run time. Test: save a paused schedule and it stays paused; save an enabled one and `next_run` is returned.

 82. Run history API (K1, §22.5): `GET /api/v1/services/{name}/runs` (paged, newest first) and `GET /api/v1/runs/{id}` with state, duration, size, verification result and log tail. Secrets never appear in a log tail or any field (rule 2); a test seeds secrets and asserts none comes back. Same shape for the CLI where it already lists runs.

 83. History view (K1, §22.5): a History tab on the service page lists past runs and opens one to show its verification detail and log tail, so a failed run can be understood from the UI. Depends on step 82. No API changes.

 84. Cancel a running backup (K2, §22.6): `POST /api/v1/runs/{id}/cancel`, `ballast cancel <service>` and a Cancel button on a running card, using the existing CANCELLED state. A cancelled run is CANCELLED, never SUCCESS (rule 1), the restic repository is left unlocked, and a stop-start container is restarted. Test with a fake slow job: cancel mid-run, assert the state, the unlocked repository and the restarted container. Largest step of v0.7.

 85. Behaviour and wording (K6, K4, K9, §22.7): removing a service while it backs up shows "Waiting for the running backup to finish…" in the dialog and the refusal path is tested with a fake slow job (K6); restore errors drop the internal "service notes: invalid input:" prefix and "restore into a path" restores relative to the backup root instead of nesting the full absolute path (K4); a service older than half its expected interval shows its age in amber text without changing its light (K9). Failing tests first for K4 and K9.

 86. Accessibility and tall pages (K10, §22.8): a skip-to-content link; touch targets of about 44 px for the status legend buttons and checkboxes; the wizard's raw restic version warning moves behind "Details"; "Stop backing up" is renamed "Remove" and says what is kept; Notifications and Schedules get the same search, grouping and folding as Backups. Web only; must not touch `internal/`.

 87. Sessions across restarts (K8, §22.9): sessions live in memory, so every server restart signs the browser out. First decide whether that is by design; this needs the maintainer's decision. If it is not by design, persist session hashes (never raw tokens) in SQLite. Do not implement before the decision is recorded in the pull request or spec change that implements it.



 ## 15. Acceptance checklist (v0.1 "done")



 - [ ] Fresh machine → `ballast wizard` → first **verified** backup in ≤15 min, no manual docs needed

 - [ ] Immich entry (or fixture equivalent): dump  library  throwaway restore, all green

 - [ ] Vaultwarden-style stop-start: container guaranteed restarted even when the job is killed mid-run

 - [ ] Unverifiable backup shows **amber, never green**

 - [ ] Secrets never in DB, logs, or API responses (test asserts this)

 - [ ] Freshness alerts fire at configured max-age (tested with fake clock)

 - [ ] Every CLI operation has an API equivalent and a test

 - [ ] Single static binary for linux/amd64  linux/arm64; no runtime dependencies beyond docker/restic

 - [ ] No telemetry; repo ships with README quickstart aimed at a homelab newcomer

 - [ ] `exec` strategy  `trigger` tested with a stub — no mobile code shipped

 - [ ] Coverage ≥90% on core packages; CI enforces lint  tests  cross-builds



 ---



 ## 16. Risks & mitigations



 | Risk | Mitigation |

 |---|---|

 | Trust barrier — people must trust a backup tool with everything | Read-only-by-default, audit logs, no telemetry, open catalog format |

 | Restore verification cost (minutes, needs Docker) | Scheduled  parallel verification jobs; amber-until-verified status |

 | restic output format drift | Pinned parse version, fail-safe on surprise |

 | Arkive (Unraid) generalizing | Speed; differentiate on verification  general Docker  (Phase 2) mobile |

 | Name collisions | Checked: "Ballast" is clear in the self-hosted/backup space; verify Docker Hub `ballast` image name and domain (`ballast.sh` / `getballast.io` / `useballast.dev`) before public launch |



 ---



 ## 17. v0.2 extension: multi-host & Proxmox (normative)



 **Terminology note:** §12 above calls the mobile/phone work "Phase 2." This v0.2 extension (multi-host Docker + Proxmox) is a *separate*, earlier release that ships before mobile; it does not change §12's scope or timeline, and mobile remains phone-code-free until its own phase regardless of what v0.2 number it ends up carrying. This section is the normative subset `/implement` and future spec changes must agree with.



 **Goal:** back up Docker stacks across multiple Docker hosts, a Proxmox tier (VMs, bare LXCs, and Docker guests), and cloud destinations — all from the web UI — plus close the v0.1 parity holes in §14 steps 14–16.



 **Deployment reality this section supports:** Ballast runs in its own unprivileged LXC (or small VM) and backs up services running bare-metal in LXCs, in Docker inside other LXCs, and in Docker inside VMs.



 ### 17.1 Remote destinations



 B2, any S3-compatible (incl. Cloudflare R2), and SFTP via restic backends, generated from the credential store as restic backend strings (R2 configured as S3-compatible). UI credential forms plus a connection-test button that validates credentials, not just reachability.



 ### 17.2 Multiple Docker hosts



 `DockerHost` resources: `local` (socket), `remote-ssh` (`ssh://user@host` — preferred for homelabs, no cert management), `remote-tls` (TCP 2376 with CA/client certs in the credential store). All Docker operations (inspect, exec, volume tar) run against a named host, never assume local.



 ### 17.3 Proxmox tier



 Proxmox via API, not SSH: an API token scoped to `VM.Backup` on `/vms/**`; `vzdump` triggered via its API endpoint and polled by `UPID`; in-guest commands via `pct exec` through the API, documented as read/dump-only.



 ### 17.4 Guest-aware layering



 Each Proxmox guest can attach catalog services. Bare LXC → `pct-exec` transport (§4.2). Docker guest → its `DockerHost` (§17.2). Flow: in-guest dump → restic the dump → then `vzdump` the whole guest. Verification (§7) drills both layers independently — an in-guest DB restore drill and a guest-config restore are separate, both-MUST-pass checks, per the isolation-of-failure principle (rule 3, `AGENTS.md`).



 ### 17.5 Docker volume backup



 Any named volume, per-service consistency strategy, via `docker run --rm -v vol:/data alpine tar` through the Docker API — identical on local and remote hosts, no host filesystem access required.



 ### 17.6 Verification method registry



 Verification methods become a pluggable, catalog-referenced registry: `archive-integrity`, `db-restore-drill` (the §7.2 throwaway-container restore, renamed for registry consistency), and the new `restart-smoke` (container up + TCP/HTTP health check returns 200) — closing the Vaultwarden login-smoke-test gap noted in §14 step 13. Every method is enforced VERIFYING → SUCCESS per §4.1; an unimplemented or skipped method is a hard failure, never a silent pass.



 ### 17.7 Deployment notes (Ballast itself)



 - **LXC (recommended for homelabs):** unprivileged, with `nesting=1,keyctl=1` if verification drills run inside it; on ZFS-backed storage use a dedicated docker dir or fuse-overlayfs. Local Docker stacks need the socket mounted; remote hosts need nothing mounted.

 - **VM alternative:** cleaner isolation, ~2 GB RAM, no nesting caveats — the safer default for operators who'd rather not reason about Docker-in-LXC-in-LXC.

 - **Trust posture:** Ballast executes *inside* other guests only via read/dump transports (`docker exec` for dumps, `pct exec` for dumps) — document this explicitly; it is the trust-sensitive edge of the product (rule 5, core design principles §3).



 ### 17.8 Out of scope (still)



 Kubernetes, multi-machine orchestration beyond one Proxmox host plus its Docker hosts, auto-discovery of arbitrary apps, and any rclone secondary-tier expansion beyond report-only (§12).



 ### 17.9 Acceptance checklist (v0.2 "done")



 - [ ] Beginner: install → B2 destination → Immich backup → verified restore drill, entirely in the UI

 - [ ] Password-protected MySQL/MariaDB backs up with credentials from the store (§14 step 15)

 - [ ] Catalog entries use discovered container names; no hardcoded defaults (§14 step 16)

 - [ ] `ballast verify`, `catalog validate`, `catalog install` work as spec'd (§9, §14 step 14)

 - [ ] Containers on a *second* Docker host (SSH or TLS context) can be backed up and verified

 - [ ] Bare-LXC service: `pct exec` in-guest dump before `vzdump`, both layers verified

 - [ ] Proxmox user: API token → vzdump + guest DB dumps → proven restore

 - [ ] A whole compose project (config + volumes + DB) rebuilds from one date selection

 - [ ] Secondary/remote-tier failures never mask primary-tier failures (rule 3, `AGENTS.md`)

 - [ ] Secrets never in DB, logs, or API responses (tested, not assumed)

 - [ ] `ballast doctor` catches the top 10 misconfiguration causes

 - [ ] Docs: 15-minute quickstart, Proxmox guide (incl. Ballast's own LXC/VM requirements), Docker-host guide, migration page


 ---



 ## 18. v0.3 extension: friction removal & compose robustness (normative)



 **Goal:** close three usability gaps found in real use (notifications only via CLI, credentials pasted by hand, Ballast only reachable from localhost) and make compose discovery miss nothing. Builds on v0.2. No mobile code (rule 6); the `exec` strategy and `ballast trigger --token` interfaces are untouched.



 ### 18.1 Notifications in the web UI

 - `NotificationChannel`: `type` (`ntfy` | `webhook` | `email`), settings, and per-event routing (failure, verification failure, freshness breach, success heartbeat, recovery) per service and per tier (§11). Channels are reusable objects referenced by services.

 - **ntfy** takes a full base URL (`https://ntfy.sh/` is only the default; self-hosted is first-class); optional username/token from the credential store. **Email:** SMTP host/port/TLS/STARTTLS/from/to, credentials from the store. **Webhook:** full URL, optional HMAC secret, JSON payload.

 - Every channel has a **test-send** that uses the same code path, auth and priority as real notifications, titled "Ballast test notification"; webhook test shows the HTTP status.

 - Sending happens in the engine's success/fail/freshness paths, never the UI: CLI-triggered and scheduled jobs notify identically. A failure notification is de-duplicated (one alert, then a reminder per cool-down, default 24 h); a return to green sends one recovery notice. Per-tier isolation (§11, rule 3) is unchanged.

 - Secrets (ntfy token, SMTP password, HMAC secret) live in the credential store and never appear in the DB, logs or API responses.



 ### 18.2 `.env` credential sources

 - The secret store gains source type `env-file` (`{path, key}`). The parser implements real dotenv semantics: `KEY=value`, double- and single-quoted values, `#` comments, blank lines, `export KEY=value`, empty values, CRLF line endings.

 - Resolved at **job-run time**, never at setup time; only path + key are stored, never the value. A missing file, missing key or unreadable path fails the job at PREPARING with a message naming the exact file and key — never an empty password.

 - A catalog entry MAY declare the documented `.env` keys it needs, giving a one-field setup ("path to your app's directory"): Immich reads `DB_USERNAME`, `DB_PASSWORD`, `DB_DATABASE_NAME` this way and shows a confirmation screen only. The UI's "test this source" lists key names found, never values.



 ### 18.3 Daemon-first serving & auth

 - `ballast serve` is the primary mode. Listen address comes from `listen` in `ballast.yaml` or `--listen` (`--addr` remains an alias); the default is `0.0.0.0:8080`, **switched on only together with the auth layer below**. The config file is the single source of truth for daemon and CLI.

 - CLI commands are thin clients of the daemon API (`--url` / `BALLAST_URL`). With no daemon reachable, single-shot `backup` and `verify` fall back to direct execution against the same config and print a hint that `serve` is the recommended permanent mode.

 - **Authentication is a release blocker.** A token is generated at first start when none is configured, printed once and stored root-only (an existing `ballast-api-token` credential is honoured unchanged); the CLI finds it in the credential dir. Every API route except a health endpoint requires it. The UI shows a login screen and holds an `HttpOnly` session cookie (`Secure` behind a TLS-terminating proxy). Constant-time comparison, no token in logs, login attempts rate-limited, token rotation supported. Single token, single user (no RBAC).

 - The scheduler inside the daemon is the default execution path; `ballast run-due` is the headless fallback. Both go through the same engine/coordinator: identical state transitions, notifications and verification results for the same job.

 - A systemd unit ships with the release: `Restart=on-failure`, `LoadCredential=` for the token, `ProtectSystem=strict`, `PrivateTmp=true`, `NoNewPrivileges=true`, with the write paths and Docker socket access a real backup needs. Docs open the networking section with: **do not port-forward Ballast to the internet** — use a TLS reverse proxy or a tailnet/VPN.



 ### 18.4 Compose coverage

 - **Filenames Docker accepts:** default lookup order `compose.yaml`, `compose.yml`, `docker-compose.yaml`, `docker-compose.yml`; auto-merged `compose.override.yaml/.yml` and `docker-compose.override.yaml/.yml`; explicit `-f` stacks; legacy v1 files.

 - **Discovery order:** (1) container labels per DockerHost — `com.docker.compose.project`, `.project.working_dir`, `.project.config_files` are authoritative (absolute paths, project name from the label, not the directory); (2) a filesystem scan of configured directories, for defined-but-not-running stacks; (3) explicit registration of a project directory for label-less stacks. Remote hosts use the helper-container file access of §17.5.

 - **Snapshot contents:** base file, overrides in merge order (order recorded in snapshot metadata), the project `.env`, and every `env_file:` target referenced by the YAML. Values are complete in the encrypted restic repo and redacted in all UI/API output and logs. The captured set must be sufficient to `docker compose up` the stack from scratch (CI-proven for every stack shape); a `docker compose config` parse check is recorded as a verification check.



 ### 18.5 Usability additions

 First-run checklist on the dashboard, weekly digest notification, Ballast health banner (self-backup age, disk space, missing credentials), `ballast config check`, and an exposure warning in `doctor` and the UI when listening beyond loopback without a TLS proxy.



 ### 18.6 Acceptance checklist (v0.3 "done")

 - [ ] A beginner sets up self-hosted ntfy entirely in the UI, presses test-send and receives the notification

 - [ ] Email and webhook channels work with test-send; failures and recoveries notify once, not repeatedly

 - [ ] Immich setup is one path entry; `.env` credentials are read at run time and a rotated password works on the next run

 - [ ] Every compose filename variant, override combination and custom `-f` stack is discovered and snapshotted; restored snapshots can `docker compose up` from scratch (CI)

 - [ ] `.env`/`env_file` values are present in the restic snapshot and redacted in every UI/API response

 - [ ] `ballast serve` runs permanently under the shipped systemd unit on `0.0.0.0:8080`; no API route or UI page is reachable without credentials

 - [ ] CLI works as a daemon client and as a standalone fallback with identical outcomes; scheduler and `run-due` parity is tested

 - [ ] Secrets never in DB, logs or API responses — re-asserted for `.env` and notification-auth paths

 - [ ] Docs: networking section leads with the do-not-port-forward warning; quickstart updated



 ## 19. v0.4 extension: replace the scripts (normative)



 **Goal:** every step of a real homelab's two backup scripts — an app-level script on the Docker VM and a host-level script on the Proxmox node — is reproducible in Ballast, set up from the web UI, and safe to run in parallel with those scripts until they are retired. Each requirement below is a gap hit while attempting that migration. No mobile code (rule 6); the `exec` strategy and `ballast trigger --token` interfaces are untouched.



 **Ownership rule (applies to all of §19):** Ballast never modifies or deletes data it did not create — dump volumes, remote folders, repository contents. A local destination refuses a non-empty folder that is not a restic repository. This is what makes a parallel run safe.



 ### 19.1 Install & runtime

 - `ballast install` (root) sets up the shipped unit (§18.3), `/etc/ballast` and `/var/lib/ballast`, migrates an existing XDG install without loss, and starts the service. It is idempotent and never overwrites a different install.

 - A root process without `$HOME` uses `/etc/ballast` and `/var/lib/ballast`. `doctor` and the UI warn when `serve` runs from a terminal rather than as a service.

 - Supported restic versions are a tested range (0.17.3 and 0.18.x at v0.4), each with golden parsing fixtures.



 ### 19.2 Remote host folders

 - `backup.hostPaths`: absolute paths on the service's Docker host, archived through a read-only helper container on that host (§17.5 plumbing), live or inside a stop-start window with the usual restart guarantee. `files` keeps meaning paths on the Ballast machine; for the `local` host the two are equivalent. Never SSH file access.

 - Paths are validated on the service's host at `service add` and at PREPARING, **before any container is stopped**. A path is never interpreted as a volume name, or the reverse.

 - Paths may be given as a container mount point and resolved from `docker inspect` at run time; the bundled Vaultwarden and CouchDB entries do this.

 - Restore writes back through the helper, to a new folder unless the user confirms overwriting.



 ### 19.3 Excludes & retention

 - `exclude` patterns on `files`, `hostPaths` and the folder service; the UI previews what is skipped.

 - `keep-last` joins `keep-daily/weekly/monthly` at destination, service and target level.



 ### 19.4 Destination targets

 - A service has one or more targets: `{destination, parts, retention override}`, any mix of destination types. Parts are the artifacts and path groups the catalog entry names.

 - One run produces its artifacts once (one stop/start window) and fans out. Each target uploads, verifies (restic read-back per destination; restore drills once per artifact) and applies retention independently, with its own light and notifications. One target failing never hides another's result (rule 3). The service light is the worst of its primary targets.



 ### 19.5 Catalog

 - Bundled entries added or extended: Gitea (`gitea dump` to stdout, zip integrity), single-database MySQL/MariaDB as an app user (`MYSQL_PWD` as a secret), Postgres with a `db_user` parameter, and `host-config` (system units, scripts, chosen `/etc` files).

 - `dumps[].user` runs a dump as a given container user. The MySQL-family restore drill works with images that ship only `mariadb`/`mariadb-admin` clients.

 - No automatic fallback strategies: a failed dump is a failed run.



 ### 19.6 Proxmox guest jobs & config

 - `proxmox-guest` is a service kind: any guest, scheduled and run through the shared engine (state machine, VERIFYING, lights, notifications, freshness). Verification is the guest-config restorability drill plus dump presence and size.

 - Keep-last prunes only dump volumes Ballast recorded creating. A guest dump can be copied to a restic destination as a target (§19.4).

 - A built-in Proxmox config service captures, through the API only (§17.3), cluster and node firewall rules, guest configs, storage definitions, pools and users/ACLs without secret fields, as JSON artifacts verified by parse-back. Ballast never writes Proxmox config.



 ### 19.7 Repository health

 - Each destination has a scheduled `restic check` (default weekly, configurable read-data subset) with its own light and alerts.

 - Stale locks are removed with `restic unlock` (never `--remove-all`) only when the lock belongs to this host, is older than the run timeout and no Ballast process holds it; otherwise the run is amber with an explanation.



 ### 19.8 Second copy: Proton Drive

 - Destination type `proton-drive` wraps Proton's official Proton Drive CLI (rule 4). Session state is file-based (`PROTON_DRIVE_CREDENTIALS_STORE=unsafe_file`) in a `0700` folder under the credential directory (rule 2), with CLI logging at `ERROR`; no desktop keyring.

 - Sign-in happens from the UI: Ballast runs the CLI's browser sign-in and shows the link; the Proton password and 2FA code never pass through Ballast. An expired session shows "sign in again".

 - After a run is verified, its verified artifacts (not the restic repository) are uploaded to `<folder>/<service>/<run date>/`. The copy is itself verified: sizes confirmed and one artifact per run downloaded and checksum-compared; an unreadable copy is amber, never green.

 - Keep-last trashes and then deletes only Ballast-named run folders. A per-artifact size cap (default 1 GB) skips and lists oversized artifacts; per-target parts selection chooses what is copied.

 - This is the secondary tier of §12: report-only, never fails the job, and never masks or is masked by the primary tier. rclone remains a Phase 2 provider for it.



 ### 19.9 Custom entries, preview & migration

 - The UI creates, validates (same rules and messages as `ballast catalog validate`) and installs custom catalog entries from templates. The `exec-script` verification method is registered; `exec` services without a `verifyScript` are amber.

 - Every service has a preview (UI and `ballast backup --preview`): parts and their targets, estimated size, containers stopped and for how long, excludes, what reaches the second copy. Nothing runs.

 - Docs and a dashboard checklist guide replacing a script: parallel run, seven green days, one test restore per service from the UI, then retire the script.



 ### 19.10 Out of scope (v0.4)

 Mobile (§12); importing old script archives into Ballast's history; writing Proxmox config; rclone providers; multi-user auth/RBAC, OIDC, TLS/ACME; automatic fallback strategies.



 ### 19.11 Acceptance checklist (v0.4 "done")

 - [ ] Every step of both reference scripts has a Ballast equivalent, set up from the web UI except `ballast install`

 - [ ] Services on remote Docker hosts with bind-mounted data back up green with no change to their compose files; a missing path fails before any container stops

 - [ ] One run lands on a local and a cloud destination with one stop/start; a per-target parts choice sends a large library to the cloud only

 - [ ] Proxmox guest backups run on a schedule with lights and alerts and never prune a dump Ballast did not create; Proxmox config is captured nightly

 - [x] Each destination is `restic check`ed on a schedule with its own light

 - [ ] Proton Drive is signed in from the UI and receives verified, read-back-checked copies of config and database backups as a separately reported tier

 - [ ] Ballast and the old scripts run side by side for seven days without touching each other's files, buckets or dumps

 - [ ] Every service restored once from the UI

 - [ ] Secrets never in DB, logs or API responses — re-asserted for the Proton session folder, database passwords and the Proxmox config snapshot


 ## 20. v0.5 extension: findings from the first real migration (normative)



 **Goal:** fix what the first real replacement of a homelab's scripts by the v0.4 build turned up (2026-10-07): verification that stalls behind an unrelated upload, a green light that can hide a missing copy, jobs that cannot be edited, missing disk and permission safety nets, and no live view of a running backup. Each requirement below was hit in practice or traced to code; the findings are tagged H1–H16 and R1–R2. No mobile *backup* code (rule 6); the `exec` strategy and `ballast trigger --token` interfaces are untouched.



 ### 20.1 Reliability and truthful lights

 - A pending retention step never blocks a new restic read-back on the same repository; an unrelated long upload never leaves a finished backup un-verified. When a run waits for a repository lock, the log and the run status say which service holds it.

 - `ballast service list` works when a service has no restic destination (Proxmox guest jobs).

 - Creating or editing a schedule or a Proxmox job never starts a run by itself. The baseline for "due" is the last successful run of any kind; only a service that was never backed up is due immediately, and the CLI and UI say so.

 - A job that has a configured copy target is never green unless that copy exists and is verified; the light shows the copy's own result. A Proxmox job's keep-last applies to its copies too, and both policies are shown after the job is created.

 - Verification of a large single-file copy (a guest dump) is logged and shown as its own phase; the operator can choose a full read-back or a bounded sample, with the trade-off stated.

 - The Immich restore drill uses the live container's exact image. The database restore drills confirm readiness over TCP so they cannot begin on an image's temporary init server, and a failure names the phase that failed. The CouchDB check treats a 401 as "running, password-protected" and says so.



 ### 20.2 Editing and cleanup

 - Proxmox jobs can be edited (schedule, keep-last, copy destination) and a service's main destination can be changed, without losing run history or the verification record.

 - Dropping a part from a destination offers to remove that destination's old snapshots of the part, shows the space it would free, and then forgets and prunes exactly those snapshot ids. Ballast still deletes only what it created (§19 ownership rule).

 - A read-only "remote machine files" source reads chosen files from a machine over SSH with no install on it, using an allow-list in which secrets (for example a VPN node key) are opt-in. The design is in §20.2.1.



 ### 20.2.1 Remote machine files (design for step 57)

 - **What it is.** A catalog strategy `remote-files` (built-in entry `remote-files`) that copies chosen files from another machine, for example a Proxmox host's `/etc/fstab`, `/etc/exports` and `/etc/nfs.conf`, into one tar artifact that is uploaded and verified like any other. It is read-only and installs nothing on the target: Ballast runs `ssh <machine> tar -cf - ...` and nothing else (plus `ls -d` to check the paths exist). The target needs only `sshd` and `tar`.

 - **Which machine.** The service's Docker host (§17.2) of kind remote-ssh names the machine, its address and its SSH key; the host does not have to run Docker, because only SSH is used. SSH host keys are trusted on first use in Ballast's own known-hosts file, as for SFTP destinations. A local host is refused with a message to register the machine first.

 - **Allow-list.** Every path must be absolute, clean (no `..`) and inside an allowed root: `/etc`, `/usr/local`, `/opt`, `/srv`, `/var/spool/cron`. Anything else is refused before connecting, with the reason in plain English. `/etc/pve` is always refused (it is covered by the Proxmox settings snapshot, §17.3).

 - **Secrets are opt-in.** Some files are secrets and are left out unless the service names their class in `secrets`: `tailscale-node-key` (`/var/lib/tailscale`, which sits outside the roots and so also needs the opt-in to be asked for), `ssh-host-keys` (`/etc/ssh/ssh_host_*`) and `password-hashes` (`/etc/shadow`, `/etc/gshadow` and their backups). When a requested folder (for example `/etc`) holds a secret that was not opted in, tar skips it, and the run log names what was skipped. An unknown class name is a configuration error. Secret contents are never shown in the UI, API or logs (rule 2); they are protected by the repository's encryption.

 - **Verification.** The archive-integrity check (every header read to the end) plus restic's read-back. A missing path fails the run before anything is uploaded; a file that changes while it is read is not tolerated silently (tar's non-zero exit fails the run).

 - **Restore.** The archive is an ordinary tar with the original absolute paths minus the leading `/`; restore extracts to a folder the operator chooses. Ballast never writes back to the remote machine.

 ### 20.3 Safety nets and guidance

 - `doctor` and the preview add up what services will put on each destination disk (last run size × retention) and warn above about 80% projected use, before a vzdump starts rather than after.

 - `doctor` checks that the Proxmox token can list guests and read a storage, and names the missing role or path. The Proxmox guide states the role needed and that a privilege-separated token needs its own permissions.

 - The "replace a script" guide warns that an old job backing up the disk Ballast writes to must exclude Ballast's folders before Ballast is turned on.



 ### 20.4 Live run status

 - One run-status record (phase, elapsed time, upload bytes and percentage per destination, lock holder) is kept in memory and read by both the CLI and the web UI. `ballast status` shows the phase and elapsed time; `ballast backup` says the daemon runs the job and how to follow it; the service card shows a progress bar with bytes first and time left second.

 - Only restic uploads have byte counts; other phases show phase and elapsed time. Progress is not persisted. The payload carries numbers and destination names only (rule 2).



 ### 20.5 Mobile-first web UI

 - The dashboard, services, destinations, schedule and restore screens work on a phone first and scale up to a desktop: touch-sized controls, tables as stacked cards on narrow screens, dark and light themes, plain-English wording. It adds no backup features and changes no API.

 - **Clarification of §2 and §12:** phone *backup* is Phase 2; a mobile-friendly web UI is in scope. This clarification lands before any UI redesign work.



 ### 20.6 Out of scope (v0.5)

 Phone backup (§12); importing old script archives; writing Proxmox config; rclone providers; multi-user auth/RBAC, OIDC, TLS/ACME; a push stream (server-sent events) for progress.



 ### 20.7 Acceptance checklist (v0.5 "done")

 - [ ] A long upload to one destination never delays verification of other services' finished backups, shown by a test with a held shared lock, a pending retention and a new read-back

 - [ ] `service list`, schedule creation and Proxmox job creation behave as in §20.1 on a host with Proxmox guest jobs; nothing runs on creation

 - [ ] A Proxmox job with a copy target and no copy snapshot is not green; keep-last 2 with a copy leaves at most 2 copy snapshots after three runs

 - [ ] Jobs and the main destination can be edited with history kept; dropping a part frees its space after a previewed cleanup

 - [ ] `doctor` and the preview warn about projected disk use and about a Proxmox token that cannot see guests

 - [ ] `ballast status` and the web UI show phase, elapsed time and upload progress of a running job

 - [ ] The database restore drills no longer flip between red and green on an unchanged backup (repeated-run test)

 - [ ] The web UI is usable at 375, 768 and 1280 px (screenshots before and after)

 - [ ] Secrets never in DB, logs or API responses — re-asserted for the progress payload and the remote-files allow-list



 ## 21. v0.6 extension: findings from running v0.5 in production (normative)



 **Goal:** fix what the first week of v0.5 on a production homelab (deployed 2026-10-10) turned up: alerts that may not repeat and cannot be audited, CLI output that cannot be scripted, repository health and off-site restores the CLI cannot reach, a self-backup that must be run by hand and leaves out its scripts, and an unwritten upgrade procedure. Each requirement was hit in practice or traced to code; the findings are tagged J1–J16. No mobile *backup* code (rule 6); the `exec` strategy and `ballast trigger --token` interfaces are untouched.



 ### 21.1 Alerts

 - A job that keeps failing gets one reminder per scheduled failure. The alert cool-down never exceeds 90% of the service's schedule gap (24 hours when the service has no schedule), so a daily job failing at 05:00:15 and again at 05:00:10 still reminds.

 - Every alert decision is logged in one line (sent, held back with the reason, or failed with the reason) and the last few decisions per service are kept and shown in the service detail view. A decision record holds the time, type, outcome and reason, never a topic, URL or token (rule 2).



 ### 21.2 Scripting the CLI

 - Command results go to stdout. Notices, warnings and errors go to stderr. Each list and show command is tested with the two streams captured separately.

 - `ballast status <service>` takes the service positionally (`--service` stays as an alias). A command that takes no positional arguments rejects extras and says what to use instead; it never drops them silently.

 - The "using credential directory" notice is shown only with `--verbose`, in `doctor`, or when the directory is not the default.

 - `ballast status --check` exits 0 when every light is green, 1 for amber or stale, 2 for red and 3 when status cannot be read, and prints one summary line. Plain `ballast status` keeps exiting 0.



 ### 21.3 Visibility and restore

 - `destination list` and `ballast status` show each destination's repository health light and when it was last checked and last OK; a never-checked destination says so. `ballast destination health [--json]` is read-only and uses the data behind `GET /api/v1/destinations/health`.

 - `ballast restore` and `ballast restore-project` accept `--from <destination>` (API parameter and web UI picker too). The destination must be one of the service's configured targets, the snapshot is resolved inside that repository with the same service-tag guard, and the default remains the main destination. This is how an off-site copy is proven restorable without changing the service's main destination.



 ### 21.4 Self-protection

 - Self-backup (§14 step 28) can be scheduled (default: daily after the nightly batch, to every destination, opt-out), has its own light and freshness alert, and applies retention scoped to the `ballast-self` tag only after a verified self-backup. Creating its schedule never runs it by surprise (§20.1). Its failure never masks another service's result (rule 3).

 - Self-backup also saves every local script that an `exec` strategy or an `exec-script` verifier points to, under `scripts/`, and lists them. `doctor` warns when a referenced script is missing or covered by no backup. Credential files stay excluded (rule 2); scripts must not contain secrets and the docs say so.



 ### 21.5 Releases and upgrades

 - A build reports its version from `git describe --tags --always --dirty`, never `dev` for a source build. The version is shown by `ballast version`, `doctor`, the API and the web UI footer. Tagging a release is the maintainer's action and is described in `docs/upgrading.md`.

 - `ballast wait-idle [--timeout]` exits 0 when nothing is running and non-zero on timeout; `ballast status --running` prints and exits with whether anything is running. `docs/upgrading.md` gives the manual upgrade and rollback procedure. Every migration states whether it can be rolled back and `doctor` shows the schema version. An interrupted run is amber, not lost (rule 1), but an upgrade should still wait for idle.



 ### 21.6 System install

 - `ballast install` also puts `ballast` on `PATH`. When `/etc/ballast` exists and the caller is root, or the daemon is reachable, the CLI uses the system config and data paths with no environment variables. A non-root caller is told to use `sudo`. Commands go through the daemon's API where it is running.



 ### 21.7 Monitoring Ballast itself

 - An optional credential file `ballast-heartbeat-url` makes the daemon request that URL on an interval (default 5 minutes) while its scheduler loop is alive, so something other than Ballast notices a dead daemon or a powered-off machine. The URL is a secret (root-only file, stripped from errors and logs). A failed ping is logged and never changes a backup's outcome. `doctor` reports whether a heartbeat is configured and when it last succeeded.

 - `ballast support-bundle [--output file]` writes one archive for bug reports: version, `doctor --json`, status, recent alert decisions, schema version, restic and docker versions and catalog entry names. Every secret is absent, asserted by the redaction harness with seeded secrets. Nothing is sent anywhere.



 ### 21.8 Out of scope (v0.6)

 Phone backup (§12); `ballast install --upgrade` (until the manual guide has been used); rclone providers; multi-user auth/RBAC, OIDC, TLS/ACME; a push stream for progress; changing what plain `ballast status` exits with.



 ### 21.9 Acceptance checklist (v0.6 "done")

 - [ ] A daily job failing at 05:00:15 then 05:00:10 reminds both days (fake-clock test), and every sent, held and failed alert has one log line and one stored decision

 - [ ] Every list/show command writes results to stdout and notices to stderr; `status <service>` shows only that service; `for s in $(ballast schedule list 2>/dev/null | cut -f1)` works

 - [ ] `ballast status --check` returns 0/1/2/3 for green, amber, red and unreadable status

 - [ ] `destination list`, `status` and `destination health` show the last repository check; a restore from the off-site destination works with `--from` and a non-target destination is refused

 - [ ] A scheduled self-backup reaches verified on every destination, old self-backups are pruned, and its scripts come back from a restore

 - [ ] A source build reports a real version in the CLI, `doctor`, API and UI; `ballast wait-idle` blocks during a run and `docs/upgrading.md` has been followed once on a production homelab

 - [ ] The CLI works as root without environment variables after `ballast install`

 - [ ] A stopped daemon stops the heartbeat; the support bundle contains no seeded secret

 - [ ] Secrets never in DB, logs or API responses — re-asserted for alert decisions, the heartbeat URL and the support bundle


 ## 22. v0.7 extension: findings from a QA and UX review of v0.6 (normative)



 **Goal:** fix what a browser QA and UX pass of v0.6 on a seeded instance (60 "Any folder" services, two destinations, about 3,200 runs over 90 days) turned up: no run history and no way to cancel, a server that accepts bad paths, a slow status route, a schedule save that undoes a pause, and a set of accessibility and wording problems. The findings are tagged K1–K10. No mobile *backup* code (rule 6); the `exec` strategy and `ballast trigger --token` interfaces are untouched.



 ### 22.1 Already fixed (context, step A, commit 833984b)

 - `POST /services` no longer silently replaces an existing service: a duplicate name is a 409.

 - "Still in use" errors for destinations and Docker hosts name the count and say what to do; the self-backup banner explains why it matters and gives the exact command.

 - Wizard and form errors scroll into view; relative paths are caught on the wizard step; a styled confirm dialog replaces the native one.

 - Overview and Backups rows link to the backup, with a status light, counts, grouping and a folded Safe group at 12 or more; the restore picker has a filter; schedules read "Weekdays at 4:15"; a running backup shows its step.

 - These are not redone by steps 79 to 87.



 ### 22.2 Server validation

 - The service layer trims a folder path and requires it to be absolute, for the CLI and API as well as the web UI. A missing path is saved with a warning, not refused, because the folder may be on a mount that is not up yet (K3).
 - Which parameters count as folder paths is data, not code: a catalog parameter with `kind: path` is checked (the only `kind` value), so new entries opt in without Go changes.



 ### 22.3 Performance

 - `GET /api/v1/status` answers in under 200 ms with 60 services and 3,200 runs. The cause is unknown, so the work starts with a benchmark and a profile, and the benchmark stays so it cannot regress (K5).



 ### 22.4 Schedules

 - Saving a schedule keeps its paused state unless the user turns it on. The API returns `next_run` and the page confirms the save with the next run time (K7).



 ### 22.5 Run history

 - A service's past runs can be listed (paged, newest first) and one run opened, with state, duration, size, verification result and log tail, in the API and in a History tab. Secrets never appear in a log tail or any field (rule 2) (K1).



 ### 22.6 Cancelling a backup

 - A running backup can be cancelled from the API, the CLI and the UI. The run ends CANCELLED and never SUCCESS (rule 1), the repository is left unlocked and any stopped container is restarted (K2).



 ### 22.7 Behaviour and wording

 - Removing a service during a backup says it is waiting for the backup to finish, and the refusal is tested (K6). Restore errors read in plain English without internal prefixes, and restoring "into a path" is relative to the backup root (K4). A service past half its expected interval shows its age in amber text; the light itself does not change (K9).



 ### 22.8 Accessibility and tall pages

 - A skip-to-content link; touch targets of about 44 px; a beginner-friendly wizard (the restic version warning behind "Details"); "Stop backing up" renamed "Remove" and saying what is kept; Notifications and Schedules searchable, grouped and folded like Backups (K10).



 ### 22.9 Sessions

 - Whether a server restart should sign the browser out is a decision for the maintainer (K8). If it should not, session hashes (never raw tokens) are kept in SQLite.



 ### 22.10 Out of scope (v0.7)

 Phone backup (§12); rclone providers; multi-user auth/RBAC, OIDC, TLS/ACME; `ballast install --upgrade`; a push stream for progress; the review's untested areas (the LIVE instance, a real phone, the 1440 px layout).



 ### 22.11 Acceptance checklist (v0.7 "done")

 - [ ] The CLI and API refuse a relative folder path and trim a trailing space; a missing absolute path is saved with a warning

 - [ ] `GET /api/v1/status` stays under 200 ms with 60 services and 3,200 runs (benchmark in the tests)

 - [ ] Saving a paused schedule leaves it paused and shows the next run time

 - [ ] A failed run's verification detail and log tail can be opened from a History tab, and no seeded secret appears in them

 - [ ] A cancelled backup ends CANCELLED, never SUCCESS, with the repository unlocked and any stopped container restarted

 - [ ] Removing a service during a backup explains the wait; restore errors are plain English; a weekly service shows an amber age cue while its light stays green

 - [ ] Skip link, 44 px touch targets, "Remove" wording and folded Notifications and Schedules pages are in place

 - [ ] The maintainer has decided whether sessions survive a restart, and the decision is recorded in the pull request or spec change that implements it

 - [ ] Secrets never in DB, logs or API responses; re-asserted for run logs and sessions

# Ballast — Project Specification (v1.0)



 > **One-line pitch:** A self-hosted, service-aware backup tool for Docker-based homelabs: it knows how to correctly back up the apps homelabers actually run (Immich, Vaultwarden, Postgres, …), *proves every backup restores*, and ships them to any restic-compatible destination (B2, S3, SFTP, local disk), with rclone as a secondary tier planned for Phase 2 (§12).



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

 - **No phone backup in v0.1** (deferred to Phase 2 — but the plugin interface must support it; see §4.2 and §12).

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



 - **Phone backup:** iOS via `pymobiledevice3 backup2 backup` (wrapped through the `exec` strategy, encryption password as a credential file), Android via adb/seedvault-style local push. USB-attach trigger: udev rule → systemd unit → `ballast trigger <job> --token`.

 - **v0.1 MUST ship, and test with a stub script:** (a) the `exec` strategy end-to-end, (b) the `trigger` command with token auth. No mobile code ships in v0.1.

 - **Secondary rclone tier** (moved out of v0.1): rclone remotes as a report-only tier that never blocks and never marks a job failed on its own (§5.2), with per-tier notification isolation tests (§11) and an acceptance check that a secondary-tier outage produces a report but never a primary job failure or masked notification.



 - Proton Drive as a *verified* tier, on top of the secondary tier (once a reliable headless path exists).



 ---



 ## 13. Testing requirements



 **Every feature ships with tests.** Coverage target: ≥90% on engine, catalog, strategies, and verification packages. CI (GitHub Actions): lint (`golangci-lint`), vet, unit tests, integration tests with Docker (testcontainers-go or docker-in-docker), cross-build (linux/amd64, linux/arm64).



 | Layer | Approach |

 |---|---|

 | Unit | Engine logic, state machine, freshness rules, notification formatting, restic output parsing (golden fixtures from real restic output) |

 | Catalog | Every bundled catalog entry validated against schema; parameter templating round-trips |

 | Integration (Docker) | Real Postgres container: dump → throwaway restore → assert; stop-start strategy against a real container; cleanup-on-failure (kill mid-job, assert container restarted) |

 | restic | Test repo on local disk  MinIO (S3-compatible) for backend coverage; snapshot read-back assertions |

 | E2E | **No E2E test may pass without a job reaching `VERIFYING → SUCCESS`.** Wizard flow test: fresh state → configured Immich-ish service (fixture container) → verified backup < 15 min (relaxed in CI, strict locally) |

 | Security | Test that secrets never appear in DB, logs, or API responses; credential-dir resolution under simulated systemd env |

 | API | Golden-file REST contract tests (request/response pairs) so GUI and CLI stay in sync |



 ## 14. Implementation order (for the implementing agent)



 Each step lands with tests green and `ballast --help` working. Do not start a step until the previous one is complete. `PLAN.md` tracks progress against this list.



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


 **v0.1 is complete as of step 13. Steps 14+ below are the v0.2 extension (§17: multi-host Docker, Proxmox, remote destinations) — design rationale and full detail in `PLAN-v0.2.md`; this list is the normative, session-by-session order `/implement` follows.**


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


 The secondary rclone tier is not part of v0.1 or v0.2; see §12.



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



 **Terminology note:** §12 above calls the mobile/phone work "Phase 2." This v0.2 extension (multi-host Docker + Proxmox) is a *separate*, earlier release that ships before mobile; it does not change §12's scope or timeline, and mobile remains phone-code-free until its own phase regardless of what v0.2 number it ends up carrying. Full design rationale, suggested-additions list, and launch sequencing live in `PLAN-v0.2.md`; this section is the normative subset `/implement` and future spec changes must agree with.



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

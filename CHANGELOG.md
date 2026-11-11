# Changelog

Tide is a production-grade GitOps controller for Docker Compose hosts paired with the `tidectl` CLI. The project evolved from an early ConOps prototype (Anurag Gupta, `anuragxxd/conops`), retaining only the core concept of polling Git and reconciling `docker compose` stacks while adding and rewriting over 26,000 lines of code across a dozen subsystems for multi-tenant, security-hardened production operations.

---

## How Tide Differs from Original ConOps (Anurag Gupta)

### 1. Control Plane UX & CLI (`tidectl` Replacing Web UI and `conops-ctl`)
- **Removed Web UI and static marketing site**: the embedded HTML dashboard and site were stripped out to minimize attack surface; all administration runs through the authenticated HTTPS API and CLI.
- **Full-featured GitOps CLI `tidectl`** (replacing the basic `conops-ctl`):
  - kubeconfig-style context management (`tidectl config init / view / get-contexts / use-context / set-context`) with automatic host CA discovery;
  - offline validation of host manifests without a running controller or Docker daemon (`tidectl validate --fleet-file ... --with-compose`);
  - asynchronous `reconcile` backed by server-side `operations`, exit codes, and `--wait`;
  - operational commands: `tidectl retry` (requeue a single application without restarting the controller), `suspend` / `resume` (persistent operator pause surviving restarts and fleet syncs), `wait` (assert synced commit, image digest, and public variables), `history`, `audit`, `gc`, `migrate-repo`, `check`, `logs`, `export`, and aligned `table`/`wide`/`json` output.

### 2. Declarative Fleet GitOps (`internal/fleet`, `internal/fleetcheck`)
- Original ConOps required manual per-app registration over HTTP/UI. In Tide, a **fleet Git repository** (`hosts/<fqdn>.yaml`) is the single source of truth that registers, updates, and decommissions host applications automatically.
- Strict YAML schema validation and unknown-field detection (`TIDE_FLEET_STRICT=warn|enforce`) prevent silent policy typos.
- Host-level and `tenants`-level policy inheritance down to individual applications (`vars`, `public_vars`, `env_files`, `build`, `require_digest`, `allowed_registries`, `reconcile_interval`, `retry_rejected`, `priority`, `require_pipeline_sources`, `strict_pipeline_gate`, `shared_required_jobs`, `trusted_web_pipelines`).
- Seamless adoption of pre-existing Compose stacks on the host via explicit Compose project naming (`project`).

### 3. Multi-Tenant GitLab CI Gate (`internal/cigate`)
- Original ConOps had no CI verification at all — every new commit was deployed unconditionally.
- **Per-tenant repository isolation**: CI gate checks evaluate only the target application's own `repo_url`, exact commit SHA, and expected Git `ref` (branch or tag). A failing pipeline in one tenant never blocks other tenants on the same host.
- **Required job sets for shared monorepos (`required_jobs`, `shared_required_jobs`, `tenants`)**: isolates tenants sharing a repository by checking shared checks plus tenant-specific jobs, including **child pipelines** discovered via `/bridges` with parent linkage, exact `sha`, and `ref` verification. Missing, `skipped`, or unfinished required jobs never count as green.
- **Automatic unblock on job retry**: Tide evaluates the latest attempt (highest job `id`) of each required job on the push pipeline, transitioning `failed` → `waiting` → `allowed` and deploying the same SHA automatically without a new commit or controller restart.
- **Pipeline source rules & strict mode**: only `push` and `merge_request_event` decide deployments by default; failed `trigger`, `api`, or manual `web` pipelines never block a green `push`. In strict mode (`strict_pipeline_gate` / `TIDE_STRICT_CI_GATE`), absence of a trusted pipeline holds deployment in `waiting` instead of allowing `absent`.
- **Opt-in trusted manual pipelines (`trusted_web_pipelines`)**: disabled by default; when enabled, enforces allowlisted users, exact SHA and ref match, allowlisted pipeline variables (rejecting check-skipping flags such as `SKIP_*` / `NO_TEST*`), and the full required job set. If the API token lacks Maintainer permissions to read `/variables` (`read_api`-only token), the policy safely stays inactive.
- **Hardened emergency bypass (`tidectl reconcile --force`)**: single-use and short-TTL, bound to one application and exact `--sha`, requires a mandatory `--reason` and authorized token (`TIDE_FORCE_TOKENS`), automatically revokes if the commit SHA changes, never bypasses Compose security/digest/secret checks, and writes immutable audit records.

### 4. Priority Apply Queue, Periodic Reconciler & Self-Healing (`internal/controller`)
- Priority work queue with trigger classes (`manual`/`rollback` → `commit`/`fleet` → `drift` → `retry`), per-app `priority`, and automatic class promotion via `queue_aging`.
- Separate concurrency semaphores for stack applies (`TIDE_RECONCILE_CONCURRENCY`) and image pulls (`TIDE_PULL_PARALLEL`) with stream-activity idle timeouts (`pull_timeout`) so waiting for a pull slot never triggers a false timeout.
- Atomic `desired_generation` and `FollowUp` tracking ensure commits or `vars` updates arriving mid-sync are always queued for another pass and never lost.
- No-op queue dropping when desired state already matches the last applied revision, duplicate-enqueue collapsing during retry backoff, and automatic stuck-queue recovery past `TIDE_QUEUE_WARN_AFTER`.

### 5. Container Health Verification, Automatic Rollback & One-Shot Jobs (`internal/compose`, `internal/controller`)
- **Direct Docker Engine API inspection**: reads container states, `RestartCount`, crash-loop flapping, exit codes, and local image digests directly from the Docker Engine socket without noisy CLI polling.
- **Healthcheck gate & automatic rollback**: after `compose up`, Tide waits for all service containers to pass healthchecks within `health_timeout` (with optional early slot release via `TIDE_SLOT_HEALTH_HOLD`). Unhealthy, flapping, or timed-out rollouts automatically roll back to `LastHealthyCommit` and `LastHealthyVars`, recording `RejectedCommit` / `RejectedVars` to prevent crash-looping on broken revisions.
- **First-class One-Shot & DB migration phase**: services depended on via `service_completed_successfully` or labeled `com.tide.oneshot=rerun` execute in a dedicated pre-stack phase with bounded `oneshot_timeout` and exit-code diagnostics.
- **Registry failure classification**: missing manifests enter a permanent `image_not_found` hold until desired state changes (preventing endless 5-minute pull loops), whereas transient network/registry failures use exponential backoff.

### 6. Compose Security Policies & Host Guardrails (`internal/fleetcheck`, `internal/compose`)
- Mandatory `@sha256` image digest pinning enforcement (`require_digest`).
- Registry allowlists (`allowed_registries`) and host-side build prohibition (`build: forbidden` / `TIDE_BUILD=forbidden`).
- Service security policy engine (`TIDE_POLICY_MODE=off|warn|enforce`) inspecting `privileged`, `network_mode: host`, `pid: host`, `ipc: host`, `cap_add`, `devices`, `security_opt`, and `container_name`, with auditable per-service `policy_exceptions`.
- Host bind-mount prefix allowlists (`TIDE_ALLOWED_BIND_PREFIXES`) and cross-project published port conflict detection prior to `compose up`.
- Controlled creation of missing external Docker volumes (`create_volumes` + `volume_name_pattern` regex) labeled with `com.tide.managed=true`.

### 7. Zero-Recreate Config Reloads (`watch_files`, `inplace_linux`)
- On Linux, Git worktree updates materialize **in-place while preserving file inodes** so containers with single-file bind mounts observe updated config bytes immediately.
- Per-entry file, directory, and glob hashing for `watch_files`: config changes trigger HTTP reload hooks (`reload_url`), in-container commands (`reload_command`, e.g. `kill -HUP 1`), or targeted single-service restarts instead of recreating containers.
- Content hashing for external host `env_files` and Compose `env_file` references with automatic drift reconciliation.

### 8. Zero-Downtime Application Source Migration (`tidectl migrate-repo` & Fleet)
- Original ConOps had no repository migration workflow. Tide performs in-place source switches (`repo_url`, `branch`, `compose_path`, `revision`, or `repo_auth_method`) while preserving `app.ID`, `ComposeProject`, `env_file`, and all data volumes:
  - stages and validates the target source before touching the live stack: checks Git readability, green CI gate for the target commit, valid rendered Compose config, external `env_file` availability, absence of persistent data or writable bind mounts inside the managed app directory, and availability of all pinned images;
  - compares the rendered Compose configuration (image with `@sha256` digest, environment variables, published ports, volumes/mounts, networks, command/entrypoint, and user labels) against the currently running containers;
  - **zero-restart adoption (`mode=no_restart`, `reason=config_identical`)**: when the rendered configuration matches the live stack, Tide swaps the Git checkout metadata in-place (preserving file inodes), records the new source and commit, and enters regular reconciliation without stopping or recreating any containers;
  - **targeted service recreation (`mode=restarted`)**: when configuration differs, Tide runs standard `docker compose up -d --remove-orphans` (never `compose down`) so only changed services are recreated while unchanged services remain running;
  - aborts immediately on any preflight error (unreachable source, red/pending CI gate, invalid Compose, missing `env_file` or image), keeping the old source and leaving the running application untouched;
  - emits structured migration logs, persistent `audit_events`, and `tide_repo_migrations_total{app,mode,reason}` Prometheus metrics; supports `--dry-run` impact plans and symmetric rollback.

### 9. Security, Encryption & Secret Redaction (`internal/apiauth`, `internal/redact`, `internal/secrets`, `internal/tlsutil`)
- **HTTPS by default** (`:8443`) with automatic self-signed certificate generation or custom `TIDE_TLS_CERT` / `TIDE_TLS_KEY`.
- **API authentication & authorization**: mandatory Bearer token auth (on-disk SHA-256 hash + generated `bootstrap_token`), named operator tokens (`TIDE_OPERATOR_TOKENS`) for audit attribution, and dedicated `TIDE_FORCE_TOKENS` for CI bypass privileges.
- **HashiCorp Vault integration** (`vault://path#key`) and external host `env_files`.
- **Credential-safe Git & Registry execution**: HTTPS Git tokens are injected via transient `GIT_CONFIG` `http.<host>.extraheader` environment variables (never written to `.git/config`), and `docker login` pipes passwords via `stdin` into an ephemeral per-sync `DOCKER_CONFIG` directory removed immediately after pull.
- **Log secret redaction**: `redact` wraps `slog` to scrub secret env values, URL userinfo, and GitLab token prefixes (`glpat-*`, `gldt-*`, `glrt-*`); rendered `docker compose config` output is never logged at any level, and missing local image checks (`docker image inspect`) log at `DEBUG`.

### 10. Git Transport & Filesystem Permission Hardening (`internal/gitremote`, `internal/gitrev`, `internal/repoauth`, `internal/fsperm`)
- Tracks branches, tags, and full 40-char commit SHA pins (`revision`), polling via lightweight `ls-remote` with jitter before fetching.
- Full support for private GitLab and custom SSH servers (`deploy_key`, `token`, `public`, managed `known_hosts`, and `ssh-keyscan`).
- Automatic recovery from corrupted Git caches and dirty worktrees, with split filesystem permissions (`0700`/`0600` for `.git`, `.ssh`, keys, and DB; `0755`/`0644` for checkout files read by unprivileged container UIDs).

### 11. Process Guardrails, Early CLI Parsing & Image Garbage Collection (`internal/procguard`, `internal/imagegc`)
- **Inter-process environment lock (`tide.lock`)**: non-blocking `flock` acquired before file migration, database open, or `syncing`-state recovery — a second controller instance exits immediately with a clear error without mutating state.
- **Early CLI flag handling**: `--help`, `--version`, and unknown flags in both `tide` and `tidectl` exit before touching files, databases, network, or Docker.
- **Image GC & volume protection**: scheduled and on-demand (`tidectl gc [--dry-run]`) pruning of unused Tide-managed images while protecting images belonging to the last `N` rollback revisions, supporting disk-usage thresholds (`TIDE_IMAGE_GC_DISK_PCT`) and `com.tide.keep=true` volume protection.
- **Self-contained controller image**: bundles Docker 27 CLI and Compose plugin (API ≥ 1.44) at build time without runtime downloads, plus SQLite `SQLITE_BUSY` retry handling.

### 12. Observability, Deploy History, Persistent Audit & Multi-Host (`internal/metrics`, `internal/store`, `internal/agent`)
- Dedicated metrics and health listener (`127.0.0.1:8081`, `/metrics` and `/healthz`) with optional `TIDE_METRICS_TOKEN` bearer protection.
- Over 35 Prometheus metrics under `tide_*` (app states, commit/image/env drift, sync durations, structured `syncerr` code/stage counters, CI gate blocks by pipeline source, per-app queue wait gauges, rollbacks, reloads, Git fetch errors, GC) with strict label hygiene — no commit SHAs, pipeline IDs, or user IDs in metric labels.
- Persistent revision history (`app_revisions`), deploy events (`deploy_events`), and append-only security/migration audit log (`audit_events`).
- Optional Git push webhooks (`POST /api/v1/hooks/git` for GitLab/GitHub) and `TIDE_MODE=standalone|agent|hub` heartbeat aggregation across hosts.

---

## Release History

### v2.1.1 (2026-11-11)
- **Fixed false `restarted` plans in `tidectl migrate-repo` when the configuration is unchanged**:
  - **`$` in values**: `docker compose config` prints every `$` as `$$`. Tide compared that escaped text with the values Docker Engine reports for the running containers, so an environment value, command, entrypoint, or label containing `$` was reported as `env_changed`, `command_changed`, or `labels_changed`. The rendered configuration is now unescaped before the comparison. Container values and image defaults are compared as they are and nothing is interpolated a second time; a literal `$$` in a container matches `$$$$` in the rendered configuration.
  - **Images referenced by tag**: for an image without `@sha256` the digest read from local `image inspect` was lost between two comparison steps, so the service was reported as `image_changed` (`no resolved digest`) while the container was running that very image. The digest (first RepoDigest, or the image ID when the image has none) now reaches the comparison and is derived the same way for the desired and the running side. A tag that points to another local image, a changed tag, and an image that is not available locally are still reported as `image_changed`.
- Added regression tests for `$` and literal `$$` values, image defaults, tag-only images, and a dry run that must not start, stop, pull, or create anything and must not print environment values in the plan or the log.

### v2.1.0 (2026-11-11)
- **Zero-downtime application source migration (`repo_url`, `branch`, `compose_path`)**:
  - When moving an application to a new repository, branch, or `compose_path` (in Fleet YAML, API `PATCH` / `migrate-repo`, or `tidectl migrate-repo`), Tide fetches the target repository in staging, verifies the CI gate for the target commit, renders `docker compose config`, and compares the normalized specification (image + `@sha256` digest, environment variables, ports, volumes/bind mounts, networks, command/entrypoint, and labels) against the currently running containers.
  - **No-restart source adoption (`mode=no_restart`, `reason=config_identical`)**: If the rendered configuration matches the live stack, Tide swaps the checkout metadata in-place (preserving regular file inodes for bind mounts), updates the stored source and commit, and resumes normal reconciliation without stopping or recreating any containers.
  - **Targeted service recreation (`mode=restarted`)**: If the configuration differs (e.g., `image_changed`, `env_changed`, `ports_changed`, `volumes_changed`, `networks_changed`, `command_changed`, `labels_changed`), Tide swaps the checkout in-place and runs standard `docker compose up -d --remove-orphans` without `compose down`, recreating only the changed services while unchanged services continue running uninterrupted.
  - **Preserved identity & safe failure**: Preserves `app.ID`, `ComposeProject`, named/external volumes, and `env_file` references. If the new source is unreachable, CI gate is not green, `env_file` is missing, or Compose fails to render/validate, Tide stays on the previous source and leaves the running application untouched.
  - **Observability**: Added `tide_repo_migrations_total{app,mode,reason}` Prometheus counter, structured logs (`mode`, `strategy`, `reason`, `changed_services`, `unchanged_services`), `migrated_no_restart` / `migrated_restarted` deploy history & audit decisions, and migration diff fields in `tidectl migrate-repo [--dry-run]`.

### v2.0.1 (2026-11-10)
- Consolidated repository branches to a single `main` branch and unified binary archive + GHCR container image publishing in the release workflow.
- Documented the full architectural and functional diff between Tide and the original ConOps prototype.

### v2.0.0 (2026-11-10)
- Renamed product from ConOps to **Tide** and CLI from `cps` to **`tidectl`** (`github.com/valthaide/tide`, `TIDE_*` / `TIDECTL_*` env vars, `tide_*` metrics, `/opt/tide` and `/etc/tide` paths, `com.tide.*` labels).
- Added one-release transitional compatibility layer (`internal/envcompat`) supporting `CONOPS_*`, `CPS_*`, the `cps` binary alias, and verified migration of `conops.db` / `conops-encryption.key` to `tide.db` / `tide-encryption.key` with deprecation warnings.
- Isolated CI gate checks per application repository, exact commit SHA, and ref; added `required_jobs`, `shared_required_jobs`, `tenants`, and parent-verified child pipeline bridge checks.
- Added automatic push-pipeline re-evaluation on job retry (`failed` → `waiting` → `allowed` using the latest attempt per job); `tidectl retry` triggers immediate re-evaluation without bypassing CI.
- Added opt-in `trusted_web_pipelines` policy (disabled by default) and `strict_pipeline_gate` / `TIDE_STRICT_CI_GATE` mode.
- Hardened `tidectl reconcile --force` with mandatory `--sha` and `--reason`, token authorization checks, short TTL, single-use consumption, automatic revocation on SHA change, and permanent `audit_events` storage (`tidectl audit`).
- Added CI gate status, reason, pipeline/job URL, source, and check timestamp to application status; stripped commit SHAs, pipeline IDs, and user identifiers from Prometheus metric labels.
- Added non-blocking `flock` startup guard (`tide.lock`) prior to opening the database, and early `--help`, `--version`, and unknown-flag handling.
- Added production-safe repository migration (`tidectl migrate-repo` and fleet `repo_url` updates) with preflight checks, volume/project preservation, `--dry-run`, rollback, and audit logging.

### v1.3.6 (2026-11-06)
- Fixed apply-queue race when an enqueue arrived as a sync finished; added stuck-queue self-recovery, `retry` endpoint/command, permanent `image_not_found` hold, `watch_files` `reload_command`, and removed `docker compose config` output from logs.

### v1.2.0 – v1.3.5
- Added periodic reconciler, priority apply queue with aging, direct Docker Engine API container inspection, controlled external volume creation, and CI gate pipeline source filtering.

### v1.0.0 – v1.1.2
- Removed Web UI, introduced Flux-style CLI, HTTPS + Bearer auth, Fleet GitOps, Vault integration, arbitrary SSH Git host support, `watch_files`, healthcheck rollback, one-shot services, Compose security policies, `require_digest`, `allowed_registries`, sync/pull concurrency limits, Image GC, and Prometheus metrics.

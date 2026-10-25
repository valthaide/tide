# Changelog

## v1.0.14

### Fixes
- **Image build without download.docker.com.** Docker CLI 27.5.1 (API 1.47) and Compose are copied from `docker:27-cli`, the image the platform runner already pulls. The git host does not need to reach download.docker.com.

## v1.0.13

### Features
- **Docker client API 1.44.** The controller image ships Docker CLI 29.9.2 and Compose 5.6.1. `DOCKER_API_VERSION` is not required. If the client still cannot speak the daemon's minimum API, sync status is `failed` with reason `docker_api_incompatible` and running containers stay up.
- **Dirty checkout.** A dirty worktree, including a file-mode-only difference, does not block sync. ConOps resets its own checkout to the requested revision.

## v1.0.12

### Features
- **Fleet `vars`.** `hosts/<fqdn>.yaml` may set `vars` on an app. ConOps passes them to `docker compose` so `${APP_TAG}` is substituted. A variable with no default that is unset fails validation (`unset_var: <name>`), status `failed`, and running containers stay up. Changing `vars` is a new revision and syncs even when the app commit did not move.
- **Ready images only.** `build: forbidden` on the app, on the host file, or via `CONOPS_BUILD` (default `allowed`). If `compose config` still has `build` for a service, sync does not start: status `failed`, reason `build_forbidden: <service>`, one ERROR, and the error metric. Running containers are not stopped.
- **Registry login for pull.** `REGISTRY_HOST`, `REGISTRY_USERNAME`, and `REGISTRY_PASSWORD` (puppet). The password goes to `docker login --password-stdin` and is not written in the clear or logged. Login failure is `failed` / `registry_auth` and does not touch running containers.
- **Pull, then up, no build.** For `build: forbidden`, `compose pull` runs first and `up -d` runs only after it succeeds, without `--build`. A missing image is `failed` / `pull_failed: <image>`; the previous containers keep running.
- **Rollback by tag.** A failed health check restores the images and `vars` of the last healthy revision from ConOps state, without a build. The status says which version was restored and why.
- **Independent app sync.** One slow or failed sync does not hold the others in `pending`. Parallelism is `CONOPS_SYNC_PARALLEL` (default 2; `CONOPS_MAX_CONCURRENT_SYNCS` still works when the new variable is unset).
- **SQLite lock after restart.** `database is locked` is retried for up to 5 seconds. A retry that then succeeds is logged at DEBUG.
- **Running images.** `cps get apps` shows the image and digest that are actually running. `conops_app_image_info{app,service,image,digest}` is 1 for the same set.

## v1.0.11

### Fixes
- **Health-check timeout logs.** Cancelling `docker ps` on the health deadline is not logged. A rollback keeps one WARN with the reason and one ERROR `App sync failed` that says the revision was rolled back.
- **CI wait log.** One INFO when waiting starts, repeats at DEBUG, one INFO when CI reaches `success` or `failed` with how long the wait was.
- **`Applied` log** after a healthy deploy: `app`, `revision`, `duration`. Status becomes `synced` with the new revision as soon as the health check passes.
- **Kept volumes.** Without `prune_volumes: true`, named volumes stay and are listed once in `Named volumes kept`.
- **Missing fleet host file.** After `CONOPS_FLEET_MISSING_CYCLES` (default 3) consecutive misses, one WARN, then DEBUG. `conops_fleet_host_file_missing` stays 1 until the file returns.

## v1.0.10

### Fixes
- **CI gate recheck.** A progress update after "pipeline still running" was putting the app back into `syncing`, so the next loop skipped it until the 5-minute stale recovery. The app stays `pending` and the gate is checked again on the next reconcile. If CI is already green by then, the deploy happens on that cycle. `conops_deploy_blocked{app,reason}` is a gauge: 1 while blocked, 0 after the block clears.
- **`cps config init` keeps TLS verification on** and records the discovered CA. It no longer sets `insecure-skip-tls-verify` for localhost.
- **`cps` inside the controller container** also trusts `/data/tls/cert.pem`.
- **`docker ps` logs.** `Executing command`, `Command output`, and `Command succeeded` for `docker ps` are DEBUG.

## v1.0.9

### Fixes
- **`watch_files` per path.** Each entry keeps its own hash. Changing `prometheus.yml` reloads only Prometheus; alertmanager and grafana stay up. `conops_app_reloads_total` increments by one.
- **Metrics of a removed app disappear** on delete, including `conops_env_drift`, `conops_last_sync_timestamp_seconds`, `conops_watch_files_hash_error`, and git series for a repo no other app uses.
- **Rollback reason.** The stored error and `conops_app_rolled_back{reason}` say `unhealthy` or `health_timeout`, with the container name and how long ConOps waited. A killed `docker ps` is no longer the message.
- **`conops_app_reloads_total{app,result}` is 0 at startup** for `success` and `error`.
- **`cps` trusts the local controller** via `CONOPS_CA_FILE`, `/etc/conops/ca.crt`, or `/opt/conops/data/tls/cert.pem`.
- **`docker ps` output is DEBUG**, not INFO.
- **Docs:** `reload_url` examples use `http://prometheus:9090/-/reload`, the compose service name, not `127.0.0.1` inside the ConOps container.

### Host CA for cps

Puppet should publish the self-signed server certificate (it is its own CA) as `/etc/conops/ca.crt` mode `0644`. Source file: `/opt/conops/data/tls/cert.pem`.

## v1.0.8

### Fixes
- **`watch_files` reload after sync.** A commit that changes a watched file now runs reload or restart in that same sync, writes it to the log, and increments `conops_app_reloads_total{app,result}`. Single-file bind mounts keep their inode across `git checkout`, so Prometheus sees `prometheus.yml` without a new container.
- **Metrics bearer token.** `CONOPS_METRICS_TOKEN` or `CONOPS_METRICS_TOKEN_FILE`. When set, `/metrics` without that bearer returns 401. Unset keeps the endpoint open. `/healthz` stays open.
- **Git error series from zero.** `conops_git_fetch_errors_total{repo,reason}` is created at 0 for `auth`, `network`, `not_found`, and `other` at startup for every known repo.
- **GitLab mirror.** A branch push fast-forwards only `refs/heads/main`. A tag push sends only `refs/tags/<tag>` and does not use `--force`. Both jobs share the `mirror-gitlab` concurrency group, so a tag cannot roll `main` back or rewrite an old tag.

## v1.0.7

### Features
- **Auto image build on a tag.** GitLab `build-image` runs on `CI_COMMIT_TAG` without a manual play and prints `Built version <tag>`. `main` stays manual.
- **`revision` in fleet `apps[]`.** Optional branch, tag, or full SHA. Empty keeps tracking `branch`. A moved tag is redeployed and logged. A SHA is deployed as-is and the branch is not polled. `conops_app_revision{app,ref,sha}` shows what is deployed. `suspend`/`resume` and health rollback apply to every kind.
- **Faster git sync.** App and fleet polls default to 30s and 60s, plus `CONOPS_POLL_JITTER` (default 15s). Change detection is `ls-remote`; a full fetch runs only when the commit changed. `conops_last_sync_timestamp_seconds{app}`.
- **Git access errors.** `conops_git_fetch_errors_total{repo,reason}` with `auth`, `network`, `not_found`, or `other`, and `conops_git_last_success_timestamp_seconds{repo}`. A failed check leaves the last successful revision running.

## v1.0.6

### Features
- **Remove app images on delete.** Dropping an app from fleet `apps[]` runs `docker compose down` and deletes images that no other container still uses. Named volumes stay unless the app has `prune_volumes: true` (the last value stored from fleet). Other apps' containers, images, and volumes are left alone.
- **`cps suspend <app>` / `cps resume <app>`** (and `POST /api/v1/apps/{id}/suspend|resume`). Automatic reconcile and git polling skip that app; the rest of the host keeps syncing. The pause is `operator_suspended` in the database, so a controller restart and a fleet reconcile do not clear it. `cps get apps` shows `SUSPENDED`, and `conops_app_suspended{app}` is 1. `resume` marks the app pending so the next reconcile matches git.
- **Rollback when a revision stays unhealthy.** After apply, ConOps waits `health_timeout` (fleet field, default 120s) for compose healthchecks. If the new revision does not become healthy and an older healthy revision exists, that revision is applied again. The failed revision is not retried until git moves to a different commit. Status shows the running (healthy) revision; `conops_app_last_sync_error` carries the failed revision's error; `conops_app_rolled_back{app,failed_revision,running_revision}` is 1. Git remains the desired state.
- **`conops_fleet_host_file_missing`** is 1 while `hosts/<CONOPS_FLEET_HOST>.yaml` is absent. The next successful reconcile sets it to 0. ConOps does not create the file.
- **Image version.** `build-image` (kaniko) and the controller Dockerfile pass the git tag into `main.Version` and `cps`. `GET /metrics` exposes `conops_build_info{version="v1.0.6"} 1`. `cps version` prints the same tag.

### Upgrade from v1.0.5

1. Build and deploy the `v1.0.6` image (GitLab job `build-image` on the tag) and restart ConOps.
2. Optional fleet fields: `prune_volumes: true` before removing an app whose named volumes should go; `health_timeout: 120s` per app.
3. `cps suspend <name>` pauses one app. `cps resume <name>` returns it to git.

## v1.0.5

### Fixes
- **`watch_files` directories and globs**: recursive tree hash for dirs; `*`/`?`/`[]` globs; escaping symlinks not followed. Clear error with `app` + `path`; metric `conops_watch_files_hash_error{app}`; log only when the error text changes (no spam every poll).
- **CI gate re-check**: `pending`/`running`/`created`/… → wait (status stays `pending`, recheck every reconcile loop); `failed`/`canceled` → block with `ci_failed` but **recheck** while `last_seen != last_synced` (pipeline retry → success deploys without a new commit); API/timeout → `ci_error`.
- Metrics: `conops_deploy_blocked{app,reason="ci_pending|ci_failed|ci_error"}`, `conops_ci_check_last_timestamp_seconds{app}`.
- **Dirty worktree**: refuse by default with `dirty_worktree` error + metric; `CONOPS_DIRTY_WORKTREE=reset` discards via hard reset. No silent re-clone on dirty tree.

### Notes
- Force/async reconcile after CI success still works (`POST .../reconcile` or `/sync`); CI jobs can call that when the pipeline finishes.
- Fleet `watch_files` paths like `prometheus/rules` and `grafana/dashboards/*.json` work without config schema changes.

### Upgrade from v1.0.4

1. Deploy **v1.0.5** and restart ConOps.
2. **Fleet YAML**: no required changes if paths were already directories — they start working. Prefer globs when you only care about a subset (`grafana/dashboards/*.json`).
3. Apps stuck after CI `pending` in 1.0.4: after upgrade they re-enter pending and deploy once the pipeline is `success` (or call `cps reconcile app <name> --wait`).
4. Optional: `CONOPS_DIRTY_WORKTREE=reset` if local edits in checkouts are expected; default `refuse` needs a clean tree.
5. Alerts: use `reason=~"ci_pending|ci_failed|ci_error"` on `conops_deploy_blocked`.

## v1.0.4

### Fixes
- **Checkout permissions**: stop applying umask 077 / full-tree 0700. App worktrees are `0755/0644` so containers running as `nobody` can read bind-mounted configs. Only `.git`, `.ssh`, `.envs`, data dir (`conops.db`, encryption key) stay private.
- On startup, **migrate** existing `runtime/<app>/repo` trees left locked by v1.0.3.
- **SOPS leftovers**: `CONOPS_SOPS` / age keys no longer abort startup; ConOps logs a **warning** and ignores them.

### Features
- **`watch_files`** in fleet app spec: on content change, `restart` a compose service or `reload` via HTTP (`reload_url`, e.g. Prometheus `/-/reload`). Hash stored; actions logged and counted in metrics.
- **`require_pipeline_success`**: before deploying a new commit, query GitLab pipelines API (`CONOPS_GITLAB_API_TOKEN` or `CONOPS_GIT_TOKEN`). Fail-closed on timeout/error → `blocked_by_ci` / metrics `conops_deploy_blocked`.
- Metrics: `conops_reconcile_last_success_timestamp`, `conops_deploy_blocked{app,reason}`, `conops_env_drift{app}`, `conops_build_info{version}`, `conops_watch_file_actions_total{app}`.
- CI: `go vet`, `go test -race` on push/PR; image build/push to GHCR on `v*` tags; release job waits for tests.

### Docs
- Path ownership vs Puppet: [docs/PATH_OWNERSHIP.md](docs/PATH_OWNERSHIP.md)
- Upgrade notes below.

### Upgrade from v1.0.3

1. **Deploy image/binary v1.0.4** and restart the ConOps container/service.
2. **Permissions**: no manual `chmod` required — startup runs `MigrateRuntimePermissions` on `CONOPS_RUNTIME_DIR`. Verify: `runuser -u nobody -- cat /opt/conops/runtime/<app>/repo/<config.yml>` (or your runtime path).
3. **Puppet**: stop managing mode/owner under ConOps runtime checkouts (see PATH_OWNERSHIP). Keep Puppet ownership of `/etc/conops`, `/etc/tenko/apps/*.env`.
4. **Unset** `CONOPS_SOPS` / `CONOPS_SOPS_AGE_KEY*` in compose/Puppet env to silence warnings.
5. **Optional fleet YAML** per app:
   ```yaml
   require_pipeline_success: true
   watch_files:
     - path: prometheus.yml
       action: reload
       reload_url: http://127.0.0.1:9090/-/reload
     - path: rules/alerts.yml
       action: restart
       service: prometheus
   ```
6. Ensure API token for CI gate has `read_api` (`CONOPS_GITLAB_API_TOKEN` preferred).
7. Protect GitHub tags `v*` in repo settings if not already.

## v1.0.3

Secrets hardening: clean git remotes (http.extraHeader), redact logs/API, fleet tokens not stored in DB, async reconcile API, env_file drift, drop SOPS binary, runtime lockdown (superseded in 1.0.4 for checkout readability).

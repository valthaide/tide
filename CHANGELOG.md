# Changelog

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

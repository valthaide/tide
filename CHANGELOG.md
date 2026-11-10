# Changelog

## v2.0.0 — Tide rename, multi-tenant CI gate, audited bypass, and safe repo migration

- Renamed product from ConOps to **Tide** and CLI from `cps` to **`tidectl`** (`github.com/valthaide/tide`, `TIDE_*` environment variables, `tide_*` Prometheus metrics, `/opt/tide` and `/etc/tide` paths, `com.tide.*` labels).
- Added one-release transitional compatibility aliases (`CONOPS_*`, `CPS_*`, `cps` binary alias, and automatic migration of `conops.db` / `conops-encryption.key` to `tide.db` / `tide-encryption.key` with deprecation warnings). Transitional aliases will be removed in the next release.
- Isolated CI gate checks per application repository, exact commit SHA, and ref; added optional `required_jobs`, `shared_required_jobs`, and parent-verified child pipeline bridge checks.
- Re-evaluated push pipelines automatically on job retry (`failed` → `waiting` → `allowed` using the latest attempt per job); `tidectl retry` triggers immediate re-evaluation without bypassing CI.
- Added opt-in `trusted_web_pipelines` policy (disabled by default) and `strict_pipeline_gate` mode so untrusted sources wait instead of deploying as `absent`.
- Hardened `tidectl reconcile --force` with mandatory `--sha` and `--reason`, authorization checks, short TTL, single-use consumption, automatic revocation on SHA change, and permanent `audit_events` storage.
- Exposed CI gate status, reason, pipeline/job URL, source, and last check timestamp on applications; removed commit SHAs, pipeline IDs, and user identifiers from Prometheus metric labels.
- Added early `--help`, `--version`, and unknown-flag handling before touching files, databases, or Docker, and non-blocking `flock` startup protection (`tide.lock`) before opening the database.
- Replaced destructive repo recreation with production-safe in-place repository migration (`tidectl migrate-repo` and fleet `repo_url` updates) featuring preflight git/CI/compose/image/bind checks, strict `compose down` error handling, volume preservation, `--dry-run`, rollback, and audit logging.

## v1.3.6

- Fixed apply-queue race where an enqueue arriving as a sync completed stayed in wait metrics without becoming runnable; added stuck-queue recovery, `retry` endpoint/command, `image_not_found` permanent hold, `watch_files` `reload_command`, and suppressed `docker compose config` secret output from logs.

# Tide

Tide is a single-host GitOps controller for Docker Compose stacks, paired with the `tidectl` CLI.

## Overview

- **Fleet-driven desired state**: One fleet repository declares the applications running on a host (`hosts/<fqdn>.yaml`), while each application tracks its own tenant or service Git repository (`repo_url`), branch/tag/SHA, and compose configuration.
- **Fleet template compose source (v2.2)**: An app may set `template: <name>` instead of `compose_path`. Tide then uses `templates/<template>/compose.yaml` from its fleet checkout as the base compose file and merges the app's optional `compose.override.yaml` (in the app folder of its own repository; silently skipped when absent) after it, with Tide's own files last: `docker compose --project-directory <app folder> -p <project> -f <template> [-f <override>] …`. Relative paths, `.env`, and `env_file` resolve from the app folder, and the compose project name never depends on the source. Setting both `template` and `compose_path` is a validation error. App commits and changes under `templates/<template>/` both trigger synchronization, recreating only services whose rendered configuration changed; switching between `compose_path` and `template` with an unchanged rendered configuration is adopted without recreating any container (`no_restart`).
- **Generated fleet schema & offline validation (v2.3)**: The JSON Schema for host files is generated from the fleet parser structs (`tide schema host --version v2.3.0 > schemas/host.schema.json`; a test keeps the committed `schemas/host.schema.json` in sync — never edit it by hand). Each release publishes it at `https://github.com/valthaide/tide/releases/download/v2.3.0/host.schema.json` (`.../releases/latest/download/host.schema.json` tracks the newest release), and it ships in the controller image. Host files reference it via `$schema:` (see `examples/fleet/hosts/example.yaml`) for editor completion and highlighting. The image also carries `tide validate <files>` — fully offline (no controller, DB, or network) schema + cross-field validation (mutually exclusive fields, unique app names, template references) with `file: field.path: reason` output, `-o json` for CI, and non-zero exit on error. Fleet CI pins the Tide image of the host's version and runs `tide validate hosts/*.yaml` (see `examples/fleet/validate-ci.yaml`); the controller loads fleet files through the identical validation, so an offline-valid file cannot be rejected for format. The format version is `schema: 1`; unknown versions are rejected with a clear `unsupported_fleet_schema` error, and breaking format changes will raise it.
- **Multi-tenant CI gate**:
  - Per-application repository, commit SHA, and ref isolation.
  - Optional `required_jobs` and `shared_required_jobs` (including parent-linked child pipeline bridges verified by exact SHA and ref) for shared repositories.
  - Automatic re-evaluation when a failed job is retried on the same push pipeline (`failed` → `waiting` → `allowed` without a new commit or controller restart).
  - Strict mode (`strict_pipeline_gate: true` or `TIDE_STRICT_CI_GATE=true`) waits when no trusted `push` or `merge_request_event` pipeline exists instead of allowing untrusted sources.
- **Audited operations & hardened `--force`**:
  - `tidectl reconcile app <name> --force --sha <sha> --reason "<reason>"` is single-use, short-lived, bound to one application and exact commit SHA, revoked automatically if the commit changes, and recorded in persistent `audit_events`.
  - Compose security rules, image digest requirements, and secret checks are never bypassed.
- **Zero-downtime source migration**:
  - `tidectl migrate-repo <app> --repo-url <url> [--branch <branch>] [--compose-path <path>] [--dry-run]` and Fleet updates to `repo_url`, `branch`, or `compose_path` stage the target repository, verify the CI gate for the target commit, render Compose, and compare image+digest, env, ports, volumes, networks, commands, and labels against the running containers.
  - Identical configurations adopt the new source and commit **without restarting or recreating containers** (`mode=no_restart`, `reason=config_identical`); changed configurations run `docker compose up -d --remove-orphans` (without `compose down`) to recreate only changed services (`mode=restarted`).
  - `app.ID`, `ComposeProject`, volumes, and `env_file` are preserved; preflight failures leave the old source and running containers untouched. Outcomes are recorded in structured logs, `audit_events`, and `tide_repo_migrations_total{app,mode,reason}`.
- **Process & CLI safety**:
  - `tide` acquires a non-blocking `flock` (`<dataDir>/tide.lock`) before opening the database or touching application state.
  - `--help`, `--version`, and unknown flags exit immediately without touching files, databases, or Docker.

## GitLab Token Permissions

- **Standard operation (default)**: Requires **read-only** scopes only: `read_api` (for `TIDE_GITLAB_API_TOKEN`) and `read_repository` (for `TIDE_GIT_TOKEN`).
- **Optional `trusted_web_pipelines` policy**: Disabled by default (`enabled: false`). Inspecting manual (`web`) pipeline variables via `GET /projects/:id/pipelines/:pipeline_id/variables` requires **Maintainer** API permissions on the target GitLab project. If the configured token lacks permission to read pipeline variables (`403 Forbidden`), `trusted_web_pipelines` automatically stays inactive and manual pipelines cannot unlock deployments.

## Key Environment Variables

| Variable | Description |
| --- | --- |
| `TIDE_DATA_DIR` | Controller state directory (`/data`, `/opt/tide/data`, or `.`) |
| `TIDE_RUNTIME_DIR` | Compose checkout workspace (default `./.tide-runtime`) |
| `TIDE_API_ADDR` | HTTPS API listen address (default `:8443`) |
| `TIDE_METRICS_ADDR` | Prometheus metrics & health listen address (default `127.0.0.1:8081`) |
| `TIDE_API_TOKEN` | Admin API bearer token |
| `TIDE_OPERATOR_TOKENS` | Comma-separated `actor:token` pairs for named operator audit attribution |
| `TIDE_FORCE_TOKENS` | Optional comma-separated `actor:token` pairs authorized for `--force` CI bypass |
| `TIDE_FLEET_REPO_URL` | Fleet Git repository URL |
| `TIDE_FLEET_HOST` | Host identifier in fleet repository |
| `TIDE_GIT_TOKEN` | Git HTTPS token (`read_repository`) |
| `TIDE_GITLAB_API_TOKEN` | GitLab API token (`read_api`) |
| `TIDE_STRICT_CI_GATE` | Require a trusted pipeline even when `trigger`/`api` pipelines exist |

## CLI Usage

```sh
tidectl --version
tidectl get apps
tidectl get app <name>
tidectl retry <name>
tidectl reconcile app <name> --force --sha <40-char-sha> --reason "incident-123"
tidectl migrate-repo <name> --repo-url https://git.example/tenant/app.git --dry-run
tidectl migrate-repo <name> --template web --dry-run
tidectl check-template --fleet-dir /path/to/fleet-checkout --template web --app-dir /path/to/app
tidectl audit
tidectl validate --fleet-file hosts/example.yaml
tide schema host --version v2.3.0
tide validate hosts/*.yaml
tide validate -o json hosts/*.yaml
```

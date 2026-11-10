# Tide

Tide is a single-host GitOps controller for Docker Compose stacks, paired with the `tidectl` CLI.

## Overview

- **Fleet-driven desired state**: One fleet repository declares the applications running on a host (`hosts/<fqdn>.yaml`), while each application tracks its own tenant or service Git repository (`repo_url`), branch/tag/SHA, and compose configuration.
- **Multi-tenant CI gate**:
  - Per-application repository, commit SHA, and ref isolation.
  - Optional `required_jobs` and `shared_required_jobs` (including parent-linked child pipeline bridges verified by exact SHA and ref) for shared repositories.
  - Automatic re-evaluation when a failed job is retried on the same push pipeline (`failed` → `waiting` → `allowed` without a new commit or controller restart).
  - Strict mode (`strict_pipeline_gate: true` or `TIDE_STRICT_CI_GATE=true`) waits when no trusted `push` or `merge_request_event` pipeline exists instead of allowing untrusted sources.
- **Audited operations & hardened `--force`**:
  - `tidectl reconcile app <name> --force --sha <sha> --reason "<reason>"` is single-use, short-lived, bound to one application and exact commit SHA, revoked automatically if the commit changes, and recorded in persistent `audit_events`.
  - Compose security rules, image digest requirements, and secret checks are never bypassed.
- **Production-safe repository migration**:
  - `tidectl migrate-repo <app> --repo-url <url> [--dry-run]` (and fleet `repo_url` updates) verifies repository readability, green CI, compose validity, absence of persistent data binds inside the app checkout directory, and image availability before stopping the running stack.
  - Volumes and compose project names are preserved; rollback is performed by restoring the previous `repo_url`.
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
tidectl audit
tidectl validate --fleet-file hosts/example.yaml
```

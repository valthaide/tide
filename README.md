<h1 align="center">
  <img src="docs/media/conops.png" alt="ConOps Logo" width="200">
  <br>
  ConOps
</h1>

<p align="center">
  <strong>GitOps for Docker Compose. Like Argo CD, but without Kubernetes.</strong>
</p>

<p align="center">
  <a href="https://github.com/anuragxxd/conops/stargazers"><img src="https://img.shields.io/github/stars/anuragxxd/conops?style=flat-square" alt="GitHub stars"></a>
  <a href="https://hub.docker.com/r/anurag1201/conops"><img src="https://img.shields.io/docker/pulls/anurag1201/conops?style=flat-square" alt="Docker Pulls"></a>
  <a href="https://github.com/anuragxxd/conops/blob/master/LICENSE"><img src="https://img.shields.io/github/license/anuragxxd/conops?style=flat-square" alt="License"></a>
  <a href="https://github.com/anuragxxd/conops/releases"><img src="https://img.shields.io/github/v/release/anuragxxd/conops?style=flat-square" alt="Release"></a>
</p>

<p align="center">
  Point ConOps at a Git repo containing a <code>docker-compose.yaml</code>.<br>
  It clones, pulls, deploys, watches for new commits, detects container drift, and self-heals.<br>
  One binary. No cluster required.
</p>

<p align="center">
  <a href="#quick-start">Quick Start</a> &middot;
  <a href="#features">Features</a> &middot;
  <a href="#usage-guide">Usage Guide</a> &middot;
  <a href="#configuration">Configuration</a> &middot;
  <a href="#method-2-rest-api--cli">API</a>
</p>

---

![ConOps Demo](docs/media/conops-demo.gif)

## Quick Start

Run ConOps with a single command:

```bash
docker run \
  --name conops \
  -p 127.0.0.1:8080:8080 \
  -e CONOPS_RUNTIME_DIR=/tmp/conops-runtime \
  -v /tmp/conops-runtime:/tmp/conops-runtime \
  -v conops_data:/data \
  -v /var/run/docker.sock:/var/run/docker.sock \
  anurag1201/conops:latest
```

Open **http://localhost:8080** and click **New App**.

> **Note:** For production use, we recommended using [Docker Compose](#production-setup).

> **Security:** The port is bound to `127.0.0.1` because ConOps has no built-in
> authentication and mounts the Docker socket. Reach it remotely over an SSH
> tunnel (`ssh -L 8080:127.0.0.1:8080 host`) or a private network, rather than
> publishing it.

## Features

- **Git-driven deployments** &mdash; push to your branch, ConOps handles the rest
- **Continuous reconciliation** &mdash; configurable loop that keeps desired state in sync
- **Self-healing** &mdash; detects missing, exited, or unhealthy containers and recovers
- **Docker preflight + fallback toolchain** &mdash; checks Docker API compatibility before sync
- **Web UI** &mdash; register apps, inspect status, view containers, trigger syncs, read logs
- **REST API + CLI** &mdash; automate everything; nothing in the UI that the API can't do
- **Private repo support** &mdash; GitHub deploy keys with AES-GCM encryption at rest
- **SQLite or PostgreSQL** &mdash; SQLite for single-node, Postgres for production
- **Single binary** &mdash; no runtime dependencies beyond Docker and Git

## Usage Guide

ConOps can be managed in two ways: via the **Web Dashboard** or the **REST API / CLI**.

### Method 1: Web Dashboard

The easiest way to get started. Navigate to `http://localhost:8080`.

*   **Dashboard**: View all registered applications and their current status (`synced`, `syncing`, `pending`, `error`).
*   **New App**: Click the button to register a repository. You'll need the Git URL, branch name, and path to the Compose file.
*   **App Details**: Click on any app to see its sync history, container health, and logs.
*   **Actions**: You can manually trigger a sync or delete an app directly from its card.

### Method 2: REST API & CLI

ConOps is API-first. Every action available in the UI can be performed via the REST API.

#### CLI Tool (`conops-ctl`)

The `conops-ctl` CLI helps you manage applications from your terminal.

**Installation:**

```bash
# Install the latest version to /usr/local/bin
curl -sfL https://raw.githubusercontent.com/valthaide/conops-private/master/install.sh | sh
```

Or download pre-built binaries from the [Releases](https://github.com/valthaide/conops-private/releases) page (private repo access required).

**Configuration:**
Set the controller URL if running remotely (defaults to `http://localhost:8080`):
```bash
export CONOPS_URL="http://conops.example.com:8080"
```

**Commands:**
```bash
# List all apps
./conops-ctl apps list

# Register an app (supports private repos via JSON)
./conops-ctl apps add app.json

# Get detailed status
./conops-ctl apps get <app-id>

# Update configuration (partial updates supported)
./conops-ctl apps update <app-id> --branch feature/login --poll-interval 1m

# Force immediate sync
./conops-ctl apps sync <app-id>
```

#### REST API

You can interact directly with the API using `curl`.

**1. Register an App**
```bash
curl -X POST http://localhost:8080/api/v1/apps/ \
  -H "Content-Type: application/json" \
  -d '{
    "name": "Example App",
    "repo_url": "https://github.com/docker/awesome-compose",
    "repo_auth_method": "public",
    "branch": "master",
    "compose_path": "fastapi/compose.yaml",
    "poll_interval": "30s"
  }'
```

**2. List Apps**
```bash
curl http://localhost:8080/api/v1/apps/
```

**3. Get App Details**
```bash
curl http://localhost:8080/api/v1/apps/{id}
```

**4. Force Sync**
```bash
curl -X POST http://localhost:8080/api/v1/apps/{id}/sync
```

**5. Update App**
```bash
curl -X PATCH http://localhost:8080/api/v1/apps/{id} \
  -H "Content-Type: application/json" \
  -d '{ "poll_interval": "1m" }'
```

**6. Delete App**
```bash
curl -X DELETE http://localhost:8080/api/v1/apps/{id}
```

## Configuration

All configuration is via environment variables.

| Variable | Default | Description |
|----------|---------|-------------|
| `DB_TYPE` | `sqlite` | Storage backend: `sqlite` or `postgres` |
| `DB_CONNECTION_STRING` | &mdash; | Required when using `postgres` |
| `CONOPS_RECONCILE_INTERVAL` | `10s` | How often the reconciler runs |
| `CONOPS_SYNC_TIMEOUT` | `5m` | Max duration for a single sync operation |
| `CONOPS_RETRY_ERRORS` | `false` | Auto-retry apps that entered `error` status |
| `CONOPS_RUNTIME_DIR` | `./.conops-runtime` | Runtime checkout directory used for compose execution |
| `CONOPS_TOOLS_DIR` | `./.conops-tools` | Cache directory for managed Docker CLI and Compose plugin downloads |
| `CONOPS_ENCRYPTION_KEY` | &mdash; | 32-byte key (raw or base64) for deploy key encryption |
| `CONOPS_ENCRYPTION_KEY_FILE` | `/data/conops-encryption.key` | Path to read/write the encryption key |

## Production Setup

For production, we recommend running ConOps with Docker Compose to handle persistence and networking cleanly.

```yaml
services:
  conops:
    image: anurag1201/conops:latest
    ports:
      - "127.0.0.1:8080:8080"
    environment:
      - CONOPS_RUNTIME_DIR=/tmp/conops-runtime
      - DB_TYPE=sqlite
    volumes:
      # Persist SQLite DB and encryption keys
      - conops_data:/data
      # Allow ConOps to manage sibling containers
      - /var/run/docker.sock:/var/run/docker.sock
      # Runtime directory for checkouts
      - /tmp/conops-runtime:/tmp/conops-runtime

volumes:
  conops_data:
```

## Metrics

ConOps exposes Prometheus metrics at `GET /metrics` (no auth; bind to localhost in production).

Useful series:
- `conops_apps{status=...}` — app count by status
- `conops_app_sync_status{app_id,app_name,status}` — current status (`1`)
- `conops_app_last_sync_timestamp_seconds{app_id,app_name}`
- `conops_app_last_sync_error{app_id,app_name}`
- `conops_app_commit_drift{app_id,app_name}`
- `conops_reconcile_loops_total`
- `conops_app_sync_total{app_id,app_name,result}` — `success` / `error`
- `conops_app_sync_duration_seconds{app_id,app_name}`

## Private Repositories

ConOps supports private repositories on GitHub, GitLab.com, and self-hosted Git (including GitLab) via SSH deploy keys.

Use an SSH repo URL (`git@host:group/repo.git` or `ssh://git@host:port/group/repo.git`) and set `repo_auth_method` to `deploy_key`:

```bash
curl -X POST http://localhost:8080/api/v1/apps/ \
  -H "Content-Type: application/json" \
  -d '{
    "name": "Private App",
    "repo_url": "git@git.example.com:my-group/private-repo.git",
    "repo_auth_method": "deploy_key",
    "deploy_key": "-----BEGIN OPENSSH PRIVATE KEY-----\n...\n-----END OPENSSH PRIVATE KEY-----",
    "branch": "main",
    "compose_path": "docker-compose.yml"
  }'
```

Host key verification is strict (`StrictHostKeyChecking=yes`). ConOps maintains a managed `known_hosts` file seeded from `CONOPS_KNOWN_HOSTS_FILE`, `~/.ssh/known_hosts`, or `/etc/ssh/ssh_known_hosts` when present. Missing host keys are fetched automatically:

- `github.com` — from `api.github.com/meta` (with embedded fallbacks)
- `gitlab.com` — via `ssh-keyscan` (with embedded fallbacks)
- other hosts (self-hosted GitLab, etc.) — via `ssh-keyscan`

If keyscan cannot reach the host, pre-seed keys with `CONOPS_KNOWN_HOSTS_FILE`.

> **Security:** Deploy keys are encrypted at rest using AES-GCM. ConOps auto-generates an encryption key on first run, or you can provide your own via `CONOPS_ENCRYPTION_KEY`.

## How It Works

```
                    ┌─────────────┐
                    │  Git Repo   │
                    └──────┬──────┘
                           │ poll
                    ┌──────▼──────┐
                    │ Git Watcher │──── new commit? ──→ mark pending
                    └─────────────┘
                           │
                    ┌──────▼──────┐
                    │ Reconciler  │──── clone/fetch → compose pull → compose up
                    └──────┬──────┘
                           │
                    ┌──────▼──────┐
                    │Drift Checker│──── container missing/exited/unhealthy? → requeue
                    └─────────────┘
```

**Status flow:** `registered` &rarr; `pending` &rarr; `syncing` &rarr; `synced`.

ConOps separates **change detection** (Git watcher) from **state application** (reconciler). This keeps the control loop predictable and easy to reason about.

## Development

```bash
# Run the controller locally
go run ./cmd/conops

# Build the CLI
go build -o bin/conops-ctl ./cmd/conops-ctl

# Run tests
go test ./...
```

## License

MIT &mdash; see [LICENSE](LICENSE).

# ConOps

Приватный форк [ConOps](https://github.com/anuragxxd/conops) — GitOps-контроллер для Docker Compose (модель близка к Flux, без Kubernetes).

## Модель

| Слой | Что |
|------|-----|
| **Config mgmt** | Runtime: `conops` + `cps`, TLS, API token |
| **Fleet-репо** | Какие apps на каком хосте |
| **App-репо** | compose + env_file / опционально vault refs |

Подробности: [ARCHITECTURE.md](ARCHITECTURE.md). План: [ROADMAP.md](ROADMAP.md).  
Changelog: [CHANGELOG.md](CHANGELOG.md). Пути vs Puppet: [docs/PATH_OWNERSHIP.md](docs/PATH_OWNERSHIP.md).

## Компоненты

- **conops** — контроллер (HTTPS API, localhost metrics, fleet, sync)
- **cps** — CLI (`--token`, `cps config`)

## Режимы (`CONOPS_MODE`)

| Mode | Поведение |
|------|-----------|
| `standalone` (default) | Полный контроллер на хосте |
| `agent` | То же + heartbeat на hub (`CONOPS_HUB_URL`) |
| `hub` | Агрегация агентов (`GET /api/v1/agents`), без локального reconcile |

## Быстрый старт CLI

```bash
cps config set-context --name local \
  --url https://127.0.0.1:8443 \
  --token "$(cat bootstrap_token)" \
  --certificate-authority /etc/conops/ca.crt
cps get apps
```

Metrics: `http://127.0.0.1:8081/metrics` · Health: `/healthz`

## Важные env (v1)

| Env | Назначение |
|-----|------------|
| `CONOPS_API_TOKEN` / `bootstrap_token` | Bearer API |
| `CONOPS_MAX_CONCURRENT_SYNCS` | Лимит параллельных sync (default 2) |
| `CONOPS_WEBHOOK_SECRET` | Включает `POST /api/v1/hooks/git` |
| `CONOPS_SOPS` | ignored (warning); remove from env |
| `CONOPS_VAULT_ADDR` + `CONOPS_VAULT_TOKEN` | Resolve `vault://path#key` в env |
| `CONOPS_FLEET_REPO_URL` | Fleet GitOps |
| `CONOPS_GITLAB_API_TOKEN` | optional; CI gate `require_pipeline_success` (`read_api`) |
| `CONOPS_DIRTY_WORKTREE` | `refuse` (default) or `reset` when checkout has local changes |
| `CONOPS_MODE` / `CONOPS_HUB_URL` | agent/hub |

### Fleet app options (v1.0.4+; dirs/globs in v1.0.5)

```yaml
apps:
  - name: monitoring
    repo_url: https://git.example/group/monitoring.git
    repo_auth_method: token
    require_pipeline_success: true
    watch_files:
      - path: prometheus.yml
        action: reload
        reload_url: http://prometheus:9090/-/reload
      - path: prometheus/rules
        action: reload
        reload_url: http://prometheus:9090/-/reload
      - path: grafana/dashboards/*.json
        action: restart
        service: grafana
```

## Лицензия

MIT — см. [LICENSE](LICENSE).

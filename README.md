# ConOps

Приватный форк [ConOps](https://github.com/anuragxxd/conops) — GitOps-контроллер для Docker Compose (модель близка к Flux, без Kubernetes).

## Модель

| Слой | Что |
|------|-----|
| **Config mgmt** | Runtime: `conops` + `cps`, TLS, API token |
| **Fleet-репо** | Какие apps на каком хосте |
| **App-репо** | compose + опционально SOPS / vault refs |

Подробности: [ARCHITECTURE.md](ARCHITECTURE.md). План: [ROADMAP.md](ROADMAP.md).

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
  --insecure-skip-tls-verify
cps get apps
```

Metrics: `http://127.0.0.1:8081/metrics` · Health: `/healthz`

## Важные env (v1)

| Env | Назначение |
|-----|------------|
| `CONOPS_API_TOKEN` / `bootstrap_token` | Bearer API |
| `CONOPS_MAX_CONCURRENT_SYNCS` | Лимит параллельных sync (default 2) |
| `CONOPS_WEBHOOK_SECRET` | Включает `POST /api/v1/hooks/git` |
| `CONOPS_SOPS_AGE_KEY_FILE` / `CONOPS_SOPS` | SOPS decrypt в runtime checkout |
| `CONOPS_VAULT_ADDR` + `CONOPS_VAULT_TOKEN` | Resolve `vault://path#key` в env |
| `CONOPS_FLEET_REPO_URL` | Fleet GitOps |
| `CONOPS_MODE` / `CONOPS_HUB_URL` | agent/hub |

## Лицензия

MIT — см. [LICENSE](LICENSE).

# ConOps

Приватный форк [ConOps](https://github.com/anuragxxd/conops) — GitOps-контроллер для Docker Compose (модель близка к Flux, без Kubernetes).

## Модель

| Слой | Что |
|------|-----|
| **Puppet** | Только ConOps runtime: контроллер `conops` + CLI `cps` |
| **Fleet-репо** | На каком хосте какие apps (repo/branch/path) |
| **App-репо** | `docker-compose.yml`, конфиги, SOPS — desired state стека |

Подробности: [ARCHITECTURE.md](ARCHITECTURE.md). План: [ROADMAP.md](ROADMAP.md).

## Компоненты

- **conops** — контроллер (HTTPS API + localhost metrics, fleet + app reconcile)
- **cps** — CLI к API (`--token`, `cps config`)

## Локальная сборка

```bash
go build -o bin/conops ./cmd/conops
go build -o bin/cps ./cmd/cps
```

## Быстрый старт CLI

```bash
# после старта conops: token в ./bootstrap_token (или CONOPS_API_TOKEN)
cps config set-context --name local \
  --url https://127.0.0.1:8443 \
  --token "$(cat bootstrap_token)" \
  --insecure-skip-tls-verify
cps config use-context local

cps check
cps get apps
cps reconcile app my-app
```

Metrics/healthz: `http://127.0.0.1:8081/metrics`, `/healthz`.

## Лицензия

MIT — см. [LICENSE](LICENSE).

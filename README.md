# ConOps

Приватный форк [ConOps](https://github.com/anuragxxd/conops) — GitOps-контроллер для Docker Compose (модель близка к Flux, без Kubernetes).

## Модель

| Слой | Что |
|------|-----|
| **Puppet** | Только ConOps runtime: контроллер `conops` + CLI `cps` |
| **Fleet-репо** | На каком хосте какие apps (repo/branch/path) |
| **App-репо** | `docker-compose.yml`, конфиги, SOPS — desired state стека |

Подробности: [ARCHITECTURE.md](ARCHITECTURE.md). План работ: [ROADMAP.md](ROADMAP.md).

## Компоненты

- **conops** — контроллер на хосте (reconcile fleet + app repos, compose)
- **cps** — CLI к API контроллера

## Локальная сборка

```bash
go build -o bin/conops ./cmd/conops
go build -o bin/cps ./cmd/cps
docker compose up -d --build
```

## CLI (сейчас)

```bash
export CONOPS_URL=http://127.0.0.1:8080

cps check
cps get apps
cps get app my-app
cps reconcile app my-app
cps suspend app my-app && cps resume app my-app
```

Целевое подключение: HTTPS API + Bearer token + `cps config` contexts (см. architecture).

## Лицензия

MIT — см. [LICENSE](LICENSE).

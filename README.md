# ConOps

Приватный форк [ConOps](https://github.com/anuragxxd/conops) — GitOps-контроллер для Docker Compose без Kubernetes.

Указываете Git-репозиторий с `docker-compose.yml`: ConOps клонирует его, поднимает стек, следит за коммитами и сам чинит drift контейнеров.

## Компоненты

- **conops** — контроллер (контейнер): API, reconciler, `/healthz`, `/metrics`
- **cps** — CLI к контроллеру (в духе flux)

Web UI нет: операторский UX — CLI + метрики.

## Быстрый старт (контроллер)

```bash
docker compose up -d --build
```

API: http://127.0.0.1:8080 (только localhost).

## CLI (`cps`)

```bash
export CONOPS_URL=http://127.0.0.1:8080   # или --url

cps check
cps get controller
cps get apps
cps get app my-app
cps logs app my-app --tail 50
cps reconcile app my-app
cps wait app my-app --timeout 5m
cps export app my-app > app.json
cps create app -f app.json
cps patch app my-app --branch main
cps delete app my-app
```

Сборка локально:

```bash
go build -o bin/cps ./cmd/cps
go build -o bin/conops ./cmd/conops
```

## Метрики и health

- `GET /healthz` — liveness (процесс + БД)
- `GET /metrics` — Prometheus

## Roadmap

См. [ROADMAP.md](ROADMAP.md).

## Лицензия

MIT — см. [LICENSE](LICENSE).

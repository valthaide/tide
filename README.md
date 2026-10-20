# ConOps

Приватный форк [ConOps](https://github.com/anuragxxd/conops) — GitOps-контроллер для Docker Compose без Kubernetes.

Указываете Git-репозиторий с `docker-compose.yml`: ConOps клонирует его, поднимает стек, следит за коммитами и сам чинит drift контейнеров.

## Что умеет этот форк

- SSH deploy keys для GitHub, GitLab и self-hosted Git
- Prometheus-метрики: `GET /metrics`
- UI / REST API / CLI (`conops-ctl`)

## Быстрый старт

```bash
docker compose up -d --build
```

UI: http://127.0.0.1:8080 (только localhost — auth нет, есть доступ к Docker socket).

CLI:

```bash
curl -sfL https://raw.githubusercontent.com/valthaide/conops-private/master/install.sh | sh
conops-ctl apps list
```

## Метрики

`GET /metrics` — статусы apps, drift коммитов, ошибки sync, длительность sync, счётчик reconcile-циклов.

## Лицензия

MIT — см. [LICENSE](LICENSE).

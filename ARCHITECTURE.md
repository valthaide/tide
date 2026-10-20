# Целевая архитектура ConOps (Flux-модель)

Зафиксировано 2026-10-20.

## Три слоя (как у Flux)

```text
┌─────────────────────────────────────────────────────────┐
│  Config management (Puppet / Ansible / …)               │
│  Ставит только runtime: образ conops + пакет/бинарник cps│
│  TLS certs, CONOPS_API_TOKEN (SOPS), bootstrap volume    │
└───────────────────────────┬─────────────────────────────┘
                            │
┌───────────────────────────▼─────────────────────────────┐
│  Fleet-репо                                              │
│  Desired state «какие apps на каком хосте»               │
│  YAML: host → apps[] → repo_url, branch, path, auth ref  │
│  ConOps на хосте reconcili’ит этот репо → registry apps  │
└───────────────────────────┬─────────────────────────────┘
                            │ git pull per app
┌───────────────────────────▼─────────────────────────────┐
│  App-репо                                                │
│  docker-compose.yml, конфиги, secrets.sops.yaml          │
│  Только desired state стека (не «куда деплоить»)         │
└─────────────────────────────────────────────────────────┘
```

| Слой | Git | Кто трогает |
|------|-----|-------------|
| **Runtime** | нет | Config mgmt: conops + cps |
| **Fleet** | отдельный репо | кто решает «какой стек на каком хосте» |
| **App** | отдельные репо | владельцы стека: compose + SOPS |

Config management **не** регистрирует apps через API и **не** кладёт compose на диск вручную. Источник правды apps — fleet git.

`cps create/patch` остаются для отладки/bootstrap; в проде apps приходят из fleet (drift: ручная регистрация vs fleet — выигрывает fleet).

### Пример фрагмента fleet (эскиз)

```yaml
# hosts/app-01.yaml
host: app-01.example.internal
apps:
  - name: monitoring
    repo_url: https://git.example/org/infra/monitoring.git
    branch: main
    compose_path: docker-compose.yml
    repo_auth_method: token   # или deploy_key; секрет — на хосте / в SOPS для ConOps
    poll_interval: 60s
```

ConOps на хосте читает только свой host-документ (файл по hostname или YAML с полем `host:`).

---

## Сеть и endpoint’ы

| Слушатель | Bind | TLS | Auth | Назначение |
|-----------|------|-----|------|------------|
| **API** | LAN / VIP | **HTTPS обязательно** | Bearer token | `cps`, автоматизация |
| **Metrics** | `127.0.0.1` only | HTTP ок | нет | Prometheus scrape |
| **Health** | с metrics или рядом на localhost | HTTP | нет | liveness |

Серверный TLS: файлы в data dir (`/data/tls/cert.pem`, `key.pem`) из config mgmt/SOPS или self-signed при первом старте.

---

## Auth: Bearer token + GitOps

Доступ к API — **token** (не клиентский сертификат). Ближе к типичному kubectl (Bearer), достаточно для закрытой сети.

### Источники token (приоритет)

1. **`CONOPS_API_TOKEN`** (из SOPS / config mgmt) — предпочтительно в проде: hash при старте, plaintext только в secret store.
2. Иначе hash на диске (`/data/api_token.sha256`).
3. Иначе bootstrap: сгенерировать, сохранить hash, plaintext в **`/data/bootstrap_token`** (0600).

`/data/bootstrap_token` **не удалять автоматически** (чтобы «забыл скопировать» ≠ ротация). Удаление — только руками/флагом.

### Recovery без ротации старого

- Есть файл → `cat /data/bootstrap_token` (тот же token).
- Или смотреть `CONOPS_API_TOKEN` в SOPS.

Ротация — только явная операция (отдельная команда/флаг), не часть «забыл скопировать».

### cps-config (аналог kubeconfig)

```yaml
# ~/.conops/config
current-context: app-01
contexts:
  app-01:
    url: https://conops.app-01.example.internal:8443
    token: cps_…
    certificate-authority: ~/.conops/ca.crt  # опционально
```

```bash
# из /etc/conops/secrets.env (нужны права на чтение файла)
sudo cps config init
cps get apps
```

---

## Поток выката

1. Push в **app-репо** (compose/SOPS).
2. При необходимости push в **fleet** (привязка app↔host).
3. ConOps на хосте: sync fleet → обновить registry → poll app-репо → `compose up` (+ sops decrypt по существующей схеме на хосте).
4. Наблюдение: `cps` по HTTPS+token, алерты по `/metrics` на localhost.

---

## Реализовано в коде

1. Fleet reconciler (`CONOPS_FLEET_*`).
2. API HTTPS + metrics/healthz на `127.0.0.1` (отдельный порт).
3. API Bearer auth + `/data/bootstrap_token` (файл не авто-удаляется).
4. `cps config` contexts + `--token` / `CONOPS_TOKEN`.
5. HTTPS git + `repo_auth_method: token` (`CONOPS_GIT_TOKEN` / `git_token`).
6. Алерто-ready метрики (`sync_in_progress`, success age, `requeue{reason}`).

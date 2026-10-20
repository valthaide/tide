# Целевая архитектура ConOps (Tenko / Flux-модель)

Зафиксировано 2026-10-20.

## Три слоя (как у Flux)

```text
┌─────────────────────────────────────────────────────────┐
│  Puppet                                                  │
│  Ставит только runtime: образ conops + пакет/бинарник cps│
│  TLS certs, CONOPS_API_TOKEN (SOPS), bootstrap volume    │
└───────────────────────────┬─────────────────────────────┘
                            │
┌───────────────────────────▼─────────────────────────────┐
│  Fleet-репо (tenko/infra/fleet или аналог)               │
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
| **Runtime** | нет | Puppet: conops + cps |
| **Fleet** | `tenko/infra/fleet` | кто решает «monitoring на mon», «stack X на club-01» |
| **App** | отдельные репо | владельцы стека: compose + SOPS |

Puppet **не** регистрирует apps через API и **не** кладёт compose на диск вручную. Источник правды apps — fleet git.

`cps create/patch` остаются для отладки/bootstrap; в проде apps приходят из fleet (drift: ручная регистрация vs fleet — выигрывает fleet).

### Пример фрагмента fleet (эскиз)

```yaml
# hosts/mon.yaml
host: mon.example.internal
apps:
  - name: monitoring
    repo_url: https://git.example/tenko/infra/monitoring.git
    branch: main
    compose_path: docker-compose.yml
    repo_auth_method: token   # или deploy_key; секрет — на хосте / в SOPS для ConOps
    poll_interval: 60s
```

ConOps на `mon` читает только свой host-документ (или directory matching hostname).

---

## Сеть и endpoint’ы

| Слушатель | Bind | TLS | Auth | Назначение |
|-----------|------|-----|------|------------|
| **API** | LAN / VIP | **HTTPS обязательно** | Bearer token | `cps`, автоматизация |
| **Metrics** | `127.0.0.1` only | HTTP ок | нет | Prometheus scrape |
| **Health** | с metrics или рядом на localhost | HTTP | нет | liveness |

Серверный TLS: файлы в data dir (`/data/tls/cert.pem`, `key.pem`) из Puppet/SOPS или self-signed при первом старте.

---

## Auth: Bearer token + GitOps

Клиентский доступ — **token** (не клиентский сертификат). Проще, ближе к типичному kubectl (Bearer), достаточно для закрытой сети.

### Источники token (приоритет)

1. **`CONOPS_API_TOKEN`** (из SOPS / Puppet) — предпочтительно в проде: hash при старте, plaintext только в secret store.
2. Иначе tokens в БД (hash).
3. Иначе bootstrap: сгенерировать, сохранить hash в БД, plaintext в **`/data/bootstrap_token`** (0600).

`/data/bootstrap_token` **не удалять автоматически** (чтобы «забыл скопировать» ≠ ротация). Удаление — только руками/флагом.

### Recovery без ротации старого

- Есть файл → `cat /data/bootstrap_token` (тот же token).
- Или с localhost: `POST /api/v1/auth/tokens` → **новый** named token, старые остаются.
- Или смотреть `CONOPS_API_TOKEN` в SOPS.

Ротация/revoke — только явные операции.

### cps-config (аналог kubeconfig)

```yaml
# ~/.config/cps/config.yaml
current-context: mon
contexts:
  mon:
    url: https://conops.mon.internal:8443
    token: cps_…
    certificate-authority: ~/.config/cps/ca.crt  # опционально
```

```bash
cps config use-context mon
cps --context mon get apps
```

---

## Поток выката

1. Push в **app-репо** (compose/SOPS).
2. При необходимости push в **fleet** (привязка app↔host).
3. ConOps на хосте: sync fleet → обновить registry → poll app-репо → `compose up` (+ sops decrypt по существующей схеме на хосте).
4. Наблюдение: `cps` по HTTPS+token, алерты по `/metrics` на localhost.

---

## Что ещё предстоит в коде (относительно текущего master)

Уже есть: controller, `cps`, apps в SQLite через API, SSH deploy key, `/healthz`, metrics на том же порту, suspend/resume.

Нужно добавить:

1. Fleet reconciler (git → apps registry).
2. API HTTPS + metrics на отдельном localhost-порту.
3. API Bearer auth + `/data/bootstrap_token` + multi-token create с loopback.
4. `cps config` contexts + `--token`.
5. HTTPS git + token (deploy token к GitLab) для app/fleet репо.
6. Алерто-ready метрики.

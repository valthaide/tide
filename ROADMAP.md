# Roadmap

Целевая модель: [ARCHITECTURE.md](ARCHITECTURE.md).  
Upstream (оригинал форка): [anuragxxd/conops](https://github.com/anuragxxd/conops).

## Чем наша версия отличается от оригинала

Сводка отличий приватного форка (`valthaide/conops-private`, тег **v1.0.0**) от upstream ConOps.

### Продукт и UX

| | Upstream | Наш форк |
|---|----------|----------|
| UI | Web UI | **Убран**; только CLI |
| CLI | `conops-ctl` (если был) | **`cps`** — flux-стиль: get/create/delete/reconcile/check/logs/wait/export/**suspend/resume**, colored tables |
| Документация | EN, публичный продукт | Короткий **RU README**, generic architecture (без привязки к клиенту) |
| Module / brand | upstream path | `github.com/valthaide/conops-private` |

### Безопасность и сеть

| | Upstream | Наш форк |
|---|----------|----------|
| API | Обычно HTTP `:8080` | **HTTPS** (`CONOPS_API_ADDR`, default `:8443`), cert в `/data/tls` или self-signed |
| Auth API | Нет / слабее | **Bearer** — `CONOPS_API_TOKEN` или `/data/bootstrap_token` (+ hash на диске) |
| Metrics | Нет или вместе с API | **`/metrics` + `/healthz` на `127.0.0.1:8081`** (отдельный listener, без auth) |
| CLI auth | — | `--token` / `CONOPS_TOKEN`, **`cps config`** (contexts ≈ kubeconfig) |

### Git и GitOps

| | Upstream | Наш форк |
|---|----------|----------|
| Private git | В основном GitHub / ограничено | **Любой SSH-хост**: GitLab, self-hosted; known_hosts / ssh-keyscan |
| Auth git | public / deploy_key (узко) | `public` · **`deploy_key`** · **`token`** (`CONOPS_GIT_TOKEN` / `git_token`) |
| Источник apps | Ручная регистрация через API | **Fleet-репо** (`CONOPS_FLEET_*`): host YAML → registry (Puppet только runtime) |
| Push-driven | Только poll | Poll **+ optional webhook** `POST /api/v1/hooks/git` |

### Runtime sync

| | Upstream | Наш форк |
|---|----------|----------|
| Секреты apps | Не в контроллере | **SOPS/age** + **Vault** (`vault://path#key`) при sync |
| Concurrent sync | Без лимита / слабо | **`CONOPS_MAX_CONCURRENT_SYNCS`** (default 2) |
| Ошибки sync | Plaintext | **Structured**: code/stage в API + `app_sync_errors_total` |
| Suspend | — | **suspend/resume** (pause poll + auto-reconcile) |
| Health / ops | — | **`GET /healthz`**, containers API, `cps check/logs/wait` |
| Cache git | Хрупкий | Re-clone при битом cache |

### Наблюдаемость

| | Upstream | Наш форк |
|---|----------|----------|
| Prometheus | Нет / минимально | Нативный `/metrics`: apps, drift, sync duration, **in_progress**, success age, **requeue{reason}**, **errors{code,stage}** |

### Мульти-хост

| | Upstream | Наш форк |
|---|----------|----------|
| Топология | Один процесс = один хост | То же (**standalone**) + опционально **`agent`** → **`hub`** (`CONOPS_MODE`, heartbeat `/api/v1/agents`) |

### CI / дистрибуция

| | Upstream | Наш форк |
|---|----------|----------|
| Docker Hub publish | Часто есть | **Убрано** из CI |
| Releases | Публичный GH | Private GH + GoReleaser; optional **GitLab mirror** (URL в secrets) |

### Что сознательно не тащим из «enterprise»-желаний

- Нет localhost mint token endpoint (`POST /auth/tokens`) — только env / bootstrap_token.
- Hub **не заменяет** standalone: локальный reconcile на агенте сохраняется.
- Config mgmt (Puppet/Ansible) **не** регистрирует apps через API в проде — источник правды fleet git.

---

## Сделано в форке (хронология до v1.0.0)

1. UI убран; CLI `cps`; `/healthz`.
2. HTTPS API + Bearer; metrics/healthz на localhost.
3. Fleet reconciler; git `token` + deploy_key (GitLab/self-hosted SSH).
4. Alert metrics (`sync_in_progress`, success age, requeue).
5. Reconciler tests + `AppRuntime` fake.
6. Concurrent sync — `CONOPS_MAX_CONCURRENT_SYNCS`.
7. Structured sync errors — code/stage + `app_sync_errors_total`.
8. SOPS/age + Vault при sync.
9. Optional git webhooks; poll остаётся default.
10. Multi-host — `CONOPS_MODE=standalone|agent|hub`.

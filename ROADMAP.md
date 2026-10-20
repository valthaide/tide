# Ближайшие изменения

Зафиксировано 2026-10-20. Порядок ≈ приоритет реализации.

## 1. Убрать UI, усилить CLI

- Удалить Web UI (`/ui`, `web/templates`, связанные handler’ы).
- Источник правды для оператора: CLI + API + `/metrics`.
- Улучшить CLI: статус apps, drift, last error, sync; удобный `list`/`get` (в духе flux).
- **Имена бинарников:**
  - CLI → `cps` (вместо `conops-ctl`)
  - контроллер в контейнере → `conops` (без переименования)
- Обновить `install.sh`, `.goreleaser.yaml`, README.

## 2. `GET /healthz` (liveness)

Отдельно от `/metrics`:

- процесс отвечает;
- проверка, что БД открывается / доступна (ping store).

Для Puppet/compose/systemd healthcheck.

## 3. HTTPS Git + token

Рядом с `deploy_key`:

- `repo_auth_method`: token (GitLab deploy token / project access token, также GitHub PAT);
- HTTPS `repo_url`;
- хранение token как у deploy key (AES-GCM);
- без `known_hosts` / SSH.

## 4. Алерто-ready метрики

Дополнить существующий `/metrics`:

- `conops_app_sync_in_progress{app_id,app_name}`
- возраст с последнего **успешного** sync (gauge timestamp или seconds)
- причина requeue: label `reason` (`runtime_missing`, `runtime_unhealthy`, `runtime_exited`, …)

---

Вне этого списка пока не расширять scope (auth UI не нужен после удаления UI; SOPS/webhooks — следующей очередью).

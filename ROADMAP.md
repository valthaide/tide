# Ближайшие изменения

Зафиксировано 2026-10-20. Тег релиза — только после закрытия всего списка.

## 1. Убрать UI, усилить CLI — DONE

- Web UI удалён (`/ui`, `web/`, `internal/ui`).
- CLI → `cps` (вместо `conops-ctl`), контроллер → `conops`.
- Команды в духе flux: `get`, `create`, `patch`, `reconcile`, `delete`.

## 2. `GET /healthz` (liveness) — TODO

Отдельно от `/metrics`:

- процесс отвечает;
- проверка, что БД открывается / доступна (ping store).

Для Puppet/compose/systemd healthcheck.

## 3. HTTPS Git + token — TODO

Рядом с `deploy_key`:

- `repo_auth_method`: token (GitLab deploy token / project access token, также GitHub PAT);
- HTTPS `repo_url`;
- хранение token как у deploy key (AES-GCM);
- без `known_hosts` / SSH.

## 4. Алерто-ready метрики — TODO

Дополнить существующий `/metrics`:

- `conops_app_sync_in_progress{app_id,app_name}`
- возраст с последнего **успешного** sync (gauge timestamp или seconds)
- причина requeue: label `reason` (`runtime_missing`, `runtime_unhealthy`, `runtime_exited`, …)

---

Вне этого списка пока не расширять scope.

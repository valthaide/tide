# Ближайшие изменения

Зафиксировано 2026-10-20. Тег релиза — только после закрытия всего списка.

## 1. Убрать UI, усилить CLI — DONE

- Web UI удалён (`/ui`, `web/`, `internal/ui`).
- CLI → `cps`, контроллер → `conops`.
- Flux-стиль: `get`, `create`, `patch`, `reconcile`, `delete`.
- Дополнительно: `check`, `get controller`, контейнеры в `get app`, `logs`, `wait`, `export`.
- Позже: `suspend` / `resume` poll.

## 2. `GET /healthz` (liveness) — DONE

- `GET /healthz`: процесс + ping БД, поле `version`.
- CLI: `cps check` / `cps get controller`.

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

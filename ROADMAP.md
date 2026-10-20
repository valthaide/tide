# Roadmap

Тег релиза — после закрытия блока ниже.  
Целевая модель: [ARCHITECTURE.md](ARCHITECTURE.md).

## Сделано

1. UI убран; CLI `cps` (flux-стиль + check/logs/wait/export/suspend/resume).
2. `GET /healthz` (процесс + DB).
3. **A.** HTTPS API (`CONOPS_API_ADDR`, default `:8443`) + Bearer (`CONOPS_API_TOKEN` / `/data/bootstrap_token`); metrics+healthz на `CONOPS_METRICS_ADDR` (default `127.0.0.1:8081`); `cps --token` / `cps config` contexts.
4. **B.** Fleet reconciler (`CONOPS_FLEET_REPO_URL`, host YAML → apps registry).
5. **C.** `repo_auth_method: token` + `CONOPS_GIT_TOKEN` / `git_token` для HTTPS clone (app + fleet).
6. **D.** Метрики: `app_sync_in_progress`, `app_sync_success_age_seconds`, `app_requeue_total{reason}`.

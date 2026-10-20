# Roadmap

Тег релиза — после закрытия блока ниже.  
Целевая модель: [ARCHITECTURE.md](ARCHITECTURE.md).

## Сделано

1. UI убран; CLI `cps` (flux-стиль + check/logs/wait/export/suspend/resume).
2. `GET /healthz` (процесс + DB).

## Дальше (порядок)

### A. Auth API + split listeners + cps config

- API **HTTPS** (серверный cert из `/data/tls` или bootstrap).
- `/metrics` (+ `/healthz`) на **отдельном порту, bind `127.0.0.1`**.
- Bearer token: `CONOPS_API_TOKEN` и/или `/data/bootstrap_token` (plaintext на диске, hash в БД; файл не авто-удалять).
- `cps`: `--token` / `CONOPS_TOKEN`, `cps config` (contexts ≈ kubeconfig).

### B. Fleet GitOps (источник apps)

- ConOps читает fleet-репо (host → список apps).
- Puppet ставит только conops+cps; apps не через Puppet API.
- App-репо = только compose/SOPS desired state.
- `cps create` — отладка; в проде побеждает fleet.

### C. HTTPS Git + token (к GitLab)

- `repo_auth_method: token` рядом с `deploy_key` для app/fleet clone.

### D. Алерто-ready метрики

- `sync_in_progress`, age успешного sync, `reason` на requeue.

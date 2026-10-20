# Roadmap

Целевая модель: [ARCHITECTURE.md](ARCHITECTURE.md).

## Сделано (v0.3 → v1.0.0)

1. UI убран; CLI `cps`; `/healthz`.
2. HTTPS API + Bearer; metrics/healthz на localhost.
3. Fleet reconciler; git `token` + deploy_key.
4. Alert metrics (`sync_in_progress`, success age, requeue).
5. **Reconciler tests** + `AppRuntime` fake.
6. **Concurrent sync** — `CONOPS_MAX_CONCURRENT_SYNCS` (default 2).
7. **Structured sync errors** — code/stage в API + `app_sync_errors_total`.
8. **SOPS/age + Vault** — decrypt/resolve при sync (`CONOPS_SOPS*`, `CONOPS_VAULT*`).
9. **Git webhooks** (optional) — `POST /api/v1/hooks/git` + `CONOPS_WEBHOOK_SECRET`; poll остаётся default.
10. **Multi-host** — `CONOPS_MODE=standalone|agent|hub`; agent heartbeat → hub `/api/v1/agents`.

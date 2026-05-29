# Runbook: LensHighLatencyP99

**Alert:** HTTP p99 latency > 2s for 5m. **Severity:** warning.

> Starting point, not exhaustive.

## Symptom
The slowest 1% of requests exceed 2s. AI features feel sluggish.

## Likely causes
- Slow upstream provider (`lens:upstream_provider_duration:p99` by provider).
- Token-ledger writes slow (see `LensTokenLedgerSlow` — DB contention).
- CPU throttling / undersized pods, or GC pressure under load.
- A specific slow route (check latency by route).

## First diagnostics
```sh
# Per-route latency — is it one route or global?
histogram_quantile(0.99, sum by (le,route) (rate(lens_http_request_duration_seconds_bucket[5m])))
# Provider contribution:
lens:upstream_provider_duration:p99
# Ledger hot path:
lens:token_ledger_write_duration:p99
kubectl -n <ns> top pods -l app.kubernetes.io/name=lens
```

## Mitigation
- Provider-bound → router downgrade / failover; raise client timeouts deliberately.
- DB-bound → see LensTokenLedgerSlow; check Postgres load + connections.
- Capacity → scale out (HA) or up; confirm requests/limits are sane.

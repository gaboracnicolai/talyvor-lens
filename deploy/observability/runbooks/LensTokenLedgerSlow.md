# Runbook: LensTokenLedgerSlow

**Alert:** token-ledger write p99 > 100ms for 5m. **Severity:** warning.

> Starting point, not exhaustive. This is the hot-path guard we explicitly added — ledger writes sit on the request path.

## Symptom
`lens:token_ledger_write_duration:p99` exceeds 100ms. Minting/credit writes are slow, which backs up request handling and inflates overall latency.

## Likely causes
- Postgres under load: lock contention on `lens_token_balances` (the per-workspace row is locked `FOR UPDATE`-style during apply), slow disk, or connection saturation.
- A hot workspace receiving a burst of credits (row contention on one balance).
- Postgres autovacuum / checkpoint storm.
- Undersized DB or connection pool exhaustion.

## First diagnostics
```sh
# Confirm it's the ledger, not general DB slowness:
lens:token_ledger_write_duration:p99
# Postgres: locks + slow queries
#   SELECT * FROM pg_stat_activity WHERE state != 'idle' ORDER BY query_start;
#   SELECT * FROM pg_locks WHERE NOT granted;
kubectl -n <ns> logs deploy/<release>-lens --tail=200 | grep -i ledger
```

## Mitigation
- DB contention → scale Postgres / tune; check `max_connections` vs pool size.
- Hot workspace → investigate the credit source (mining loop misconfig?).
- If sustained and harming the request path, consider moving ledger writes off the synchronous path (async credit) — a design change, file an issue.

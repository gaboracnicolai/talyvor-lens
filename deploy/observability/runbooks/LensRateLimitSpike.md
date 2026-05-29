# Runbook: LensRateLimitSpike

**Alert:** rate-limit rejections (HTTP 429) sustained > 1 req/s for 10m. **Severity:** warning.

> Starting point, not exhaustive.

## Symptom
Lens is returning 429s at an elevated rate. Some tenants are being throttled.

## Likely causes
- A single tenant exceeding its configured RPM/TPM (legitimately busy, or a runaway client / retry storm).
- Limits set too low after a traffic increase.
- An abusive or misconfigured caller hammering the API.

## First diagnostics
```sh
# Which workspace(s)?
topk(5, sum by (workspace) (rate(lens_rate_limit_rejections_total[5m])))
# Compare to their request volume + the limit types being hit.
kubectl -n <ns> logs deploy/<release>-lens --tail=200 | grep -i "rate limit"
```

## Mitigation
- Legitimate growth → raise that workspace's limits (tenant config).
- Runaway client → contact the tenant; they likely have a retry loop without backoff.
- Abuse → tighten/deny the key.
- Note: rejections protect the platform — a spike isn't always "bad", but a sustained one means someone is being denied service.

# Runbook: LensProviderDown

**Alert:** a provider's circuit breaker open (`lens_circuit_breaker_state == 1`) for 2m. **Severity:** critical.

> Starting point, not exhaustive.

## Symptom
Requests to a provider (e.g. `openai`, `anthropic`) are short-circuiting — the breaker tripped after repeated failures and is refusing calls until it half-opens.

## Likely causes
- Provider outage or elevated 5xx/timeouts upstream.
- Expired / rate-limited / revoked provider API key.
- Network egress problem (DNS, TLS, firewall) from the cluster.

## First diagnostics
```sh
# Which provider + how long open?
lens_circuit_breaker_state == 1
# Upstream error/latency for that provider:
sum by (provider,status) (rate(lens_upstream_provider_requests_total[5m]))
# Provider's own status page; and from a pod:
kubectl -n <ns> exec deploy/<release>-lens -- wget -qO- https://api.openai.com/v1/models
```

## Mitigation
- Provider outage → rely on fallback routing to other providers; communicate impact.
- Key issue → rotate the key in the referenced Secret; restart picks it up.
- Egress → fix networking; the breaker auto half-opens and recovers once calls succeed.

# Runbook: LensInstanceDown

**Alert:** active HA instance count dropped ≥1 below its 15m peak for 5m. **Severity:** critical.

> Starting point, not exhaustive. Only meaningful when HA is enabled (`LENS_HA_ENABLED=true`); the gauge stays 0 otherwise.

## Symptom
`lens_ha_instance_count` fell below its recent peak — a gateway instance disappeared from the HA registry (crashed, OOM-killed, evicted, or failed its heartbeat to Redis).

## Likely causes
- A pod crashed / was OOM-killed / evicted.
- Redis unreachable, so instances can't heartbeat (registry TTL expires them).
- A scale-down (expected — silence/ignore if intentional).
- Node failure.

## First diagnostics
```sh
kubectl -n <ns> get pods -l app.kubernetes.io/name=lens -o wide
kubectl -n <ns> get events --sort-by=.lastTimestamp | tail -20
# Registry view from a live pod:
kubectl -n <ns> exec deploy/<release>-lens -- wget -qO- localhost:8080/ha/status
# Redis reachable?
kubectl -n <ns> exec deploy/<release>-lens -- wget -qO- localhost:8080/readyz
```

## Mitigation
- Crash/OOM → check logs + raise memory limits; let the Deployment reschedule.
- Redis down → restore Redis; instances re-register on the next heartbeat.
- Intentional scale-down → no action (consider tuning the alert's `for`/threshold).
- PodDisruptionBudget should prevent dropping below quorum during voluntary drains — verify it's enabled.

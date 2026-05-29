# Talyvor Lens — Helm chart

The reference Helm chart for deploying the **Lens AI gateway** to Kubernetes.
Track/Docs/Code charts follow this same structure.

> ### ⚠ NOT YET TESTED ON A LIVE CLUSTER
> This chart is validated by **static checks only** — `helm lint`,
> `helm template` (default + HA overrides), and `kubeconform` schema
> validation. It **lints clean, templates without errors, and is schema-valid**.
> It has **not** been `helm install`-ed against a real cluster. Running it on a
> kind/minikube/staging cluster and confirming the pods come up healthy is a
> separate manual step the operator must do before production use.

## What you get by default

A minimal, production-shaped install:

- **one** gateway `Deployment` replica (HA is opt-in),
- a `Service` (ClusterIP), `ConfigMap` (non-secret env), `ServiceAccount`,
- a **pre-install/pre-upgrade migration hook** `Job`,
- secrets supplied **by reference** (the chart creates none),
- external Postgres / Redis / NATS (the chart bundles no databases).

Everything else — HA, autoscaling, PodDisruptionBudget, in-cluster mining
nodes, embedded dev databases, chart-created secrets — is **opt-in, off by
default**.

## Prerequisites

- Kubernetes 1.23+ and Helm 3.8+ (or Helm 4).
- A reachable **Postgres**, **Redis**, and **NATS** (Lens requires all three).
- A pullable gateway image (`ghcr.io/talyvor/talyvor-lens` by default).
- A **Secret** containing the required env (see [Secrets](#secrets)).
- The image must expose the HA endpoints **`/livez`** and **`/readyz`** (added
  by the Lens HA upgrade) — the probes target them.

## Install / upgrade / uninstall

```sh
# 1. Create the required Secret (see Secrets below), e.g.:
kubectl create secret generic lens-env \
  --from-literal=LENS_DATABASE_URL='postgres://…' \
  --from-literal=LENS_REDIS_URL='redis://…' \
  --from-literal=LENS_NATS_URL='nats://…' \
  --from-literal=LENS_OPENAI_API_KEY='sk-…' \
  --from-literal=LENS_ANTHROPIC_API_KEY='sk-ant-…'

# 2. Install (point at your Secret).
helm install lens deploy/helm/lens \
  --set secret.existingSecret=lens-env

# Upgrade
helm upgrade lens deploy/helm/lens --set secret.existingSecret=lens-env

# Uninstall
helm uninstall lens
```

HA example (renders 3 replicas + HPA + PDB + in-cluster nodes):

```sh
helm template lens deploy/helm/lens -f deploy/helm/lens/examples/values-ha.yaml
```

## Values

| Key | Default | Description |
|-----|---------|-------------|
| `replicaCount` | `1` | Gateway replicas. Keep at 1 unless `config.ha.enabled`. |
| `image.repository` | `ghcr.io/talyvor/talyvor-lens` | Gateway image. |
| `image.tag` | `""` | Defaults to chart `appVersion`. |
| `image.pullPolicy` | `IfNotPresent` | |
| `imagePullSecrets` | `[]` | Private-registry pull secrets. |
| `containerPort` | `8080` | Gateway listen port (match `config.listenAddr`). |
| `service.type` / `service.port` | `ClusterIP` / `8080` | Gateway Service. |
| `resources` | 100m/128Mi → 1/512Mi | Requests/limits — tune to traffic. |
| `podSecurityContext` | non-root uid 65532, RuntimeDefault seccomp | Pod security. |
| `securityContext` | readOnlyRootFS, drop ALL caps, no priv-esc | Container security. |
| `probes.liveness.path` | `/livez` | Liveness endpoint (HA). |
| `probes.readiness.path` | `/readyz` | Readiness endpoint (HA, drain-aware). |
| `config.listenAddr` | `0.0.0.0:8080` | `LENS_LISTEN_ADDR`. |
| `config.logLevel` | `info` | `LENS_LOG_LEVEL`. |
| `config.ha.enabled` | `false` | `LENS_HA_ENABLED`. Required before scaling >1. |
| `config.ha.{heartbeatSec,instanceTtlSec,drainTimeoutSec}` | 5/15/30 | HA timers. |
| `config.extraEnv` | `{}` | Extra **non-secret** `LENS_*` env. |
| `secret.existingSecret` | `""` | **Recommended.** Name of a pre-created Secret. |
| `secret.create` | `false` | **Dev only.** Create a Secret from `secret.data`. |
| `secret.data` | `{}` | **Dev only.** Never commit real secrets. |
| `postgres.embedded` / `redis.embedded` | `false` | Dev-only embedded DBs (see below). |
| `migrations.enabled` | `true` | Run the migration hook Job. |
| `migrations.image` / `command` / `args` | `""` / `[]` / `[]` | Migration mechanism (see below). |
| `autoscaling.enabled` | `false` | HPA (only with HA). |
| `autoscaling.{min,max}Replicas` | 2 / 10 | |
| `autoscaling.target{CPU,Memory}UtilizationPercentage` | 70 / 80 | |
| `podDisruptionBudget.enabled` | `false` | PDB for the HA case. |
| `nodes.enabled` | `false` | Master switch for in-cluster mining nodes. |
| `nodes.{node,cachenode,embednode}.*` | see `values.yaml` | Per-node-type workload config. |

Full, commented defaults live in [`values.yaml`](./values.yaml); bad values are
caught at lint time by [`values.schema.json`](./values.schema.json).

## Secrets

The gateway reads these env via `envFrom` (a ConfigMap for non-secret values +
a Secret for the rest). The Secret must contain at least:

```
LENS_DATABASE_URL   LENS_REDIS_URL   LENS_NATS_URL
LENS_OPENAI_API_KEY   LENS_ANTHROPIC_API_KEY
```

…and optionally `LENS_JWT_SECRET`, `LENS_GOOGLE_API_KEY`, and other provider
keys.

**The chart creates no secrets by default.** Three supported patterns:

1. **By reference (recommended):** create the Secret yourself and set
   `secret.existingSecret=<name>`.
2. **External secret managers (recommended for prod):** use the
   [External Secrets Operator](https://external-secrets.io/) or
   [Sealed Secrets](https://sealed-secrets.netlify.app/) to materialise a
   Secret, then reference it via `secret.existingSecret`. (`serviceAccount.annotations`
   is the place for IRSA / Workload Identity if your secret store needs it.)
3. **Dev/test only:** `secret.create=true` + `secret.data={…}` renders a Secret
   from values. **Never commit real secrets to values.**

If neither `existingSecret` nor `create` is set, the gateway expects a Secret
named `<release>-lens-env` to exist — create it or the pods won't start.

## Embedded databases (dev only)

Production deployments point at **external** Postgres/Redis/NATS. For local
dev you may want in-cluster Bitnami subcharts. They are **intentionally not
declared** in `Chart.yaml` (that would force `helm dependency build` — a
network fetch — on every lint/template). To use them, add this to `Chart.yaml`
and run `helm dependency build`:

```yaml
dependencies:
  - name: postgresql
    version: "~15"
    repository: https://charts.bitnami.com/bitnami
    condition: postgres.embedded
  - name: redis
    version: "~19"
    repository: https://charts.bitnami.com/bitnami
    condition: redis.embedded
```

then set `postgres.embedded=true` / `redis.embedded=true` and point
`LENS_DATABASE_URL` / `LENS_REDIS_URL` at the subchart Services. **Not for
production.**

## Migrations

`migrations.enabled=true` installs a **pre-install/pre-upgrade hook Job** that
runs **before** new gateway pods roll out (hook weight `-5`).

⚠ **There is no working default migration command.** The gateway image ships
neither a `migrate` subcommand nor the SQL files, so the chart cannot guess how
to migrate. With `migrations.command` unset, the hook **fails closed** (exits
non-zero with guidance) rather than pretend migrations ran. Choose one:

- point `migrations.image` at a dedicated migrations image and set
  `migrations.command`, or
- use a migration tool (`migrate`, Flyway, …) image + command, or
- set `migrations.enabled=false` and run migrations out of band.

## High availability

Set `config.ha.enabled=true` (turns on `LENS_HA_ENABLED`, which the gateway
needs before it's safe to run multiple replicas), then raise `replicaCount` or
enable `autoscaling`/`podDisruptionBudget`. See `examples/values-ha.yaml`.

## In-cluster mining nodes (advanced, optional)

`nodes.enabled=true` renders optional `Deployment`/`DaemonSet` workloads for the
`talyvor-node` / `-cachenode` / `-embednode` mining binaries. These are **not**
part of a standard gateway deploy and each needs its **own published image**
(the default gateway image contains only the gateway binary). Off by default.

## Raw manifests (no Helm)

[`deploy/k8s/`](../../k8s/) holds plain YAML (a kustomize base) rendered from
this chart with default values, for operators who don't use Helm. **Helm is the
source of truth** — regenerate with `make k8s-manifests`, don't hand-edit.

## Validation

```sh
make helm-lint        # helm lint
make helm-template    # render default + HA override
make helm-validate    # render | kubeconform (schema validation)
make k8s-manifests    # regenerate deploy/k8s from the chart
```

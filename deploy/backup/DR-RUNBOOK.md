# Lens — Disaster Recovery Runbook

How to recover Lens's Postgres data when something goes wrong. This runbook is
the procedure; the machinery is in `deploy/backup/scripts/`. (A Helm CronJob
that runs `pg_backup.sh` on a schedule is a **planned follow-up** — see
[Scheduling backups](#scheduling-backups); until it lands you schedule the
script yourself.)

> ## ⚠ HONEST STATUS — read this first
> This runbook + scripts are **statically validated** (shellcheck-clean, the
> chart lints/templates, and the **local** backup→restore→verify round-trip
> passes via `make backup-verify-local`). That proves the *machinery* works.
> It does **NOT** prove your *production* backups restore correctly. The only
> thing that proves that is a **periodic restore drill against real backups**
> (see [Backup is not validated until restored](#backup-is-not-validated-until-restored)).
> **A backup you have never restored from is not a backup.** Treat the targets
> below as targets to validate, not guarantees.

## RTO / RPO targets (to be validated)

| Metric | Target | Basis | How to validate |
|--------|--------|-------|-----------------|
| **RPO** (max data loss) | **≤ 24h** | nightly `pg_backup.sh` run | shorten the schedule, or add WAL/PITR (see `PITR.md`) for seconds-RPO |
| **RTO** (time to recover) | **measure it — do not assume** | dominated by DB size ÷ restore throughput + DNS/secret/rollout time | time a real `pg_restore.sh` of a prod-sized backup during a drill; record the number below |

Do **not** publish a fake RTO number. Run one full restore of a
production-sized backup, time it end to end (download + gunzip + `pg_restore` +
app rollout + readiness), and use *that* as your RTO. Re-measure as the DB grows.

## Prerequisites

- Backups are being produced on a schedule by `pg_backup.sh`, and you have
  recent backups in your local dir and/or S3 bucket. (The Helm CronJob that
  automates this is a planned follow-up — until then, schedule `pg_backup.sh`
  yourself; see [Scheduling backups](#scheduling-backups).)
- DB admin credentials available via the same Kubernetes Secret the gateway
  uses (`secret.existingSecret`) — never hardcode them.
- `pg_restore`, `psql`, `gunzip` available where you run the scripts (the
  `postgres:16` image has them; the scripts are bash).

## Scheduling backups

`pg_backup.sh` is the machinery; running it on a schedule is your
responsibility until the Helm CronJob lands. A native Kubernetes
`CronJob` (gated `backup.enabled=false`, shipping `pg_backup.sh` via a
ConfigMap) is a **tracked follow-up** to be added once the chart is on `main`.
Until then, schedule backups by any of:

- a hand-written Kubernetes `CronJob` running `postgres:16` with `pg_backup.sh`
  mounted and DB creds from your existing Secret;
- a host `cron` entry / systemd timer invoking the script;
- your managed-Postgres provider's built-in scheduled snapshots (in addition
  to these portable dumps — see `PITR.md`).

Whatever you use, **alert on non-zero exit** — the script fails loud precisely
so a missed/failed backup is visible, never silent.

---

## Scenario (a): accidental data loss in a single table

Symptom: someone deleted/corrupted rows (or a table) but the database is
otherwise healthy. You do **not** want to overwrite the whole prod DB.

1. Find the most recent backup from *before* the incident (filenames are UTC
   timestamped: `lens-YYYYMMDDTHHMMSSZ-{daily,weekly}.dump.gz`).
2. Restore it into a **scratch** database (never touch prod yet):
   ```sh
   TARGET_DB=lens_recover \
   psql -d postgres -c 'CREATE DATABASE lens_recover'
   deploy/backup/scripts/pg_restore.sh \
     --file lens-<ts>-daily.dump.gz --target-db lens_recover --no-clean \
     --yes-i-understand-this-overwrites
   ```
3. Extract only the affected rows/table from `lens_recover` and merge back into
   prod with care (e.g. `pg_dump --data-only -t <table> lens_recover | psql <prod>`,
   or hand-crafted `INSERT … ON CONFLICT`). **Review before applying.**
4. For the token-ledger specifically, see [the ledger note](#token-ledger-special-note)
   before re-inserting rows — it is append-only and financially meaningful.
5. Drop the scratch DB when done.

## Scenario (b): full database loss

Symptom: the database is gone / unrecoverable; you have backups.

1. Provision a fresh, empty Postgres (same major version) and put its admin
   creds in the Secret.
2. Restore the latest good backup directly into it:
   ```sh
   # PGHOST/PGPORT/PGUSER/PGPASSWORD point at the new DB; or use DATABASE_URL.
   deploy/backup/scripts/pg_restore.sh \
     --file lens-<ts>-daily.dump.gz \
     --yes-i-understand-this-overwrites          # uses DATABASE_URL's db
   ```
   (Use `--no-clean` if the target is brand-new/empty.)
3. Point Lens at the restored DB (update the Secret's `LENS_DATABASE_URL`),
   roll the Deployment, and confirm `/readyz` goes green.
4. **Time this.** That elapsed time is your real RTO — record it below.

## Scenario (c): corrupted / unusable backup

How you'd know: `backup_verify.sh` FAILs, `pg_restore` errors mid-stream, the
file is truncated/zero-byte, or gunzip reports a CRC error.

1. **Do not delete the bad backup** — it's evidence. Move on to the next-older
   backup and verify it:
   ```sh
   deploy/backup/scripts/backup_verify.sh --file lens-<older-ts>-daily.dump.gz
   ```
2. Walk backwards (daily → weekly) until one PASSes. Restore from the newest
   PASSing backup (Scenario b). Your effective RPO just got worse by however
   far back you had to go — note it in the incident.
3. Root-cause the corruption: disk full mid-dump? interrupted upload? The
   backup script refuses zero-byte dumps and exits non-zero on any failure, so
   a *silent* corruption usually means storage-side rot — verify more often
   (see drill cadence) and consider checksums/object-lock on the bucket.

## Scenario (d): region / cluster loss (restore to new infra)

Symptom: the whole cluster/region is gone. This is why backups go to an
**off-cluster** S3 bucket (`BACKUP_S3_BUCKET`), ideally in another region.

1. Stand up a new cluster + a fresh Postgres in the surviving/region B.
2. Fetch the latest backup from the bucket:
   ```sh
   aws s3 cp s3://$BACKUP_S3_BUCKET/lens/lens-<ts>-daily.dump.gz . \
     ${BACKUP_S3_ENDPOINT:+--endpoint-url "$BACKUP_S3_ENDPOINT"}
   ```
3. Restore into the new DB (Scenario b), install the chart pointed at it, verify
   `/readyz`.
4. Repoint DNS / ingress to the new cluster.
5. If you use managed-Postgres PITR (see `PITR.md`), prefer a cross-region
   replica/PITR restore here for a far tighter RPO than the daily dump.

---

## Token-ledger special note

`lens_token_ledger` is **append-only** and **financially meaningful** — it
records minted/spent LENS and drives `lens_token_balances`. Restoring it to a
**stale** state has economic consequences:

- Rows appended **after** the backup (mints, spends, conversions) are **lost** —
  balances will be wrong, and any LENS minted/spent in that window is
  effectively un-recorded.
- Restoring it **alongside** a newer copy (Scenario a merge) risks **double-
  counting** or PK collisions.

Before trusting a restored ledger:
1. Compare `SELECT count(*), max(created_at) FROM lens_token_ledger` between the
   restored copy and any surviving source-of-truth (e.g. application logs, the
   economy/audit trail, provider invoices).
2. Reconcile the gap window explicitly — identify ledger events that occurred
   after the backup timestamp and decide (with whoever owns the economy)
   whether to replay, write compensating entries, or accept the loss. **Do not
   silently let balances drift.**
3. Recompute `lens_token_balances` from the ledger after any manual surgery.

When in doubt, restore the ledger into a scratch DB first and reconcile there
before touching production.

## BACKUP IS NOT VALIDATED UNTIL RESTORED

A backup you have never restored from is **not** a backup — it's an untested
hope. The only proof is a restore drill.

**The drill** (`backup_verify.sh`) restores the latest backup into a throwaway
scratch DB, runs sanity checks (tables present, token-ledger queryable + row
count sane), reports PASS/FAIL, and drops the scratch DB. It never touches prod.

```sh
# Against real backups (point PG* env at an admin role with CREATEDB):
BACKUP_DIR=/path/to/backups \
PGHOST=… PGPORT=5432 PGUSER=… PGPASSWORD=… MAINT_DB=postgres \
deploy/backup/scripts/backup_verify.sh

# Or the fully-local, infra-free machinery check (docker):
make backup-verify-local
```

**Recommended cadence**

| Drill | Cadence | Notes |
|-------|---------|-------|
| `backup_verify.sh` against the latest prod backup | **weekly** | catches storage rot / broken dumps early; cheap (scratch DB) |
| Full restore of a prod-sized backup into fresh infra, timed | **quarterly** | the only way to keep your RTO number honest; also rehearses Scenario (b)/(d) |
| `make backup-verify-local` | **in CI / on any backup-script change** | proves the machinery still round-trips |

Automate the weekly drill (a CronJob running `backup_verify.sh`) and **alert on
FAIL**. Record every drill result in the table below.

## DR contacts & escalation (maintain this)

| Role | Name | Contact | Notes |
|------|------|---------|-------|
| DR owner | _TODO_ | _TODO_ | owns this runbook |
| DBA / Postgres | _TODO_ | _TODO_ | |
| Platform on-call | _TODO_ | _TODO_ | cluster/infra |
| Economy owner | _TODO_ | _TODO_ | token-ledger reconciliation decisions |

## Restore-drill log (maintain this)

| Date (UTC) | Drill type | Backup tested | Result | Measured RTO | Run by |
|------------|-----------|---------------|--------|--------------|--------|
| _TODO_ | _weekly / quarterly_ | _filename_ | _PASS/FAIL_ | _e.g. 0h42m_ | _name_ |

> If the most recent row is older than your cadence, treat DR as **unverified**
> until you run a drill.

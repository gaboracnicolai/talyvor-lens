# Point-in-Time Recovery (PITR) for Lens

This is **operator guidance**, not chart machinery. `pg_backup.sh` produces
nightly `pg_dump`s (run on a schedule — the automating Helm CronJob is a planned
follow-up; see `DR-RUNBOOK.md`). That gives a **coarse RPO** — you can lose up
to a full backup interval (24h by default) of writes. If you need a
tighter RPO, you want **WAL archiving / PITR**, which is configured on the
**Postgres side**, not in this chart.

## The tradeoff

| Approach | RPO | RTO | Complexity | Owned by |
|----------|-----|-----|------------|----------|
| `pg_dump` (`pg_backup.sh`, scheduled) | up to backup interval (≈24h) | restore one dump (minutes–hours, size-dependent) | low | these scripts |
| Continuous WAL archiving + base backups (PITR) | seconds–minutes | base restore + WAL replay | high | your Postgres / platform |
| Managed-Postgres PITR (RDS, Cloud SQL, Crunchy, Aiven…) | seconds–minutes | provider console / API | low (provider runs it) | your DB provider |

A `pg_dump` is simple, portable, and easy to verify (see `backup_verify.sh`).
It is **not** a fine-RPO solution. WAL/PITR is fine-RPO but is real operational
weight: you must archive every WAL segment durably, take periodic base
backups, monitor archiving lag, and rehearse replay to a recovery target.

## Recommendation

**Use managed-Postgres PITR for tight-RPO needs.** RDS / Cloud SQL / Crunchy /
Aiven all provide point-in-time restore with a few clicks or one API call, and
they operate the WAL pipeline for you. That is the pragmatic path — running
your own WAL archiving correctly (and *proving* it restores) is a sustained
commitment most teams should not take on just for Lens.

Keep the `pg_dump` CronJob **as well** even with managed PITR: it gives you a
portable, provider-independent artifact you can restore anywhere (e.g. into a
different cloud during a region/provider outage — see DR scenario (d)), and it's
trivially verifiable off-platform.

## If you must self-host WAL archiving

This is a sketch, not a turnkey recipe — validate it yourself end to end.

1. **Enable archiving** on the primary (`postgresql.conf`):
   ```
   wal_level = replica
   archive_mode = on
   archive_command = 'test ! -f /wal-archive/%f && cp %p /wal-archive/%f'   # ship %p durably (S3/NFS/…)
   ```
   In practice use a tool that ships WAL to object storage with retries +
   verification — **pgBackRest** or **WAL-G** — rather than a raw `cp`.
2. **Take base backups** regularly (`pg_basebackup`, or pgBackRest/WAL-G).
3. **Monitor**: archiving lag, last-archived segment, base-backup age. Alert if
   archiving stalls — a silently broken `archive_command` means no PITR.
4. **Restore to a target time**: restore the base backup, set
   `restore_command` + `recovery_target_time` (Postgres 12+: `recovery.signal`),
   start, let it replay WAL to the target.
5. **Rehearse it.** Same rule as `pg_dump`: an untested PITR setup is not a
   recovery capability. Drill it on a schedule into throwaway infra.

pgBackRest (<https://pgbackrest.org>) and WAL-G (<https://github.com/wal-g/wal-g>)
are the mature tools here and handle most of the above correctly; prefer them
over hand-rolled `archive_command`s.

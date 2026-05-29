#!/usr/bin/env bash
#
# pg_backup.sh — back up the Lens Postgres database.
#
# Produces a custom-format pg_dump (-Fc), gzips it, names it with a UTC
# timestamp, writes it to a local directory and (optionally) uploads it to an
# S3-compatible bucket. Prunes local backups by a daily/weekly retention
# policy. Fails LOUD: any error exits non-zero so a CronJob is marked failed
# rather than silently producing nothing.
#
# Configuration (env only — never hardcode secrets):
#   DATABASE_URL        Postgres connection URI (preferred). If unset, the
#                       standard PG* vars (PGHOST/PGPORT/PGUSER/PGPASSWORD/
#                       PGDATABASE) are used by pg_dump directly.
#   BACKUP_DIR          Local output directory (default /backups).
#   BACKUP_S3_BUCKET    If set, upload to s3://$BACKUP_S3_BUCKET/$BACKUP_S3_PREFIX/.
#   BACKUP_S3_ENDPOINT  S3-compatible endpoint URL (e.g. MinIO/R2/Wasabi). Optional.
#   BACKUP_S3_PREFIX    Key prefix within the bucket (default "lens").
#   KEEP_DAILY          Local daily backups to retain (default 7).
#   KEEP_WEEKLY         Local weekly (Sunday) backups to retain (default 4).
#
# S3 retention is NOT done here — use a bucket lifecycle policy (the
# production-correct pattern). This script only prunes the local directory.
set -euo pipefail

BACKUP_DIR="${BACKUP_DIR:-/backups}"
BACKUP_S3_PREFIX="${BACKUP_S3_PREFIX:-lens}"
KEEP_DAILY="${KEEP_DAILY:-7}"
KEEP_WEEKLY="${KEEP_WEEKLY:-4}"

log() { printf '%s [pg_backup] %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" >&2; }
die() { log "ERROR: $*"; exit 1; }

command -v pg_dump >/dev/null 2>&1 || die "pg_dump not found on PATH"
command -v gzip >/dev/null 2>&1 || die "gzip not found on PATH"

mkdir -p "$BACKUP_DIR" || die "cannot create BACKUP_DIR=$BACKUP_DIR"

ts="$(date -u +%Y%m%dT%H%M%SZ)"
# Tag Sunday backups as "weekly" at creation time so retention needs no
# date-parsing (portable across GNU + busybox date).
if [ "$(date -u +%u)" = "7" ]; then tier="weekly"; else tier="daily"; fi
outfile="$BACKUP_DIR/lens-${ts}-${tier}.dump.gz"
tmpfile="${outfile}.partial"

log "starting backup -> $outfile"

# -Fc = custom format (selective restore, parallelizable). We gzip the result
# for uniform handling/transport; restore + verify gunzip before pg_restore.
if [ -n "${DATABASE_URL:-}" ]; then
	pg_dump -Fc --no-owner --no-privileges "$DATABASE_URL" | gzip -c >"$tmpfile" || die "pg_dump failed"
else
	pg_dump -Fc --no-owner --no-privileges | gzip -c >"$tmpfile" || die "pg_dump failed (using PG* env)"
fi

# Guard against a zero-byte dump masquerading as success.
if [ ! -s "$tmpfile" ]; then
	rm -f -- "$tmpfile"
	die "backup is empty — refusing to keep it"
fi
mv -- "$tmpfile" "$outfile"
log "backup written ($(wc -c <"$outfile" | tr -d ' ') bytes)"

# Optional upload to S3-compatible storage.
if [ -n "${BACKUP_S3_BUCKET:-}" ]; then
	command -v aws >/dev/null 2>&1 || die "BACKUP_S3_BUCKET set but aws CLI not found"
	endpoint_args=()
	if [ -n "${BACKUP_S3_ENDPOINT:-}" ]; then
		endpoint_args=(--endpoint-url "$BACKUP_S3_ENDPOINT")
	fi
	dest="s3://${BACKUP_S3_BUCKET}/${BACKUP_S3_PREFIX}/$(basename "$outfile")"
	log "uploading -> $dest"
	aws "${endpoint_args[@]}" s3 cp "$outfile" "$dest" || die "S3 upload failed"
	log "upload complete"
fi

# Local retention: keep newest KEEP_DAILY of all backups, plus newest
# KEEP_WEEKLY of the Sunday "weekly" backups; delete the rest.
prune_local() {
	local keep_list f k protected
	keep_list="$(mktemp)"
	find "$BACKUP_DIR" -maxdepth 1 -type f -name 'lens-*.dump.gz' | sort | tail -n "$KEEP_DAILY" >>"$keep_list"
	find "$BACKUP_DIR" -maxdepth 1 -type f -name 'lens-*-weekly.dump.gz' | sort | tail -n "$KEEP_WEEKLY" >>"$keep_list"

	while IFS= read -r f; do
		protected=0
		while IFS= read -r k; do
			if [ "$f" = "$k" ]; then protected=1; break; fi
		done <"$keep_list"
		if [ "$protected" -eq 0 ]; then
			rm -f -- "$f"
			log "pruned old backup: $(basename "$f")"
		fi
	done < <(find "$BACKUP_DIR" -maxdepth 1 -type f -name 'lens-*.dump.gz' | sort)

	rm -f -- "$keep_list"
}
prune_local

log "done"

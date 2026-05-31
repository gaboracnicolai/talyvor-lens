#!/usr/bin/env bash
#
# backup_verify.sh — the RESTORE DRILL. Restores the latest (or a chosen)
# backup into a throwaway scratch database, runs sanity checks, reports
# PASS/FAIL, then drops the scratch database. This is the ONLY way to prove a
# backup is actually restorable — run it on a schedule.
#
# "A backup you have never restored from is not a backup."
#
# Usage:
#   backup_verify.sh [--file FILE]
#
#   --file FILE   Backup to verify (.dump/.dump.gz). Default: newest in BACKUP_DIR.
#   -h, --help    Show this help.
#
# Env:
#   BACKUP_DIR              Where to find backups when --file is omitted (default /backups).
#   MAINT_DB                Maintenance DB to CREATE/DROP the scratch DB from (default postgres).
#   SCRATCH_DB_PREFIX       Scratch DB name prefix (default lens_verify).
#   VERIFY_REQUIRED_TABLES  Space-separated tables that MUST exist
#                           (default "lens_token_ledger lens_token_balances").
# Connection: PGHOST/PGPORT/PGUSER/PGPASSWORD (the connecting role needs CREATEDB).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
BACKUP_DIR="${BACKUP_DIR:-/backups}"
MAINT_DB="${MAINT_DB:-postgres}"
SCRATCH_DB_PREFIX="${SCRATCH_DB_PREFIX:-lens_verify}"
VERIFY_REQUIRED_TABLES="${VERIFY_REQUIRED_TABLES:-lens_token_ledger lens_token_balances}"

FILE=""
while [ $# -gt 0 ]; do
	case "$1" in
		--file) FILE="${2:-}"; shift 2 ;;
		-h|--help)
			cat <<'EOF'
backup_verify.sh — restore-drill: restore the latest (or chosen) backup into a
throwaway scratch DB, run sanity checks, report PASS/FAIL, then drop it.

Usage: backup_verify.sh [--file FILE]
  --file FILE   Backup to verify (default: newest in BACKUP_DIR).
  -h, --help    Show this help.

Env: BACKUP_DIR, MAINT_DB, SCRATCH_DB_PREFIX, VERIFY_REQUIRED_TABLES,
     PGHOST/PGPORT/PGUSER/PGPASSWORD (connecting role needs CREATEDB).
EOF
			exit 0 ;;
		*) echo "unknown argument: $1 (try --help)" >&2; exit 1 ;;
	esac
done

log() { printf '%s [backup_verify] %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" >&2; }
die() { log "ERROR: $*"; exit 1; }

command -v psql >/dev/null 2>&1 || die "psql not found on PATH"

if [ -z "$FILE" ]; then
	FILE="$(find "$BACKUP_DIR" -maxdepth 1 -type f -name 'lens-*.dump.gz' | sort | tail -n 1)"
	[ -n "$FILE" ] || die "no backups found in $BACKUP_DIR (and no --file given)"
fi
[ -f "$FILE" ] || die "backup file not found: $FILE"

SCRATCH="${SCRATCH_DB_PREFIX}_$(date -u +%Y%m%d%H%M%S)"

maint() { psql -v ON_ERROR_STOP=1 -d "$MAINT_DB" -tAc "$1"; }
scratch_sql() { psql -v ON_ERROR_STOP=1 -d "$SCRATCH" -tAc "$1"; }

# Invoked via `trap cleanup EXIT` below, not called directly.
# shellcheck disable=SC2329
cleanup() {
	# Always drop the scratch DB, even on failure. FORCE handles lingering conns.
	psql -d "$MAINT_DB" -tAc "DROP DATABASE IF EXISTS \"$SCRATCH\" WITH (FORCE)" >/dev/null 2>&1 \
		|| psql -d "$MAINT_DB" -tAc "DROP DATABASE IF EXISTS \"$SCRATCH\"" >/dev/null 2>&1 || true
}
trap cleanup EXIT

log "verifying $FILE into scratch DB $SCRATCH"
maint "CREATE DATABASE \"$SCRATCH\"" >/dev/null || die "could not create scratch DB"

# Restore into the fresh scratch DB (no --clean: it's empty).
TARGET_DB="$SCRATCH" "$SCRIPT_DIR/pg_restore.sh" \
	--file "$FILE" --target-db "$SCRATCH" --no-clean \
	--yes-i-understand-this-overwrites || die "restore into scratch DB failed"

# ── sanity checks ──
fail=0

table_count="$(scratch_sql "SELECT count(*) FROM information_schema.tables WHERE table_schema='public'")"
if [ "${table_count:-0}" -gt 0 ]; then
	log "CHECK ok: $table_count public tables present"
else
	log "CHECK FAIL: no public tables in restored DB"; fail=1
fi

for tbl in $VERIFY_REQUIRED_TABLES; do
	reg="$(scratch_sql "SELECT to_regclass('public.${tbl}')")"
	if [ -n "$reg" ]; then
		log "CHECK ok: required table present: $tbl"
	else
		log "CHECK FAIL: required table MISSING: $tbl"; fail=1
	fi
done

# Token-ledger is append-only + financially meaningful — confirm it's queryable
# and report its row count so the operator can eyeball it against expectations.
if [ -n "$(scratch_sql "SELECT to_regclass('public.lens_token_ledger')")" ]; then
	ledger_rows="$(scratch_sql "SELECT count(*) FROM lens_token_ledger")"
	if [ "${ledger_rows:-x}" -ge 0 ] 2>/dev/null; then
		log "CHECK ok: lens_token_ledger queryable, row count = $ledger_rows"
	else
		log "CHECK FAIL: lens_token_ledger not queryable"; fail=1
	fi
fi

if [ "$fail" -eq 0 ]; then
	log "RESULT: PASS — backup $FILE restored + passed sanity checks"
	exit 0
fi
log "RESULT: FAIL — backup $FILE did NOT pass verification"
exit 1

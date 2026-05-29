#!/usr/bin/env bash
#
# pg_restore.sh — restore a Lens backup (produced by pg_backup.sh) into a
# target database. DESTRUCTIVE: requires an explicit confirmation flag so it
# can never run by accident. Can restore into a DIFFERENT database name than
# production (used by backup_verify.sh for non-destructive restore drills).
#
# Usage:
#   pg_restore.sh --file FILE --yes-i-understand-this-overwrites [options]
#
# Options:
#   --file FILE        Backup file (.dump or .dump.gz). REQUIRED.
#   --target-db NAME   Database to restore INTO. Default: PGDATABASE, or the
#                      dbname in DATABASE_URL. Use a scratch name for drills.
#   --yes-i-understand-this-overwrites   REQUIRED. Without it, this is a no-op.
#   --no-clean         Skip pg_restore --clean (use for a FRESH/empty target).
#   -h, --help         Show this help.
#
# Connection: set PGHOST/PGPORT/PGUSER/PGPASSWORD (from your secret), or
# DATABASE_URL for the default-target case. Never hardcode credentials.
set -euo pipefail

FILE=""
TARGET_DB="${TARGET_DB:-}"
CONFIRMED=0
CLEAN=1

usage() {
	cat <<'EOF'
pg_restore.sh — restore a Lens backup into a target database (DESTRUCTIVE).

Usage:
  pg_restore.sh --file FILE --yes-i-understand-this-overwrites [--target-db NAME] [--no-clean]

  --file FILE        Backup file (.dump or .dump.gz). REQUIRED.
  --target-db NAME   Database to restore INTO (default: DATABASE_URL's db).
  --yes-i-understand-this-overwrites   REQUIRED confirmation. Without it: no-op.
  --no-clean         Skip pg_restore --clean (for a FRESH/empty target).
  -h, --help         Show this help.

Connection: PGHOST/PGPORT/PGUSER/PGPASSWORD, or DATABASE_URL for the default target.
EOF
}
log() { printf '%s [pg_restore] %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" >&2; }
die() { log "ERROR: $*"; exit 1; }

while [ $# -gt 0 ]; do
	case "$1" in
		--file) FILE="${2:-}"; shift 2 ;;
		--target-db) TARGET_DB="${2:-}"; shift 2 ;;
		--yes-i-understand-this-overwrites) CONFIRMED=1; shift ;;
		--no-clean) CLEAN=0; shift ;;
		-h|--help) usage; exit 0 ;;
		*) die "unknown argument: $1 (try --help)" ;;
	esac
done

[ -n "$FILE" ] || die "--file is required (try --help)"
[ -f "$FILE" ] || die "backup file not found: $FILE"
command -v pg_restore >/dev/null 2>&1 || die "pg_restore not found on PATH"

# Resolve the target connection. Prefer an explicit --target-db (drills,
# restoring beside prod); otherwise fall back to DATABASE_URL as-is.
if [ -n "$TARGET_DB" ]; then
	conn=(--dbname="$TARGET_DB")
	target_desc="$TARGET_DB (via PG* env)"
elif [ -n "${DATABASE_URL:-}" ]; then
	conn=(--dbname="$DATABASE_URL")
	target_desc="the database in DATABASE_URL"
else
	die "no target: pass --target-db NAME (with PG* env) or set DATABASE_URL"
fi

if [ "$CONFIRMED" -ne 1 ]; then
	die "refusing to restore into $target_desc without --yes-i-understand-this-overwrites"
fi

log "restoring $FILE -> $target_desc"

restore_args=(--no-owner --no-privileges --exit-on-error)
if [ "$CLEAN" -eq 1 ]; then
	# --clean --if-exists drops existing objects first so an overwrite is clean.
	restore_args+=(--clean --if-exists)
fi

# Decompress .gz on the fly; pg_restore reads the custom-format dump from stdin.
case "$FILE" in
	*.gz)
		command -v gunzip >/dev/null 2>&1 || die "gunzip not found on PATH"
		gunzip -c "$FILE" | pg_restore "${conn[@]}" "${restore_args[@]}" || die "restore failed"
		;;
	*)
		pg_restore "${conn[@]}" "${restore_args[@]}" "$FILE" || die "restore failed"
		;;
esac

log "restore complete -> $target_desc"

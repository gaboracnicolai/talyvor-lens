#!/usr/bin/env bash
#
# local_drill.sh — prove the backup→restore→verify round-trip works LOCALLY,
# end to end, with zero production access. Spins up a throwaway Postgres
# container, seeds minimal Lens-shaped data, runs pg_backup.sh, then
# backup_verify.sh (which restores into a scratch DB and sanity-checks it).
#
# This is the one piece of the DR machinery that can be self-tested without
# real infrastructure. It does NOT validate a real production restore — see
# deploy/backup/DR-RUNBOOK.md "BACKUP IS NOT VALIDATED UNTIL RESTORED".
#
# Requires: docker. Uses the Debian postgres:16 image (has bash + pg client
# tools), matching the chart's default backup image.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
CONTAINER="${DRILL_CONTAINER:-lens-backup-drill}"
IMAGE="${DRILL_IMAGE:-postgres:16}"
PW="drill-not-a-secret"

log() { printf '== %s\n' "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

command -v docker >/dev/null 2>&1 || die "docker not found — cannot run the local drill"

cleanup() { docker rm -f "$CONTAINER" >/dev/null 2>&1 || true; }
trap cleanup EXIT

docker rm -f "$CONTAINER" >/dev/null 2>&1 || true

log "starting throwaway Postgres ($IMAGE)"
docker run -d --name "$CONTAINER" \
	-e POSTGRES_PASSWORD="$PW" -e POSTGRES_DB=lens \
	-v "$SCRIPT_DIR":/scripts:ro \
	"$IMAGE" >/dev/null || die "failed to start container"

log "waiting for Postgres to accept connections"
ready=0
for _ in $(seq 1 30); do
	if docker exec "$CONTAINER" pg_isready -U postgres >/dev/null 2>&1; then ready=1; break; fi
	sleep 1
done
[ "$ready" -eq 1 ] || die "Postgres did not become ready in time"

log "seeding minimal Lens-shaped data (token-ledger + balances)"
docker exec "$CONTAINER" psql -U postgres -d lens -v ON_ERROR_STOP=1 -c "
  CREATE TABLE lens_token_balances (workspace_id TEXT PRIMARY KEY, balance DOUBLE PRECISION NOT NULL DEFAULT 0);
  CREATE TABLE lens_token_ledger (id BIGSERIAL PRIMARY KEY, workspace_id TEXT NOT NULL, amount DOUBLE PRECISION NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT now());
  INSERT INTO lens_token_balances (workspace_id, balance) VALUES ('ws-demo', 8);
  INSERT INTO lens_token_ledger (workspace_id, amount) VALUES ('ws-demo', 10), ('ws-demo', -2);
" >/dev/null || die "seed failed"

log "running pg_backup.sh inside the container"
docker exec \
	-e DATABASE_URL="postgresql://postgres:${PW}@localhost:5432/lens" \
	-e BACKUP_DIR=/backups \
	"$CONTAINER" /scripts/pg_backup.sh || die "pg_backup.sh failed"

log "running backup_verify.sh inside the container (restore drill into scratch DB)"
docker exec \
	-e PGHOST=localhost -e PGPORT=5432 -e PGUSER=postgres -e PGPASSWORD="$PW" \
	-e BACKUP_DIR=/backups -e MAINT_DB=postgres \
	"$CONTAINER" /scripts/backup_verify.sh || die "backup_verify.sh reported FAIL"

log "LOCAL DRILL: PASS — backup produced, restored into a scratch DB, sanity checks passed"

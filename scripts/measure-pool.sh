#!/usr/bin/env bash
# shellcheck disable=SC2016 # the single-quoted $-expressions expand on the remote host, on purpose
# measure-pool.sh — re-runs the B9.1 measurement in docs/pool-b91-measured.md.
#
#   scripts/measure-pool.sh <ssh-target>          e.g. root@<prod-host>
#
# Three parts, each READ-ONLY against production:
#   1. the live pooling configuration, read from the running lens container (keys shown as set/unset);
#   2. production counts, in a read-only Postgres transaction (default_transaction_read_only=on);
#   3. the committed poolsafety corpora scored by cmd/hitrate with production's embedding key and
#      model, in a one-off container that inherits the lens service's environment and touches no DB.
# Then, if LENS_TEST_DATABASE_URL is set locally, the end-to-end exact-lane tests (real Postgres).
#
# Nothing here writes to production. `lens poolcheck` is deliberately NOT run: it rewrites the
# pool_safety_attestation row, which is a gate.
set -euo pipefail

target="${1:?usage: scripts/measure-pool.sh <ssh-target>}"
here="$(cd "$(dirname "$0")/.." && pwd)"
remote() { ssh -o ConnectTimeout=10 "$target" "cd ~/talyvor-lens && $1"; }
sql() {
  remote "docker compose exec -T -e PGOPTIONS='-c default_transaction_read_only=on' postgres \
    psql -U lens -d talyvor_lens -v ON_ERROR_STOP=1 -At -F ' | '"
}

echo "═══ 1. live pooling configuration (lens container) ═══"
remote 'docker compose exec -T lens sh -c "for v in LENS_CACHE_POOLABLE_ENABLED LENS_ECONOMY_ENABLED \
  LENS_SEMANTIC_THRESHOLD LENS_EMBEDDING_MODEL LENS_POOL_CONSUMER_DISCOUNT LENS_POOL_ROYALTY_SHARE \
  LENS_POOL_ROYALTY_MINTING_ENABLED LENS_POOL_SHADOW_LOG_ENABLED LENS_SEMANTIC_CACHE_RETENTION \
  LENS_MAX_CACHE_TTL LENS_OPENAI_API_KEY; do eval x=\\\${\$v-unset}; case \$v in *KEY) [ \"\$x\" = unset ] || x=set;; esac; \
  echo \"  \$v=\$x\"; done"'
remote 'docker compose logs lens 2>&1 | grep -E "POOLING (ENABLED|DISABLED)|cross-tenant pooled hits|Pool-B royalty" | tail -3 | cut -c1-220' || true

echo
echo "═══ 2. production counts (read-only transaction) ═══"
sql <<'SQL'
SELECT 'workspaces: total | cache_poolable', count(*), count(*) FILTER (WHERE cache_poolable) FROM workspaces;
SELECT 'semantic pool (prompt_embeddings): poolable | private', count(*) FILTER (WHERE is_poolable), count(*) FILTER (WHERE NOT is_poolable) FROM prompt_embeddings;
SELECT 'serves by source: ' || serve_source, count(*), count(DISTINCT workspace_id) FROM token_events GROUP BY serve_source ORDER BY 1;
SELECT 'pooled serves all time (exact | semantic)', count(*) FILTER (WHERE serve_source = 'cache_hit_pooled'), count(*) FILTER (WHERE serve_source = 'cache_hit_pooled_semantic') FROM token_events;
SELECT 'pooled charges to the asker: rows | µLXC charged | µLXC saved', count(*), COALESCE(sum(-amount), 0), COALESCE(sum((metadata->>'pool_saved_ulxc')::numeric), 0) FROM lxc_ledger WHERE metadata ? 'pool_saved_ulxc';
SELECT 'royalty claims: ' || status, count(*), sum(minted_amount) FROM pool_royalty_mints GROUP BY status;
SELECT 'royalty ledger: ' || type, count(*), sum(amount) FROM lens_token_ledger WHERE type LIKE 'pool_royalty%' GROUP BY type;
SELECT 'would-have-pooled log (pooled_shadow_observations)', count(*) FROM pooled_shadow_observations;
SELECT 'attestation: model | threshold | worst pair | checked', embedding_model, threshold, worst_pair, checked_at FROM pool_safety_attestation;
SQL
echo "  exact pool (Redis keys lens:exact:*):"
remote 'docker compose exec -T redis redis-cli --scan --pattern "lens:exact:*" | grep -vc ":owner$"' || true

echo
echo "═══ 3. committed corpora at production's configuration (cmd/hitrate) ═══"
bin="$(mktemp)"
(cd "$here" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$bin" ./cmd/hitrate)
scp -q "$bin" "$target:/tmp/hitrate-measure-pool"
rm -f "$bin"
model="$(remote 'docker compose exec -T lens sh -c "echo \${LENS_EMBEDDING_MODEL:-text-embedding-3-small}"')"
remote "docker compose run --rm --no-deps -T -e LENS_EMBEDDING_MODEL=$model \
  -v /tmp/hitrate-measure-pool:/hitrate:ro --entrypoint /hitrate lens; rm -f /tmp/hitrate-measure-pool"

if [ -n "${LENS_TEST_DATABASE_URL:-}" ]; then
  echo
  echo "═══ 4. identical prompt from a second workspace, end to end (real Postgres, local) ═══"
  (cd "$here" && CGO_ENABLED=0 go test ./internal/proxy/ -run 'TestPoolB91' -count=1 -v 2>&1 \
    | grep -E '^(--- |ok|FAIL)|B9.1 exact lane')
fi

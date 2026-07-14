package mining

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestLedgerMetadata_JSONBAcrossExecModes_Integration is the #133 guard.
//
// A ledger write persists a jsonb `metadata` column. Under the SimpleProtocol
// that LENS_DB_PGBOUNCER=true forces (PgBouncer transaction pooling can't use
// persistent prepared statements), pgx infers the wire type of a []byte
// parameter as bytea — there is no server Describe to learn the column is jsonb
// — and Postgres rejects the hex-encoded bytea with 22P02. The reproduction
// probe proved a $N::jsonb cast does NOT rescue it (the cast applies to the
// already-wrong hex text) while a text-encoded value succeeds on BOTH
// protocols. This test pins that: the same Credit must land under simple
// protocol AND under the extended/direct default, so the fix can't regress the
// direct-connection topology.
//
// On the unfixed tree the simple_protocol subtest FAILS with 22P02.
func TestLedgerMetadata_JSONBAcrossExecModes_Integration(t *testing.T) {
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG jsonb protocol guard")
	}
	cases := []struct {
		name string
		mode pgx.QueryExecMode
	}{
		{"simple_protocol_pgbouncer", pgx.QueryExecModeSimpleProtocol},
		{"extended_protocol_direct", pgx.QueryExecModeCacheStatement},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			cfg, err := pgxpool.ParseConfig(url)
			if err != nil {
				t.Fatal(err)
			}
			cfg.ConnConfig.DefaultQueryExecMode = tc.mode
			pool, err := pgxpool.NewWithConfig(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()

			for _, ddl := range []string{
				`DROP TABLE IF EXISTS lens_token_ledger`,
				`DROP TABLE IF EXISTS lens_token_balances`,
				`CREATE TABLE lens_token_balances (workspace_id TEXT PRIMARY KEY,
					balance BIGINT NOT NULL DEFAULT 0,
					lifetime_earned BIGINT NOT NULL DEFAULT 0,
					lifetime_spent BIGINT NOT NULL DEFAULT 0,
					updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`, // BIGINT µLENS (prod 0082)
				// metadata NOT NULL DEFAULT '{}'::jsonb — the production shape
				// (0019/0034). A NULL would violate it, so the fix must emit
				// valid JSON text, never SQL NULL, for empty metadata.
				`CREATE TABLE lens_token_ledger (id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
					workspace_id TEXT NOT NULL, amount BIGINT NOT NULL,
					balance_after BIGINT NOT NULL, type TEXT NOT NULL, description TEXT,
					metadata JSONB NOT NULL DEFAULT '{}'::jsonb, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
			} {
				if _, err := pool.Exec(ctx, ddl); err != nil {
					t.Fatalf("schema: %v", err)
				}
			}

			store := NewLedgerStore(pool)
			meta := map[string]interface{}{"request_workspace_id": "ws_src", "rarity": 0.25}
			if err := store.Credit(ctx, "ws_jsonb", 1.0, "pattern_mine", "pattern shared", meta); err != nil {
				t.Fatalf("Credit with jsonb metadata under %s exec mode: %v", tc.name, err)
			}

			// The populated jsonb round-trips (proves it landed as JSON, not bytea).
			var gotWS string
			if err := pool.QueryRow(ctx,
				`SELECT metadata->>'request_workspace_id' FROM lens_token_ledger WHERE workspace_id='ws_jsonb'`,
			).Scan(&gotWS); err != nil {
				t.Fatalf("read back jsonb metadata under %s: %v", tc.name, err)
			}
			if gotWS != "ws_src" {
				t.Fatalf("jsonb metadata not persisted under %s: want ws_src, got %q", tc.name, gotWS)
			}

			// Empty metadata must also work and yield a non-null JSON value
			// (the column is NOT NULL).
			if err := store.Credit(ctx, "ws_empty", 1.0, "pattern_mine", "no meta", nil); err != nil {
				t.Fatalf("Credit with nil metadata under %s: %v", tc.name, err)
			}
			var gotEmpty string
			if err := pool.QueryRow(ctx,
				`SELECT metadata::text FROM lens_token_ledger WHERE workspace_id='ws_empty'`,
			).Scan(&gotEmpty); err != nil {
				t.Fatalf("read back empty metadata under %s: %v", tc.name, err)
			}
			if gotEmpty != "{}" {
				t.Fatalf("nil metadata should persist as {} under %s, got %q", tc.name, gotEmpty)
			}
		})
	}
}

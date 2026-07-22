package alerts

import (
	"context"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/migrations"
)

var cacheServeMigrateOnce sync.Once

// cacheServePool migrates the REAL schema (token_events is built across its migrations up to
// 0100_token_events_serve_source) into a private schema and returns a pool pinned to it — the
// same idiom as the other LENS_TEST_DATABASE_URL-gated integration tests.
func cacheServePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG cache-serve spend test")
	}
	const schema = "cache_serve_realpg"
	ctx := context.Background()
	cacheServeMigrateOnce.Do(func() {
		cfg, err := pgx.ParseConfig(url)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		cfg.RuntimeParams["search_path"] = schema + ",public"
		conn, err := pgx.ConnectConfig(ctx, cfg)
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		defer conn.Close(ctx)
		for _, ddl := range []string{`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`, `CREATE SCHEMA ` + schema} {
			if _, err := conn.Exec(ctx, ddl); err != nil {
				t.Fatalf("reset schema: %v", err)
			}
		}
		if _, err := dbmigrate.Run(ctx, conn, migrations.FS); err != nil {
			t.Fatalf("migrate: %v", err)
		}
	})
	poolCfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("pool cfg: %v", err)
	}
	poolCfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// tokenEventRow is the field set these tests assert on — the row, not a status code.
type tokenEventRow struct {
	serveSource   string
	costUSD       float64
	costEstimated bool
	inputTokens   int
	outputTokens  int
	provider      string
	promptText    string
	distillMethod string
	sessionID     string
}

func fetchRowByRequestID(t *testing.T, pool *pgxpool.Pool, requestID string) tokenEventRow {
	t.Helper()
	var row tokenEventRow
	err := pool.QueryRow(context.Background(),
		`SELECT serve_source, cost_usd, cost_estimated, input_tokens, output_tokens,
		        provider, prompt_text, distill_method, session_id
		 FROM token_events WHERE request_id = $1`, requestID).
		Scan(&row.serveSource, &row.costUSD, &row.costEstimated, &row.inputTokens, &row.outputTokens,
			&row.provider, &row.promptText, &row.distillMethod, &row.sessionID)
	if err != nil {
		t.Fatalf("fetch token_events row for request_id=%s: %v", requestID, err)
	}
	return row
}

// A cache-served request must produce a token_events row that is unmistakably a cache hit:
// serve_source carries the layer, cost_usd is EXACTLY zero (Talyvor's provider cost — the
// requester's LXC debit is a different ledger and is NOT represented here), the tokens Lens
// measured are persisted, and the cost is flagged estimated (no provider usage exists).
func TestRecordCacheServe_RealPG_WritesZeroCostTaggedRow(t *testing.T) {
	pool := cacheServePool(t)
	m := New(pool, nil, nil)

	err := m.RecordCacheServe(context.Background(),
		"ws-cv", "team-cv", "sprint-cv", "chat", "claude-sonnet-4-6",
		100, 50, "sess-cv-1", "req-cv-hit-1", "text", "cache_hit_semantic")
	if err != nil {
		t.Fatalf("RecordCacheServe: %v", err)
	}

	row := fetchRowByRequestID(t, pool, "req-cv-hit-1")
	if row.serveSource != "cache_hit_semantic" {
		t.Errorf("serve_source = %q, want cache_hit_semantic", row.serveSource)
	}
	if row.costUSD != 0 {
		t.Errorf("cost_usd = %v, want exactly 0 (Talyvor bought nothing upstream)", row.costUSD)
	}
	if !row.costEstimated {
		t.Error("cost_estimated = false, want true (length-derived tokens, no provider usage)")
	}
	if row.inputTokens != 100 || row.outputTokens != 50 {
		t.Errorf("tokens = %d/%d, want 100/50", row.inputTokens, row.outputTokens)
	}
	if row.provider != "anthropic" {
		t.Errorf("provider = %q, want anthropic (derived from the model as on every row)", row.provider)
	}
	if row.promptText != "" {
		t.Errorf("prompt_text = %q, want empty (cache rows persist no prompt — streaming precedent)", row.promptText)
	}
	if row.distillMethod != "" {
		t.Errorf("distill_method = %q, want empty", row.distillMethod)
	}
	if row.sessionID != "sess-cv-1" {
		t.Errorf("session_id = %q, want sess-cv-1", row.sessionID)
	}
}

// The serve_source vocabulary is enforced in the schema (0100 CHECK) so a typo'd layer label can
// never silently create an uncountable category three months from now.
func TestRecordCacheServe_RealPG_RejectsUnknownServeSource(t *testing.T) {
	pool := cacheServePool(t)
	m := New(pool, nil, nil)

	err := m.RecordCacheServe(context.Background(),
		"ws-cv", "", "", "chat", "claude-sonnet-4-6",
		10, 5, "", "req-cv-bogus-1", "text", "cache_hit_bogus")
	if err == nil {
		t.Fatal("RecordCacheServe accepted serve_source=cache_hit_bogus — the 0100 CHECK constraint must reject it")
	}
}

// MONEY-DIFF DISCIPLINE: the upstream write path must be byte-unchanged by 0100. A RecordSpend row
// gets serve_source='upstream' purely via the column DEFAULT, and its cost_usd is the exact
// catalog price it was before this change (claude-sonnet-4-6: (100×3.00 + 50×15.00)/1e6 = 0.00105).
func TestRecordSpend_RealPG_UpstreamDefaultAndCostUnchanged(t *testing.T) {
	pool := cacheServePool(t)
	m := New(pool, nil, nil)

	err := m.RecordSpend(context.Background(),
		"ws-cv", "team-cv", "sprint-cv", "chat", "claude-sonnet-4-6",
		100, 50, "prompt text", "sess-cv-2", "req-cv-miss-1", "text", false)
	if err != nil {
		t.Fatalf("RecordSpend: %v", err)
	}

	row := fetchRowByRequestID(t, pool, "req-cv-miss-1")
	if row.serveSource != "upstream" {
		t.Errorf("serve_source = %q, want upstream (the DEFAULT — the 15-column INSERT is untouched)", row.serveSource)
	}
	const wantCost = 0.00105
	if diff := row.costUSD - wantCost; diff > 1e-12 || diff < -1e-12 {
		t.Errorf("cost_usd = %v, want exactly %v — the upstream money write must be unchanged", row.costUSD, wantCost)
	}
	if row.costEstimated {
		t.Error("cost_estimated = true, want false (passed through unchanged)")
	}
}

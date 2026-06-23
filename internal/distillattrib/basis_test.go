package distillattrib

import (
	"context"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/alerts"
)

// TestRecordRoyaltyBasis_InsertShape pins the basis write: a once-per-relationship
// INSERT ... ON CONFLICT DO NOTHING into distill_royalty_basis — and MINT-FREE, the
// SQL can never reference a ledger/mint/credit table.
func TestRecordRoyaltyBasis_InsertShape(t *testing.T) {
	fe := &fakeExecer{}
	s := NewStore(fe)
	if err := s.RecordRoyaltyBasis(context.Background(), "wsA", "wsB", "h1", 0.0001, "gpt-4o-mini", 500, 20); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fe.sql, "INSERT INTO distill_royalty_basis") {
		t.Fatalf("SQL = %q, want INSERT INTO distill_royalty_basis", fe.sql)
	}
	// Capture-once: ON CONFLICT ... DO NOTHING (never an upsert-latest that would
	// change PR3's mint amount on a re-serve).
	if !strings.Contains(fe.sql, "ON CONFLICT") || !strings.Contains(strings.ToUpper(fe.sql), "DO NOTHING") {
		t.Fatalf("SQL missing ON CONFLICT ... DO NOTHING (capture-once):\n%s", fe.sql)
	}
	// MINT-FREE (structural): never a ledger / spend / mint / held / LXC / credit ref.
	for _, banned := range []string{"lens_token_ledger", "token_events", "pool_royalty", "lxc_", "credit", "held_balance"} {
		if strings.Contains(strings.ToLower(fe.sql), banned) {
			t.Fatalf("MINT-FREE VIOLATION: basis SQL references %q:\n%s", banned, fe.sql)
		}
	}
	if len(fe.args) != 7 {
		t.Fatalf("args = %v, want 7 (owner, requester, hash, cogs, model, in, out)", fe.args)
	}
}

// TestRecordRoyaltyBasis_NilInert — a nil store is a no-op (basis inert until a real
// pool is wired).
func TestRecordRoyaltyBasis_NilInert(t *testing.T) {
	if err := (*Store)(nil).RecordRoyaltyBasis(context.Background(), "a", "b", "h", 1, "m", 1, 1); err != nil {
		t.Fatalf("nil store must no-op, got %v", err)
	}
}

// TestDistillAttrib_NoLedgerImport — STRUCTURAL: the package's PRODUCTION source
// imports NO ledger/mint package, so RecordRoyaltyBasis (a money FIGURE) provably
// cannot mint. Parses every non-test .go file and fails on any banned import.
func TestDistillAttrib_NoLedgerImport(t *testing.T) {
	fset := token.NewFileSet()
	banned := []string{"internal/mining", "internal/poolroyalty", "internal/economy", "internal/ledger"}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imp := range f.Imports {
			for _, b := range banned {
				if strings.Contains(imp.Path.Value, b) {
					t.Errorf("%s imports a ledger/mint package %s — distillattrib must stay mint-free", name, imp.Path.Value)
				}
			}
		}
	}
}

// TestRecordRoyaltyBasis_ValueProvenancePinned_Integration — the money-basis proof
// against real Postgres: a recorded basis stores avoided_cogs_usd + its provenance
// (model + token split) FAITHFULLY (recomputing CostUSD(stored) yields the stored
// figure), and is PINNED once-per-relationship — a re-serve with a different
// model/cost does NOT overwrite it (ON CONFLICT DO NOTHING, not upsert-latest, which
// matters because an overwrite would change PR3's mint amount on re-serve). Gated on
// LENS_TEST_DATABASE_URL.
func TestRecordRoyaltyBasis_ValueProvenancePinned_Integration(t *testing.T) {
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set LENS_TEST_DATABASE_URL for the real-PG royalty-basis test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("pgxpool: %v", err)
	}
	t.Cleanup(pool.Close)
	// Mirror migration 0061 on a clean table.
	for _, ddl := range []string{
		`DROP TABLE IF EXISTS distill_royalty_basis`,
		`CREATE TABLE distill_royalty_basis (
			owner_workspace_id TEXT NOT NULL, requester_workspace_id TEXT NOT NULL,
			content_hash TEXT NOT NULL, avoided_cogs_usd DOUBLE PRECISION NOT NULL,
			vision_model TEXT NOT NULL, vision_input_tokens INTEGER NOT NULL,
			vision_output_tokens INTEGER NOT NULL,
			captured_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (owner_workspace_id, requester_workspace_id, content_hash))`,
	} {
		if _, err := pool.Exec(ctx, ddl); err != nil {
			t.Fatalf("ddl: %v", err)
		}
	}
	s := NewStore(pool)

	const model1, in1, out1 = "gpt-4o-mini", 500, 20
	cogs1 := alerts.CostUSD(model1, in1, out1)
	if cogs1 <= 0 {
		t.Fatalf("precondition: CostUSD(%s,%d,%d) must be > 0 for a non-vacuous proof, got %v", model1, in1, out1, cogs1)
	}
	if err := s.RecordRoyaltyBasis(ctx, "wsA", "wsB", "h1", cogs1, model1, in1, out1); err != nil {
		t.Fatal(err)
	}

	var gotCogs float64
	var gotModel string
	var gotIn, gotOut int
	read := func() {
		if err := pool.QueryRow(ctx,
			`SELECT avoided_cogs_usd, vision_model, vision_input_tokens, vision_output_tokens
			 FROM distill_royalty_basis
			 WHERE owner_workspace_id='wsA' AND requester_workspace_id='wsB' AND content_hash='h1'`).
			Scan(&gotCogs, &gotModel, &gotIn, &gotOut); err != nil {
			t.Fatal(err)
		}
	}

	// Value + provenance FAITHFUL: each column EXACTLY, and the figure is re-derivable.
	read()
	if gotModel != model1 || gotIn != in1 || gotOut != out1 {
		t.Fatalf("provenance not faithful: got (%s,%d,%d), want (%s,%d,%d)", gotModel, gotIn, gotOut, model1, in1, out1)
	}
	if gotCogs != cogs1 {
		t.Fatalf("avoided_cogs_usd = %v, want exactly %v", gotCogs, cogs1)
	}
	if rederived := alerts.CostUSD(gotModel, gotIn, gotOut); rederived != gotCogs {
		t.Fatalf("stored basis not re-derivable: CostUSD(stored model,in,out) = %v != stored %v", rederived, gotCogs)
	}

	// PINNED: re-serve the SAME relationship with a DIFFERENT model/cost → UNCHANGED.
	const model2 = "claude-3-5-sonnet"
	cogs2 := alerts.CostUSD(model2, 9999, 9999)
	if cogs2 == cogs1 {
		t.Fatal("test setup: the second basis must differ from the first")
	}
	if err := s.RecordRoyaltyBasis(ctx, "wsA", "wsB", "h1", cogs2, model2, 9999, 9999); err != nil {
		t.Fatal(err)
	}
	read()
	if gotModel != model1 || gotIn != in1 || gotOut != out1 || gotCogs != cogs1 {
		t.Fatalf("basis was OVERWRITTEN on re-serve (must be pinned): got (%s,%d,%d,%v), want original (%s,%d,%d,%v)",
			gotModel, gotIn, gotOut, gotCogs, model1, in1, out1, cogs1)
	}
	var rows int64
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM distill_royalty_basis`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("a re-serve must not append: rows=%d, want 1", rows)
	}

	// A different requester is a SEPARATE relationship → a new row.
	if err := s.RecordRoyaltyBasis(ctx, "wsA", "wsC", "h1", cogs1, model1, in1, out1); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM distill_royalty_basis`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatalf("a new requester must be a separate row: rows=%d, want 2", rows)
	}
}

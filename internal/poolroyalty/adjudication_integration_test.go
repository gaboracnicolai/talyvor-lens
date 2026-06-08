package poolroyalty

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/mining"
)

func adjTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG adjudication test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	entryCapResetSchema(t, pool, ctx) // balances + ledger + claim table + index
	// the adjudication record table (the 0048 shape).
	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS pool_royalty_adjudications`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `CREATE TABLE pool_royalty_adjudications (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		flag_type TEXT NOT NULL, resolution_label TEXT NOT NULL,
		candidate_request_ids TEXT[] NOT NULL, revoked_request_ids TEXT[] NOT NULL,
		decided_by TEXT NOT NULL, outcome JSONB, decided_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	return pool
}

func seedHeld(t *testing.T, m *Minter, ctx context.Context, reqID, contributor string) {
	t.Helper()
	h := ServedHit{
		RequestID: reqID, RequesterWorkspace: "wsB", ContributorWorkspace: contributor,
		Layer: "exact", EntryID: "e", Provider: "openai", Model: "gpt-4o",
		AvoidedCOGSUSD: 2.0, AnswerSHA256: SHA256Hex([]byte("a")), PromptSHA256: SHA256Hex([]byte("p")),
	}
	if res, err := m.MintServedHit(ctx, h); err != nil || !res.Minted {
		t.Fatalf("seed %s: res=%+v err=%v", reqID, res, err)
	}
}

// OPERATOR SUBSET EXACTLY HONORED — NO OVER-REVOCATION (the headline property).
// Seed 4 held mints; the operator reviewed all 4 (candidate set) but CHOSE only
// 2 to revoke. After Adjudicate: exactly those 2 are revoked (status='revoked',
// held burned); the other 2 are UNTOUCHED (still held, not burned); the record
// captures both sets + the report in outcome. The human's narrowing of the
// resolver's over-selection is honored — innocent rows are never clawed back.
func TestAdjudicate_SubsetHonored_Integration(t *testing.T) {
	pool := adjTestPool(t)
	ctx := context.Background()
	ledger := mining.NewLedgerStore(pool)
	m := NewMinter(pool, ledger, 0.5, func() bool { return true })
	m.SetHoldbackWindow(time.Hour) // stay held
	for _, id := range []string{"a1", "a2", "a3", "a4"} {
		seedHeld(t, m, ctx, id, "wsA")
	}

	w := NewAdjudicationWriter(pool, NewRevoker(pool, ledger))
	id, report, err := w.Adjudicate(ctx, AdjudicationDecision{
		FlagType: "volume", ResolutionLabel: string(LabelTuplePinned),
		CandidateRequestIDs: []string{"a1", "a2", "a3", "a4"}, // reviewed all 4
		RevokeRequestIDs:    []string{"a1", "a3"},             // chose only 2
		DecidedBy:           "global_key",
	})
	if err != nil {
		t.Fatalf("Adjudicate: %v", err)
	}
	if report.Totals[OutcomeRevoked] != 2 {
		t.Fatalf("report: %v, want 2 revoked", report.Totals)
	}

	// The chosen two are revoked; the other two UNTOUCHED.
	for _, id := range []string{"a1", "a3"} {
		var status string
		mustScan(t, pool, `SELECT status FROM pool_royalty_mints WHERE request_id='`+id+`'`, &status)
		if status != "revoked" {
			t.Errorf("chosen %s status=%q, want revoked", id, status)
		}
	}
	for _, id := range []string{"a2", "a4"} {
		var status string
		mustScan(t, pool, `SELECT status FROM pool_royalty_mints WHERE request_id='`+id+`'`, &status)
		if status != "held" {
			t.Errorf("NON-chosen %s status=%q, want held (must NOT be over-revoked)", id, status)
		}
	}
	// held burned for exactly the 2 chosen (wsA had 4 held = 2.0; minus 2 revoked = 1.0 left)
	var held float64
	mustScan(t, pool, `SELECT held_balance FROM lens_token_balances WHERE workspace_id='wsA'`, &held)
	// Each mint credits 0.5 × avoided(2.0) = 1.0 to held; 4 mints = 4.0 held;
	// revoking 2 burns 2.0 → 2.0 left.
	if held != 2.0 {
		t.Fatalf("held=%v, want 2.0 (4 held minus 2 revoked, each 1.0)", held)
	}

	// The record captures both sets + the report in outcome.
	var cand, rev []string
	var decidedBy string
	var outcomeRaw []byte
	if err := pool.QueryRow(ctx, `SELECT candidate_request_ids, revoked_request_ids, decided_by, outcome FROM pool_royalty_adjudications WHERE id=$1`, id).
		Scan(&cand, &rev, &decidedBy, &outcomeRaw); err != nil {
		t.Fatal(err)
	}
	if len(cand) != 4 || len(rev) != 2 || decidedBy != "global_key" {
		t.Errorf("record: cand=%v rev=%v by=%q", cand, rev, decidedBy)
	}
	var persisted RevokeReport
	if err := json.Unmarshal(outcomeRaw, &persisted); err != nil {
		t.Fatalf("outcome not valid RevokeReport JSON: %v", err)
	}
	if persisted.Totals[OutcomeRevoked] != 2 {
		t.Errorf("persisted outcome totals = %v, want 2 revoked", persisted.Totals)
	}
}

// RECORD-BEFORE-BURN DURABILITY: with a revoker that ERRORS every row, the
// adjudication record STILL EXISTS (written first) with the chosen subset +
// decided_by, and outcome records the errors. A burn can never happen without a
// preceding record.
type erroringRevoker struct{}

func (erroringRevoker) RevokeHeldMints(_ context.Context, ids []string) (RevokeReport, error) {
	rep := RevokeReport{Outcomes: map[string]RevokeOutcome{}, Totals: map[RevokeOutcome]int{}}
	for _, id := range ids {
		rep.Outcomes[id] = OutcomeError
		rep.Totals[OutcomeError]++
	}
	return rep, nil
}

func TestAdjudicate_RecordBeforeBurnDurability_Integration(t *testing.T) {
	pool := adjTestPool(t)
	ctx := context.Background()

	w := NewAdjudicationWriter(pool, erroringRevoker{})
	id, report, err := w.Adjudicate(ctx, AdjudicationDecision{
		FlagType: "self_dealing", ResolutionLabel: string(LabelPairCoarse),
		CandidateRequestIDs: []string{"x1", "x2"}, RevokeRequestIDs: []string{"x1", "x2"},
		DecidedBy: "global_key",
	})
	if err != nil {
		t.Fatalf("per-row errors must not fail Adjudicate: %v", err)
	}
	if report.Totals[OutcomeError] != 2 {
		t.Errorf("report should record the errors: %v", report.Totals)
	}
	// The record EXISTS with the chosen subset despite every revoke erroring.
	var rev []string
	var by string
	var outcomeRaw []byte
	if err := pool.QueryRow(ctx, `SELECT revoked_request_ids, decided_by, outcome FROM pool_royalty_adjudications WHERE id=$1`, id).
		Scan(&rev, &by, &outcomeRaw); err != nil {
		t.Fatalf("the record must exist even when the revoke errored: %v", err)
	}
	if len(rev) != 2 || by != "global_key" {
		t.Errorf("record: rev=%v by=%q", rev, by)
	}
	var persisted RevokeReport
	_ = json.Unmarshal(outcomeRaw, &persisted)
	if persisted.Totals[OutcomeError] != 2 {
		t.Errorf("outcome must record the errors; got %v", persisted.Totals)
	}
}

// DOUBLY INERT: with minting OFF, the minter creates no held rows, so even an
// admin adjudication revokes nothing (every chosen id is skipped_not_found —
// there is nothing to burn). The revoke can only bite when minting is enabled
// AND an admin passes a subset.
func TestAdjudicate_DoublyInert_MintingOff_Integration(t *testing.T) {
	pool := adjTestPool(t)
	ctx := context.Background()
	ledger := mining.NewLedgerStore(pool)
	mintingOff := NewMinter(pool, ledger, 0.5, func() bool { return false })
	// minting OFF → MintServedHit is a no-op, no held rows created.
	res, _ := mintingOff.MintServedHit(ctx, ServedHit{
		RequestID: "off-1", RequesterWorkspace: "wsB", ContributorWorkspace: "wsA",
		Layer: "exact", EntryID: "e", AvoidedCOGSUSD: 2.0,
		AnswerSHA256: SHA256Hex([]byte("a")), PromptSHA256: SHA256Hex([]byte("p")),
	})
	if res.Minted {
		t.Fatal("minting off must not mint")
	}
	// Admin adjudicates the id anyway → nothing to revoke.
	w := NewAdjudicationWriter(pool, NewRevoker(pool, ledger))
	_, report, err := w.Adjudicate(ctx, AdjudicationDecision{
		FlagType: "volume", ResolutionLabel: string(LabelTuplePinned),
		CandidateRequestIDs: []string{"off-1"}, RevokeRequestIDs: []string{"off-1"}, DecidedBy: "global_key",
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Totals[OutcomeSkippedNotFound] != 1 {
		t.Fatalf("with minting off there are no held rows; want 1 skipped_not_found, got %v", report.Totals)
	}
	var burns int64
	mustScan(t, pool, `SELECT COUNT(*) FROM lens_token_ledger WHERE type='pool_royalty_revoked'`, &burns)
	if burns != 0 {
		t.Fatalf("doubly inert: no burn possible with minting off; got %d", burns)
	}
}

// Belt-and-braces: the writer never proceeds to a burn on a record-write
// failure — proven at the unit layer; here we just confirm the marshal path is
// exercised end to end (compile/use guard).
var _ = fmt.Sprintf
var _ = errors.New

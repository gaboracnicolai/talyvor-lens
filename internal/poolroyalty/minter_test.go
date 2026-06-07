package poolroyalty

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
)

func newMockPool(t *testing.T) pgxmock.PgxPoolIface {
	t.Helper()
	pool, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// fakeLedger records CreditTx calls; it satisfies the minimal ledger seam the
// Minter needs (the real *mining.LedgerStore matches the same signature).
type creditCall struct {
	workspaceID string
	amount      float64
	txType      string
}

type fakeLedger struct {
	calls []creditCall
	err   error
}

func (f *fakeLedger) CreditTx(_ context.Context, _ pgx.Tx, workspaceID string, amount float64, txType, _ string, _ map[string]interface{}) error {
	f.calls = append(f.calls, creditCall{workspaceID: workspaceID, amount: amount, txType: txType})
	return f.err
}

func sampleHit() ServedHit {
	return ServedHit{
		RequestID:            "req-1",
		RequesterWorkspace:   "wsB",
		ContributorWorkspace: "wsA",
		Layer:                "exact",
		EntryID:              "lens:exact:deadbeef",
		Provider:             "openai",
		Model:                "gpt-4o",
		AvoidedCOGSUSD:       2.0,
		AnswerSHA256:         tAnswerHash,
		PromptSHA256:         tPromptHash,
	}
}

// Fixed 64-hex evidence hashes for pass-through assertions (the minter treats
// them opaquely; HASHING correctness is tested via SHA256Hex's known vectors).
const (
	tAnswerHash = "1111111111111111111111111111111111111111111111111111111111111111"
	tPromptHash = "2222222222222222222222222222222222222222222222222222222222222222"
)

func enabledOn() bool  { return true }
func enabledOff() bool { return false }

// EXACTLY-ONCE: the first mint for a request_id claims the row and credits the
// contributor in one transaction; a second attempt with the same request_id
// hits the UNIQUE conflict (RowsAffected 0), performs NO ledger credit, and
// reports AlreadyMinted — the povi_challenges claim/RowsAffected guard.
func TestMintServedHit_ExactlyOncePerRequestID(t *testing.T) {
	pool := newMockPool(t)
	ledger := &fakeLedger{}
	m := NewMinter(pool, ledger, 0.5, enabledOn)

	// First serve: claim row inserted → credit → commit.
	pool.ExpectBegin()
	pool.ExpectExec(`INSERT INTO pool_royalty_mints`).
		WithArgs("req-1", "wsB", "wsA", "exact", "lens:exact:deadbeef", "openai", "gpt-4o", 0.0, 2.0, 1.0, tAnswerHash, tPromptHash).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	pool.ExpectCommit()

	res, err := m.MintServedHit(context.Background(), sampleHit())
	if err != nil {
		t.Fatalf("first MintServedHit: %v", err)
	}
	if !res.Minted || res.AlreadyMinted {
		t.Errorf("first serve: Minted=%v AlreadyMinted=%v, want true/false", res.Minted, res.AlreadyMinted)
	}
	if res.Amount != 1.0 { // 0.5 × 2.0
		t.Errorf("minted amount = %v, want 1.0 (s × avoided_COGS)", res.Amount)
	}
	if len(ledger.calls) != 1 {
		t.Fatalf("ledger credits = %d, want 1", len(ledger.calls))
	}
	if c := ledger.calls[0]; c.workspaceID != "wsA" || c.amount != 1.0 || c.txType != TypePoolRoyalty {
		t.Errorf("credit = %+v, want wsA / 1.0 / %s", c, TypePoolRoyalty)
	}

	// Retry with the SAME request_id: UNIQUE conflict → no credit, no commit of
	// any ledger write. The claim insert wrote nothing, so the tx just ends.
	pool.ExpectBegin()
	pool.ExpectExec(`INSERT INTO pool_royalty_mints`).
		WithArgs("req-1", "wsB", "wsA", "exact", "lens:exact:deadbeef", "openai", "gpt-4o", 0.0, 2.0, 1.0, tAnswerHash, tPromptHash).
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	pool.ExpectRollback()

	res2, err := m.MintServedHit(context.Background(), sampleHit())
	if err != nil {
		t.Fatalf("retry MintServedHit: %v", err)
	}
	if res2.Minted || !res2.AlreadyMinted {
		t.Errorf("retry: Minted=%v AlreadyMinted=%v, want false/true", res2.Minted, res2.AlreadyMinted)
	}
	if len(ledger.calls) != 1 {
		t.Errorf("ledger credits after retry = %d, want still 1 (exactly once)", len(ledger.calls))
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// DEFLATIONARY FAILURE DIRECTION: a reused request_id — even from a DIFFERENT
// hit (different contributor/entry) — suppresses the later mint. Collisions
// can only under-mint, never inflate supply.
func TestMintServedHit_ReusedRequestID_SuppressesNeverInflates(t *testing.T) {
	pool := newMockPool(t)
	ledger := &fakeLedger{}
	m := NewMinter(pool, ledger, 0.5, enabledOn)

	hit := sampleHit()
	hit.ContributorWorkspace = "wsC" // different contributor, same request_id
	hit.Layer = "semantic"
	hit.EntryID = "emb-row-9"
	hit.Similarity = 0.97

	pool.ExpectBegin()
	pool.ExpectExec(`INSERT INTO pool_royalty_mints`).
		WithArgs("req-1", "wsB", "wsC", "semantic", "emb-row-9", "openai", "gpt-4o", 0.97, 2.0, 1.0, tAnswerHash, tPromptHash).
		WillReturnResult(pgxmock.NewResult("INSERT", 0)) // claim already taken
	pool.ExpectRollback()

	res, err := m.MintServedHit(context.Background(), hit)
	if err != nil {
		t.Fatalf("MintServedHit: %v", err)
	}
	if res.Minted || !res.AlreadyMinted {
		t.Errorf("Minted=%v AlreadyMinted=%v, want false/true", res.Minted, res.AlreadyMinted)
	}
	if len(ledger.calls) != 0 {
		t.Errorf("ledger credits = %d, want 0 (suppressed)", len(ledger.calls))
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// SINGLE TRANSACTION: a CreditTx failure rolls the claim back with it — no
// orphan claim row (which would permanently suppress the contributor's mint),
// no orphan credit.
func TestMintServedHit_CreditFailureRollsBackClaim(t *testing.T) {
	pool := newMockPool(t)
	ledger := &fakeLedger{err: errors.New("ledger down")}
	m := NewMinter(pool, ledger, 0.5, enabledOn)

	pool.ExpectBegin()
	pool.ExpectExec(`INSERT INTO pool_royalty_mints`).
		WithArgs("req-1", "wsB", "wsA", "exact", "lens:exact:deadbeef", "openai", "gpt-4o", 0.0, 2.0, 1.0, tAnswerHash, tPromptHash).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	pool.ExpectRollback() // claim + credit roll back together

	res, err := m.MintServedHit(context.Background(), sampleHit())
	if err == nil {
		t.Fatal("want error when CreditTx fails")
	}
	if res.Minted {
		t.Error("Minted must be false on rollback")
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations (claim must roll back, no commit): %v", err)
	}
}

// INERT BY DEFAULT: minting disabled → zero DB interaction, zero credits.
func TestMintServedHit_DisabledIsInert(t *testing.T) {
	pool := newMockPool(t) // NO expectations: any DB call fails the test
	ledger := &fakeLedger{}
	m := NewMinter(pool, ledger, 0.5, enabledOff)

	res, err := m.MintServedHit(context.Background(), sampleHit())
	if err != nil {
		t.Fatalf("disabled MintServedHit: %v", err)
	}
	if res.Minted || res.AlreadyMinted || len(ledger.calls) != 0 {
		t.Errorf("disabled minter must be a no-op; res=%+v credits=%d", res, len(ledger.calls))
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// CROSS-TENANT ONLY: a workspace served by its own pooled entry earns no
// royalty (there is no counterparty).
func TestMintServedHit_SelfHitDoesNotMint(t *testing.T) {
	pool := newMockPool(t) // NO expectations
	ledger := &fakeLedger{}
	m := NewMinter(pool, ledger, 0.5, enabledOn)

	hit := sampleHit()
	hit.ContributorWorkspace = hit.RequesterWorkspace
	res, err := m.MintServedHit(context.Background(), hit)
	if err != nil || res.Minted || len(ledger.calls) != 0 {
		t.Errorf("self-hit must not mint; res=%+v err=%v credits=%d", res, err, len(ledger.calls))
	}
}

// Defensive no-ops: empty request_id (no idempotency key → no mint), empty
// contributor (pre-feature entry), zero avoided_COGS, nil minter.
func TestMintServedHit_DefensiveNoOps(t *testing.T) {
	pool := newMockPool(t) // NO expectations
	ledger := &fakeLedger{}
	m := NewMinter(pool, ledger, 0.5, enabledOn)

	noKey := sampleHit()
	noKey.RequestID = ""
	if res, err := m.MintServedHit(context.Background(), noKey); err != nil || res.Minted {
		t.Errorf("empty request_id must not mint; res=%+v err=%v", res, err)
	}

	noOwner := sampleHit()
	noOwner.ContributorWorkspace = ""
	if res, err := m.MintServedHit(context.Background(), noOwner); err != nil || res.Minted {
		t.Errorf("empty contributor must not mint; res=%+v err=%v", res, err)
	}

	free := sampleHit()
	free.AvoidedCOGSUSD = 0
	if res, err := m.MintServedHit(context.Background(), free); err != nil || res.Minted {
		t.Errorf("zero avoided_COGS must not mint; res=%+v err=%v", res, err)
	}

	var nilM *Minter
	if res, err := nilM.MintServedHit(context.Background(), sampleHit()); err != nil || res.Minted {
		t.Errorf("nil minter must be a safe no-op; res=%+v err=%v", res, err)
	}

	if len(ledger.calls) != 0 {
		t.Errorf("no defensive case may credit; credits=%d", len(ledger.calls))
	}
}

// BURN-AND-MINT INVARIANT: minted = s × avoided_COGS and Talyvor's net
// (1−s) × avoided_COGS ≥ 0 for every share in [0,1]; out-of-range shares are
// clamped at construction so the invariant cannot be violated by config.
func TestRoyaltyShare_InvariantAndClamping(t *testing.T) {
	for _, tc := range []struct {
		in, want float64
	}{
		{0.0, 0.0}, {0.3, 0.3}, {0.5, 0.5}, {1.0, 1.0},
		{-0.5, 0.0}, // clamped low
		{1.5, 1.0},  // clamped high
	} {
		m := NewMinter(nil, nil, tc.in, enabledOn)
		if got := m.Share(); got != tc.want {
			t.Errorf("NewMinter(share=%v).Share() = %v, want %v", tc.in, got, tc.want)
		}
		const cogs = 3.0
		minted := m.Share() * cogs
		talyvorNet := (1 - m.Share()) * cogs
		if minted < 0 || talyvorNet < 0 {
			t.Errorf("share=%v: minted=%v talyvorNet=%v — invariant (1−s)×COGS ≥ 0 violated", tc.in, minted, talyvorNet)
		}
		if math.Abs(minted+talyvorNet-cogs) > 1e-9 {
			t.Errorf("share=%v: minted+net=%v, want %v (conservation)", tc.in, minted+talyvorNet, cogs)
		}
	}
}

// NaN HARDENING (diff-review finding): strconv.ParseFloat("NaN") parses
// without error and every <,>,<= comparison on NaN is false — so an
// unguarded NaN share (or NaN avoided_COGS) would sail through the clamp and
// the amount<=0 guard into CreditTx, permanently corrupting the contributor's
// balance (NaN propagates through every subsequent bal+delta). NaN and ±Inf
// must be neutralized at both layers.
func TestMintServedHit_NaNAndInfNeverReachTheLedger(t *testing.T) {
	pool := newMockPool(t) // NO expectations: any DB call fails the test
	ledger := &fakeLedger{}

	if m := NewMinter(pool, ledger, math.NaN(), enabledOn); m.Share() != 0 {
		t.Errorf("NewMinter(NaN).Share() = %v, want 0 (deflationary-safe)", m.Share())
	}
	if m := NewMinter(pool, ledger, math.Inf(1), enabledOn); m.Share() != 1 {
		t.Errorf("NewMinter(+Inf).Share() = %v, want 1 (clamped)", m.Share())
	}
	if m := NewMinter(pool, ledger, math.Inf(-1), enabledOn); m.Share() != 0 {
		t.Errorf("NewMinter(-Inf).Share() = %v, want 0 (clamped)", m.Share())
	}

	// Even with a valid share, a non-finite avoided_COGS must not credit.
	m := NewMinter(pool, ledger, 0.5, enabledOn)
	for _, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		h := sampleHit()
		h.AvoidedCOGSUSD = bad
		res, err := m.MintServedHit(context.Background(), h)
		if err != nil || res.Minted {
			t.Errorf("AvoidedCOGSUSD=%v must not mint; res=%+v err=%v", bad, res, err)
		}
	}
	if len(ledger.calls) != 0 {
		t.Fatalf("non-finite values must NEVER reach CreditTx; credits=%d", len(ledger.calls))
	}
}

// STAGE 2.3.0 — SHA256Hex is the house hex(sha256(...)) over RAW bytes,
// UNSALTED (no provider:model prefix — pure content identity). Known vectors.
func TestSHA256Hex_KnownVectors(t *testing.T) {
	if got := SHA256Hex([]byte("abc")); got != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Errorf("SHA256Hex(abc) = %q", got)
	}
	if got := SHA256Hex(nil); got != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Errorf("SHA256Hex(empty) = %q", got)
	}
}

// NO HASH → NO MINT (the privacy-coherence gate): a live serve whose evidence
// hashes could not be captured must write ZERO claim rows and mint NOTHING —
// an unadjudicable mint must never be created. The request itself still
// serves (the proxy never blocks on minting). This gate is WRITE-PATH ONLY:
// historical pre-2.3.0 rows with '' hashes are existing data the gate never
// scans, so backfill can't misfire.
func TestMintServedHit_NoHashNoMint(t *testing.T) {
	pool := newMockPool(t) // NO expectations: any DB call fails the test
	ledger := &fakeLedger{}
	m := NewMinter(pool, ledger, 0.5, enabledOn)

	noAnswer := sampleHit()
	noAnswer.AnswerSHA256 = ""
	if res, err := m.MintServedHit(context.Background(), noAnswer); err != nil || res.Minted {
		t.Errorf("missing answer hash must not mint; res=%+v err=%v", res, err)
	}

	noPrompt := sampleHit()
	noPrompt.PromptSHA256 = ""
	if res, err := m.MintServedHit(context.Background(), noPrompt); err != nil || res.Minted {
		t.Errorf("missing prompt hash must not mint; res=%+v err=%v", res, err)
	}

	if len(ledger.calls) != 0 {
		t.Fatalf("no-hash serves must NEVER credit; credits=%d", len(ledger.calls))
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("no-hash serves must NEVER touch the DB: %v", err)
	}
}

// TAMPER-EVIDENCE (the anti-evidence-destruction property, semantic path):
// the pooled semantic upsert OVERWRITES response on re-contribution
// (semanticUpsertPooledSQL: ON CONFLICT (prompt_hash) DO UPDATE SET response
// = EXCLUDED.response), so a contributor can replace their own entry's bytes
// after a serve. The hash recorded AT SERVE makes that detectable: the
// recorded hash stays bound to the served bytes (deterministic), and any
// overwrite yields a different current-entry hash — recorded ≠ current
// proves the entry changed since the serve.
func TestAnswerHash_TamperEvidence_OverwriteDetectable(t *testing.T) {
	served := []byte(`{"choices":[{"message":{"content":"the honest answer"}}]}`)
	recorded := SHA256Hex(served) // what 2.3.0 stores at serve time

	// determinism: the recorded hash is reproducible from the served bytes
	if SHA256Hex(served) != recorded {
		t.Fatal("SHA256Hex must be deterministic over the served bytes")
	}

	// a later overwrite (poisoner replacing their entry) changes the bytes
	overwritten := []byte(`{"choices":[{"message":{"content":"evidence destroyed"}}]}`)
	if SHA256Hex(overwritten) == recorded {
		t.Fatal("distinct entry bytes must yield a different hash")
	}
	// adjudicator's check: recorded != hash(current entry) => entry changed
	// since the serve — tamper-evident, adverse-inference signal.
}

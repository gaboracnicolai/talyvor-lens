package poolroyalty

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

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
	amount      int64
	txType      string
	desc        string
	meta        map[string]interface{}
}

type fakeLedger struct {
	calls []creditCall
	err   error
}

func (f *fakeLedger) CreditHeldTx(_ context.Context, _ pgx.Tx, workspaceID string, amount int64, txType, desc string, meta map[string]interface{}) error {
	f.calls = append(f.calls, creditCall{workspaceID: workspaceID, amount: amount, txType: txType, desc: desc, meta: meta})
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
		WithArgs(pgxmock.AnyArg(), "wsB", "wsA", "exact", "lens:exact:deadbeef", "openai", "gpt-4o", 0.0, 2.0, micro(1.0), tAnswerHash, tPromptHash, (72 * time.Hour).Microseconds()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	pool.ExpectCommit()

	res, err := m.MintServedHit(context.Background(), sampleHit())
	if err != nil {
		t.Fatalf("first MintServedHit: %v", err)
	}
	if !res.Minted || res.AlreadyMinted {
		t.Errorf("first serve: Minted=%v AlreadyMinted=%v, want true/false", res.Minted, res.AlreadyMinted)
	}
	if res.Amount != micro(1.0) { // 0.5 × 2.0
		t.Errorf("minted amount = %v, want 1.0 (s × avoided_COGS)", res.Amount)
	}
	if len(ledger.calls) != 1 {
		t.Fatalf("ledger credits = %d, want 1", len(ledger.calls))
	}
	if c := ledger.calls[0]; c.workspaceID != "wsA" || c.amount != micro(1.0) || c.txType != TypePoolRoyaltyHeld {
		t.Errorf("credit = %+v, want wsA / 1.0 / %s", c, TypePoolRoyaltyHeld)
	}

	// Retry with the SAME request_id: UNIQUE conflict → no credit, no commit of
	// any ledger write. The claim insert wrote nothing, so the tx just ends.
	pool.ExpectBegin()
	pool.ExpectExec(`INSERT INTO pool_royalty_mints`).
		WithArgs(pgxmock.AnyArg(), "wsB", "wsA", "exact", "lens:exact:deadbeef", "openai", "gpt-4o", 0.0, 2.0, micro(1.0), tAnswerHash, tPromptHash, (72 * time.Hour).Microseconds()).
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

// #145: the contributor's HELD ledger row must NOT carry the requester
// workspace id — not in the description, not in any metadata value. The
// requester stays ONLY in the admin-only pool_royalty_mints claim row (pinned
// here via the INSERT args). A contributor reading tokens/history learns it was
// served, never BY WHOM. This PR changes ZERO economics: same amount, same
// claim row, same idempotency — only the redundant contributor-facing copy goes.
func TestMintServedHit_LedgerRowOmitsRequester(t *testing.T) {
	pool := newMockPool(t)
	ledger := &fakeLedger{}
	m := NewMinter(pool, ledger, 0.5, enabledOn)

	// The claim INSERT STILL carries the requester ("wsB") — the authoritative
	// copy is untouched; only the contributor-facing ledger row is scrubbed.
	pool.ExpectBegin()
	pool.ExpectExec(`INSERT INTO pool_royalty_mints`).
		WithArgs(pgxmock.AnyArg(), "wsB", "wsA", "exact", "lens:exact:deadbeef", "openai", "gpt-4o", 0.0, 2.0, micro(1.0), tAnswerHash, tPromptHash, (72 * time.Hour).Microseconds()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	pool.ExpectCommit()

	res, err := m.MintServedHit(context.Background(), sampleHit())
	if err != nil {
		t.Fatalf("MintServedHit: %v", err)
	}
	// Economics guard: amount unchanged (0.5 × 2.0).
	if !res.Minted || res.Amount != micro(1.0) {
		t.Fatalf("economics changed: Minted=%v Amount=%v, want true/1.0", res.Minted, res.Amount)
	}
	if len(ledger.calls) != 1 {
		t.Fatalf("ledger credits = %d, want 1", len(ledger.calls))
	}
	c := ledger.calls[0]

	const requester = "wsB"
	if strings.Contains(c.desc, requester) {
		t.Errorf("LEAK: held-row description %q contains the requester %q", c.desc, requester)
	}
	for k, v := range c.meta {
		if s, ok := v.(string); ok && strings.Contains(s, requester) {
			t.Errorf("LEAK: held-row metadata[%q] = %q contains the requester %q", k, s, requester)
		}
	}
	if _, ok := c.meta["request_workspace_id"]; ok {
		t.Errorf("LEAK: held-row metadata still carries the request_workspace_id key")
	}
	// Sanity: still a recognizable pool-royalty credit to the contributor — we
	// scrubbed the counterparty, not the row.
	if c.workspaceID != "wsA" || c.txType != TypePoolRoyaltyHeld {
		t.Errorf("credit = %+v, want contributor wsA / %s", c, TypePoolRoyaltyHeld)
	}
	// The authoritative requester copy survives: the claim INSERT was called
	// with "wsB" (ExpectationsWereMet verifies it fired with that arg).
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations (claim INSERT must still carry wsB): %v", err)
	}
}

// CLAIM-CONFLICT PATH (deflationary): when the claim INSERT reports a conflict
// (RowsAffected 0) — a legitimate identical repeat now that the key is
// server-derived from contributor+requester+content+window — the mint reports
// AlreadyMinted and credits NOBODY. The RowsAffected==0 guard can only under-mint,
// never inflate. (Real cross-tenant suppression is separately DISPROVEN by
// TestMintKey_TwoContributorSuppression_Integration: a different contributor
// derives a different key and no longer collides.)
func TestMintServedHit_ClaimConflict_SuppressesNeverInflates(t *testing.T) {
	pool := newMockPool(t)
	ledger := &fakeLedger{}
	m := NewMinter(pool, ledger, 0.5, enabledOn)

	pool.ExpectBegin()
	pool.ExpectExec(`INSERT INTO pool_royalty_mints`).
		WithArgs(pgxmock.AnyArg(), "wsB", "wsA", "exact", "lens:exact:deadbeef", "openai", "gpt-4o", 0.0, 2.0, micro(1.0), tAnswerHash, tPromptHash, (72 * time.Hour).Microseconds()).
		WillReturnResult(pgxmock.NewResult("INSERT", 0)) // the derived key was already claimed
	pool.ExpectRollback()

	res, err := m.MintServedHit(context.Background(), sampleHit())
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
		WithArgs(pgxmock.AnyArg(), "wsB", "wsA", "exact", "lens:exact:deadbeef", "openai", "gpt-4o", 0.0, 2.0, micro(1.0), tAnswerHash, tPromptHash, (72 * time.Hour).Microseconds()).
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

// Defensive no-ops: empty contributor OR requester (both are mint-KEY inputs
// now — SEC-11), zero avoided_COGS, nil minter. NOTE the deliberate ABSENCE of
// an empty-client-header case: h.RequestID no longer gates a mint (see
// TestMintKey_EmptyClientHeaderStillMints_Integration).
func TestMintServedHit_DefensiveNoOps(t *testing.T) {
	pool := newMockPool(t) // NO expectations
	ledger := &fakeLedger{}
	m := NewMinter(pool, ledger, 0.5, enabledOn)

	noRequester := sampleHit()
	noRequester.RequesterWorkspace = ""
	if res, err := m.MintServedHit(context.Background(), noRequester); err != nil || res.Minted {
		t.Errorf("empty requester (a key input) must not mint; res=%+v err=%v", res, err)
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
// historical pre-2.3.0 rows with " hashes are existing data the gate never
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

// ─── 2.3b primitive #1: the per-pair mint cap ───

// capArgs is the claim-insert arg tuple sampleHit() produces (12 args incl.
// the 2.3.0 evidence hashes) — shared by the cap tests below.
func capExpectClaim(pool pgxmock.PgxPoolIface) {
	pool.ExpectExec(`INSERT INTO pool_royalty_mints`).
		WithArgs(pgxmock.AnyArg(), "wsB", "wsA", "exact", "lens:exact:deadbeef", "openai", "gpt-4o", 0.0, 2.0, micro(1.0), tAnswerHash, tPromptHash, (72 * time.Hour).Microseconds()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
}

// UNDER CAP: the after-CreditTx count (which includes the just-inserted row)
// is ≤ cap → the mint commits exactly as before.
func TestMintServedHit_UnderCap_Mints(t *testing.T) {
	pool := newMockPool(t)
	ledger := &fakeLedger{}
	m := NewMinter(pool, ledger, 0.5, enabledOn)
	m.SetCap(3, 24*time.Hour)

	pool.ExpectBegin()
	capExpectClaim(pool)
	pool.ExpectQuery(`SELECT COUNT\(\*\) FROM pool_royalty_mints`).
		WithArgs("wsB", "wsA", (24 * time.Hour).Microseconds()).
		WillReturnRows(pgxmock.NewRows([]string{"n"}).AddRow(int64(3))) // 3rd of 3 — at cap, allowed
	pool.ExpectCommit()

	res, err := m.MintServedHit(context.Background(), sampleHit())
	if err != nil || !res.Minted || res.Capped {
		t.Fatalf("under-cap mint must succeed; res=%+v err=%v", res, err)
	}
	if len(ledger.calls) != 1 {
		t.Errorf("credits=%d want 1", len(ledger.calls))
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// AT CAP+1: the count exceeds the cap → Result{Capped}, and the deferred
// rollback discards the claim insert AND the credit atomically (the same
// rollback path AlreadyMinted uses). Zero net rows, zero mint; the customer
// was already served before the mint ran.
func TestMintServedHit_OverCap_RollsBackClaimAndCredit(t *testing.T) {
	pool := newMockPool(t)
	ledger := &fakeLedger{}
	m := NewMinter(pool, ledger, 0.5, enabledOn)
	m.SetCap(3, 24*time.Hour)

	pool.ExpectBegin()
	capExpectClaim(pool)
	pool.ExpectQuery(`SELECT COUNT\(\*\) FROM pool_royalty_mints`).
		WithArgs("wsB", "wsA", (24 * time.Hour).Microseconds()).
		WillReturnRows(pgxmock.NewRows([]string{"n"}).AddRow(int64(4))) // would be the 4th — over
	pool.ExpectRollback()

	res, err := m.MintServedHit(context.Background(), sampleHit())
	if err != nil {
		t.Fatalf("cap-hit must not error (serve-but-skip): %v", err)
	}
	if res.Minted || !res.Capped {
		t.Errorf("res=%+v, want Capped=true Minted=false", res)
	}
	if res.CapReason != "per_pair" {
		t.Errorf("CapReason = %q, want per_pair", res.CapReason)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet (must roll back, never commit): %v", err)
	}
}

// CAP DISABLED (PerPair==0, the default): the cap branch never runs — NO
// COUNT statement is issued — and mint behavior is byte-identical to pre-cap
// regardless of prior volume. Asserted by the absence of any COUNT
// expectation: pgxmock fails on any unexpected query.
func TestMintServedHit_CapDisabled_NoCountByteIdentical(t *testing.T) {
	pool := newMockPool(t)
	ledger := &fakeLedger{}
	m := NewMinter(pool, ledger, 0.5, enabledOn) // SetCap never called → 0 → disabled

	pool.ExpectBegin()
	capExpectClaim(pool)
	pool.ExpectCommit() // straight to commit — no COUNT in between

	res, err := m.MintServedHit(context.Background(), sampleHit())
	if err != nil || !res.Minted {
		t.Fatalf("disabled-cap mint must be byte-identical to pre-cap; res=%+v err=%v", res, err)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// WINDOW ROLL + UN-CLAIM SEMANTICS (pinned intentionally): a serve capped in
// window W rolled back — which UN-CLAIMS its request_id. The SAME request_id
// in a LATER window (count back under cap) may then mint: the cap is a
// per-window bound; later windows have fresh budget. The claim insert
// succeeds again (RowsAffected 1) precisely because the capped attempt's
// rollback left no row behind.
func TestMintServedHit_CappedThenLaterWindow_SameRequestIDMints(t *testing.T) {
	pool := newMockPool(t)
	ledger := &fakeLedger{}
	m := NewMinter(pool, ledger, 0.5, enabledOn)
	m.SetCap(3, 24*time.Hour)

	// Window W: capped → rollback (un-claims req-1).
	pool.ExpectBegin()
	capExpectClaim(pool)
	pool.ExpectQuery(`SELECT COUNT\(\*\) FROM pool_royalty_mints`).
		WithArgs("wsB", "wsA", (24 * time.Hour).Microseconds()).
		WillReturnRows(pgxmock.NewRows([]string{"n"}).AddRow(int64(4)))
	pool.ExpectRollback()

	// Later window: same request_id; claim inserts cleanly (no conflict —
	// the rollback left nothing), count is back under cap → mints.
	pool.ExpectBegin()
	capExpectClaim(pool)
	pool.ExpectQuery(`SELECT COUNT\(\*\) FROM pool_royalty_mints`).
		WithArgs("wsB", "wsA", (24 * time.Hour).Microseconds()).
		WillReturnRows(pgxmock.NewRows([]string{"n"}).AddRow(int64(1)))
	pool.ExpectCommit()

	if res, err := m.MintServedHit(context.Background(), sampleHit()); err != nil || !res.Capped {
		t.Fatalf("window W attempt must be Capped; res=%+v err=%v", res, err)
	}
	if res, err := m.MintServedHit(context.Background(), sampleHit()); err != nil || !res.Minted {
		t.Fatalf("later-window same-request_id attempt must mint; res=%+v err=%v", res, err)
	}
	if len(ledger.calls) != 2 { // CreditTx ran both times; first was rolled back
		t.Errorf("credit calls=%d want 2 (first discarded by rollback)", len(ledger.calls))
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// STAGE 2.3a — CAP ROLLBACK DISCARDS THE HELD CREDIT: identical to the
// over-cap rollback test, now asserting against the HELD-credit flow — the
// held credit sits inside the same tx (before the cap COUNT, whose exactness
// rides the FOR UPDATE the held credit acquires), so Result{Capped} +
// deferred Rollback discard claim AND held credit together. held_balance is
// provably unchanged because NO Commit ever happens (pgxmock would fail on
// an unexpected Commit) and the fake ledger's call is rolled back with the tx.
func TestMintServedHit_OverCap_RollsBackHeldCredit(t *testing.T) {
	pool := newMockPool(t)
	ledger := &fakeLedger{}
	m := NewMinter(pool, ledger, 0.5, enabledOn)
	m.SetCap(3, 24*time.Hour)

	pool.ExpectBegin()
	capExpectClaim(pool)
	pool.ExpectQuery(`SELECT COUNT\(\*\) FROM pool_royalty_mints`).
		WithArgs("wsB", "wsA", (24 * time.Hour).Microseconds()).
		WillReturnRows(pgxmock.NewRows([]string{"n"}).AddRow(int64(4)))
	pool.ExpectRollback() // claim + HELD credit discarded together; no Commit

	res, err := m.MintServedHit(context.Background(), sampleHit())
	if err != nil || res.Minted || !res.Capped {
		t.Fatalf("capped serve must roll back the held credit; res=%+v err=%v", res, err)
	}
	if len(ledger.calls) != 1 || ledger.calls[0].txType != TypePoolRoyaltyHeld {
		t.Errorf("the (rolled-back) credit must have been the HELD type; calls=%+v", ledger.calls)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet (must roll back, never commit): %v", err)
	}
}

// ─── per-ENTRY cap (2.3b follow-up) ───

// UNDER per-entry cap (per-pair disabled): the entry COUNT (incl. just-inserted
// row) is <= cap → commit.
func TestMintServedHit_UnderEntryCap_Mints(t *testing.T) {
	pool := newMockPool(t)
	ledger := &fakeLedger{}
	m := NewMinter(pool, ledger, 0.5, enabledOn)
	m.SetCap(0, 24*time.Hour)      // per-pair OFF
	m.SetEntryCap(5, 24*time.Hour) // per-entry ON

	pool.ExpectBegin()
	capExpectClaim(pool)
	pool.ExpectQuery(`SELECT COUNT\(\*\) FROM pool_royalty_mints\s+WHERE entry_id`).
		WithArgs("lens:exact:deadbeef", (24 * time.Hour).Microseconds()).
		WillReturnRows(pgxmock.NewRows([]string{"n"}).AddRow(int64(5)))
	pool.ExpectCommit()

	res, err := m.MintServedHit(context.Background(), sampleHit())
	if err != nil || !res.Minted || res.Capped {
		t.Fatalf("under entry-cap must mint; res=%+v err=%v", res, err)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// AT per-entry cap+1: Capped with reason "per_entry"; claim+credit rolled back.
func TestMintServedHit_OverEntryCap_RollsBackWithReason(t *testing.T) {
	pool := newMockPool(t)
	ledger := &fakeLedger{}
	m := NewMinter(pool, ledger, 0.5, enabledOn)
	m.SetCap(0, 24*time.Hour)
	m.SetEntryCap(5, 24*time.Hour)

	pool.ExpectBegin()
	capExpectClaim(pool)
	pool.ExpectQuery(`SELECT COUNT\(\*\) FROM pool_royalty_mints\s+WHERE entry_id`).
		WithArgs("lens:exact:deadbeef", (24 * time.Hour).Microseconds()).
		WillReturnRows(pgxmock.NewRows([]string{"n"}).AddRow(int64(6)))
	pool.ExpectRollback()

	res, err := m.MintServedHit(context.Background(), sampleHit())
	if err != nil {
		t.Fatalf("entry cap-hit must not error: %v", err)
	}
	if res.Minted || !res.Capped || res.CapReason != "per_entry" {
		t.Errorf("res=%+v, want Capped=true Minted=false CapReason=per_entry", res)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// BOTH caps enabled, DETERMINISTIC ORDER: per-pair is checked first, so a serve
// that breaches per-pair reports "per_pair" and the per-entry COUNT is never
// even run (no query expectation for it).
func TestMintServedHit_BothCaps_PerPairCheckedFirst(t *testing.T) {
	pool := newMockPool(t)
	ledger := &fakeLedger{}
	m := NewMinter(pool, ledger, 0.5, enabledOn)
	m.SetCap(3, 24*time.Hour)      // per-pair ON
	m.SetEntryCap(5, 24*time.Hour) // per-entry ON

	pool.ExpectBegin()
	capExpectClaim(pool)
	pool.ExpectQuery(`SELECT COUNT\(\*\) FROM pool_royalty_mints\s+WHERE requester_workspace_id`).
		WithArgs("wsB", "wsA", (24 * time.Hour).Microseconds()).
		WillReturnRows(pgxmock.NewRows([]string{"n"}).AddRow(int64(4))) // breaches per-pair
	pool.ExpectRollback() // no per-entry COUNT expectation — must not run

	res, err := m.MintServedHit(context.Background(), sampleHit())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Capped || res.CapReason != "per_pair" {
		t.Errorf("res=%+v, want per_pair (checked first)", res)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet (per-entry COUNT must NOT run when per-pair already capped): %v", err)
	}
}

// BOTH enabled, per-pair PASSES then per-entry breaches → reason "per_entry".
func TestMintServedHit_BothCaps_EntryBreachesAfterPairPasses(t *testing.T) {
	pool := newMockPool(t)
	ledger := &fakeLedger{}
	m := NewMinter(pool, ledger, 0.5, enabledOn)
	m.SetCap(10, 24*time.Hour)
	m.SetEntryCap(5, 24*time.Hour)

	pool.ExpectBegin()
	capExpectClaim(pool)
	pool.ExpectQuery(`SELECT COUNT\(\*\) FROM pool_royalty_mints\s+WHERE requester_workspace_id`).
		WithArgs("wsB", "wsA", (24 * time.Hour).Microseconds()).
		WillReturnRows(pgxmock.NewRows([]string{"n"}).AddRow(int64(4))) // under per-pair (10)
	pool.ExpectQuery(`SELECT COUNT\(\*\) FROM pool_royalty_mints\s+WHERE entry_id`).
		WithArgs("lens:exact:deadbeef", (24 * time.Hour).Microseconds()).
		WillReturnRows(pgxmock.NewRows([]string{"n"}).AddRow(int64(6))) // over per-entry (5)
	pool.ExpectRollback()

	res, err := m.MintServedHit(context.Background(), sampleHit())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Capped || res.CapReason != "per_entry" {
		t.Errorf("res=%+v, want per_entry (per-pair passed, per-entry breached)", res)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// DISABLED: per-entry == 0 → no entry COUNT runs (byte-identical to pre-feature).
func TestMintServedHit_EntryCapDisabled_NoCount(t *testing.T) {
	pool := newMockPool(t)
	ledger := &fakeLedger{}
	m := NewMinter(pool, ledger, 0.5, enabledOn) // neither cap set → both 0

	pool.ExpectBegin()
	capExpectClaim(pool)
	pool.ExpectCommit() // straight to commit — NO COUNT of either kind

	if res, err := m.MintServedHit(context.Background(), sampleHit()); err != nil || !res.Minted {
		t.Fatalf("both caps disabled must mint byte-identically; res=%+v err=%v", res, err)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

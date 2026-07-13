package poolroyalty

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/mining"
)

// SEC-11 — the pool-royalty mint idempotency key must be SERVER-DERIVED from
// (contributor : requester : answer_sha256 : hold-window bucket), NEVER the
// client X-Talyvor-Request-ID. A bare UNIQUE(request_id) over a client-chosen
// header lets a verified requester SUPPRESS the royalty owed to another tenant
// by reusing one request_id across serves of different contributors' content.
//
// These are real-PG tests (the suppression property is the UNIQUE constraint +
// ON CONFLICT DO NOTHING — pgxmock cannot exhibit it). Gated on
// LENS_TEST_DATABASE_URL exactly like the cap/holdback integration tests. Each
// runs in its OWN schema (search_path) so a parallel package can't collide.

func mintKeyHarness(t *testing.T) (*pgxpool.Pool, *mining.LedgerStore) {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG SEC-11 mint-key test")
	}
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = "lens_mintkey_test"
	cfg.MaxConns = 10
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `DROP SCHEMA IF EXISTS lens_mintkey_test CASCADE; CREATE SCHEMA lens_mintkey_test`); err != nil {
		t.Fatalf("schema: %v", err)
	}
	entryCapResetSchema(t, pool, ctx) // the shared pool_royalty_mints / ledger / balances DDL
	return pool, mining.NewLedgerStore(pool)
}

// heldOf returns a workspace's held_balance in µLENS (0 if the row is absent). SEC-2: BIGINT µLENS.
func heldOf(t *testing.T, pool *pgxpool.Pool, ws string) int64 {
	t.Helper()
	var held int64
	_ = pool.QueryRow(context.Background(),
		`SELECT COALESCE((SELECT held_balance FROM lens_token_balances WHERE workspace_id=$1),0)`, ws).Scan(&held)
	return held
}

// balanceOf returns a workspace's spendable balance in µLENS (0 if the row is absent). SEC-2: BIGINT µLENS.
func balanceOf(t *testing.T, pool *pgxpool.Pool, ws string) int64 {
	t.Helper()
	var bal int64
	_ = pool.QueryRow(context.Background(),
		`SELECT COALESCE((SELECT balance FROM lens_token_balances WHERE workspace_id=$1),0)`, ws).Scan(&bal)
	return bal
}

// THE SEC-11 RED PROOF. Requester R is served contributor A's content, then
// contributor B's DIFFERENT content, REUSING one X-Talyvor-Request-ID. Both are
// legitimate cross-tenant royalties owed to two different tenants.
//
// TODAY (bare UNIQUE(request_id) = the client header): A's serve claims the row;
// B's serve collides on the same request_id → ON CONFLICT → AlreadyMinted → B is
// SUPPRESSED. This test FAILS (B not minted) — that is the vulnerability.
// AFTER the fix (server key binds the contributor + served content): the two
// serves derive DIFFERENT keys → BOTH mint.
func TestMintKey_TwoContributorSuppression_Integration(t *testing.T) {
	pool, ledger := mintKeyHarness(t)
	ctx := context.Background()
	m := NewMinter(pool, ledger, 0.5, func() bool { return true })

	const sharedHeader = "req-reused-by-attacker" // R reuses ONE header across serves

	hA := ServedHit{
		RequestID: sharedHeader, RequesterWorkspace: "wsR", ContributorWorkspace: "wsA",
		Layer: "exact", EntryID: "entry-A", Provider: "openai", Model: "gpt-4o",
		AvoidedCOGSUSD: 2.0,
		AnswerSHA256:   SHA256Hex([]byte("answer-from-A")), PromptSHA256: SHA256Hex([]byte("prompt")),
	}
	hB := hA // same requester, same reused header — DIFFERENT contributor + content
	hB.ContributorWorkspace = "wsB"
	hB.EntryID = "entry-B"
	hB.AnswerSHA256 = SHA256Hex([]byte("answer-from-B"))

	resA, err := m.MintServedHit(ctx, hA)
	if err != nil || !resA.Minted {
		t.Fatalf("A serve: res=%+v err=%v, want Minted", resA, err)
	}
	resB, err := m.MintServedHit(ctx, hB)
	if err != nil {
		t.Fatal(err)
	}
	if !resB.Minted {
		t.Fatalf("SEC-11 SUPPRESSION: contributor B's royalty was suppressed by A's reused request_id "+
			"(res=%+v). A verified requester must not be able to deny another tenant's mint by reusing "+
			"X-Talyvor-Request-ID.", resB)
	}

	var rows int64
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM pool_royalty_mints`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatalf("claim rows=%d, want 2 (A and B each mint despite the shared client header)", rows)
	}
	for _, ws := range []string{"wsA", "wsB"} {
		if held := heldOf(t, pool, ws); held != micro(0.5*2.0) {
			t.Fatalf("%s held=%v, want %v µLENS (each contributor keeps its own royalty)", ws, held, micro(0.5*2.0))
		}
	}
}

// EXACTLY-ONCE PRESERVED + HEADER CANNOT FORCE A SECOND MINT. A genuine identical
// repeat — same contributor, requester, answer bytes, window — but a DIFFERENT
// client header must still dedup to ONE mint.
//
// TODAY (header IS the key): two different headers → TWO mints. This test FAILS
// (2 rows). AFTER the fix (content-derived key): the header is irrelevant → the
// repeat dedups → ONE mint.
func TestMintKey_ExactlyOnceIgnoresHeader_Integration(t *testing.T) {
	pool, ledger := mintKeyHarness(t)
	ctx := context.Background()
	m := NewMinter(pool, ledger, 0.5, func() bool { return true })

	base := ServedHit{
		RequestID: "header-1", RequesterWorkspace: "wsR", ContributorWorkspace: "wsA",
		Layer: "exact", EntryID: "entry-A", Provider: "openai", Model: "gpt-4o",
		AvoidedCOGSUSD: 2.0,
		AnswerSHA256:   SHA256Hex([]byte("identical-answer")), PromptSHA256: SHA256Hex([]byte("p")),
	}
	first, err := m.MintServedHit(ctx, base)
	if err != nil || !first.Minted {
		t.Fatalf("first serve: res=%+v err=%v, want Minted", first, err)
	}
	repeat := base
	repeat.RequestID = "header-2-totally-different" // only the client header changes
	second, err := m.MintServedHit(ctx, repeat)
	if err != nil {
		t.Fatal(err)
	}
	if second.Minted || !second.AlreadyMinted {
		t.Fatalf("legit identical repeat must dedup regardless of the client header: res=%+v want "+
			"AlreadyMinted (exactly-once preserved; a header change cannot manufacture a 2nd mint)", second)
	}
	var rows int64
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM pool_royalty_mints`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("rows=%d, want 1 (the repeat took no new claim)", rows)
	}
	if held := heldOf(t, pool, "wsA"); held != micro(0.5*2.0) {
		t.Fatalf("wsA held=%v, want %v µLENS (credited exactly once)", held, micro(0.5*2.0))
	}
}

// SEC-11 GUARD (pure, no DB, always runs): the mint key derivation is
// deterministic, contributor/requester/content/window-scoped, and — structurally
// — has NO parameter through which the client X-Talyvor-Request-ID could enter.
func TestMintClaimKey_Derivation(t *testing.T) {
	const req, ans, bucket = "wsR", "answerhashABC", "2026-07-12T00:00:00Z"
	base := mintClaimKey("wsA", req, ans, bucket)

	// (1) Deterministic — identical inputs → identical key (exactly-once foundation).
	if again := mintClaimKey("wsA", req, ans, bucket); again != base {
		t.Fatalf("non-deterministic key: %q vs %q", again, base)
	}
	// (2) Contributor IS in the key — different contributor → different key. THIS is
	//     what makes cross-tenant suppression impossible.
	if k := mintClaimKey("wsB", req, ans, bucket); k == base {
		t.Fatal("different contributor produced the SAME key — cross-tenant suppression would be reachable")
	}
	// (3) Served content IS in the key — different answer bytes → different key.
	if k := mintClaimKey("wsA", req, "DIFFERENTanswerhash", bucket); k == base {
		t.Fatal("different served answer produced the SAME key")
	}
	// (4) Requester IS in the key — per-relationship scoping (owner:requester:content).
	if k := mintClaimKey("wsA", "wsOTHER", ans, bucket); k == base {
		t.Fatal("different requester produced the SAME key")
	}
	// (5) Window bucket IS in the key — a later window → different key (a genuine
	//     re-serve in a new window earns again; the accepted retry tradeoff).
	if k := mintClaimKey("wsA", req, ans, "2026-07-20T00:00:00Z"); k == base {
		t.Fatal("different window bucket produced the SAME key")
	}
	// (6) By construction mintClaimKey takes exactly (contributor, requester,
	//     answer, bucket) — the client header is not among its inputs, so it cannot
	//     influence the key regardless of what the requester sends.
}

// windowBucketFor: two instants in one hold window share a bucket (dedup within a
// window); a later window gets a fresh bucket (re-serve earns again); a
// non-positive window falls back rather than bucketing on a zero interval.
func TestWindowBucketFor(t *testing.T) {
	const w = 72 * time.Hour
	boundary := time.Date(2026, 7, 12, 5, 0, 0, 0, time.UTC).Truncate(w) // an exact bucket start
	if windowBucketFor(boundary, w) != windowBucketFor(boundary.Add(time.Hour), w) {
		t.Fatal("two instants in the SAME hold window must share a bucket")
	}
	if windowBucketFor(boundary, w) == windowBucketFor(boundary.Add(w+time.Hour), w) {
		t.Fatal("instants in DIFFERENT hold windows must get different buckets")
	}
	if windowBucketFor(boundary, 0) == "" {
		t.Fatal("a non-positive window must fall back, not yield an empty bucket")
	}
}

// SEC-11 item 3(c): the FinalizeSweeper and Revoker key off the request_id READ
// FROM the row (their CAS), never re-deriving it — so switching pool_royalty to a
// server-derived key is transparent to them. Also: Result.RequestID is that
// derived key and matches the stored row (the operator correlation handle now
// that the client header is not stored).
func TestMintKey_SweeperAndRevokerCAS_DerivedKey_Integration(t *testing.T) {
	pool, ledger := mintKeyHarness(t)
	ctx := context.Background()
	m := NewMinter(pool, ledger, 0.5, func() bool { return true })
	m.SetHoldbackWindow(time.Millisecond) // due almost immediately, for the sweep

	mint := func(contrib, ans string) Result {
		res, err := m.MintServedHit(ctx, ServedHit{
			RequestID: "ignored-client-header", RequesterWorkspace: "wsR", ContributorWorkspace: contrib,
			Layer: "exact", EntryID: "e-" + contrib, Provider: "openai", Model: "gpt-4o",
			AvoidedCOGSUSD: 2.0, AnswerSHA256: SHA256Hex([]byte(ans)), PromptSHA256: SHA256Hex([]byte("p")),
		})
		if err != nil || !res.Minted {
			t.Fatalf("mint %s: res=%+v err=%v", contrib, res, err)
		}
		return res
	}
	rA := mint("wsA", "ansA")
	_ = mint("wsB", "ansB")
	rC := mint("wsC", "ansC")

	// Result.RequestID is the server-derived key and MATCHES the stored row.
	var storedReq string
	if err := pool.QueryRow(ctx, `SELECT request_id FROM pool_royalty_mints WHERE contributor_workspace_id='wsA'`).Scan(&storedReq); err != nil {
		t.Fatal(err)
	}
	if rA.RequestID == "" || storedReq != rA.RequestID {
		t.Fatalf("stored request_id=%q vs Result.RequestID=%q — must be the (matching) derived key", storedReq, rA.RequestID)
	}

	// REVOKER CAS keyed on the derived request_id (as an explicit id): wsC's held
	// row flips to revoked and its held balance is burned.
	rev := NewRevoker(pool, ledger)
	rep, err := rev.RevokeHeldMints(ctx, []string{rC.RequestID})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Outcomes[rC.RequestID] != OutcomeRevoked {
		t.Fatalf("revoke outcome=%v, want %q (CAS on the derived key read from the row)", rep.Outcomes[rC.RequestID], OutcomeRevoked)
	}
	if held := heldOf(t, pool, "wsC"); held != 0 {
		t.Fatalf("wsC held=%v after revoke, want 0 (burned)", held)
	}

	// SWEEPER CAS reads request_id FROM each due row and finalizes by it: the two
	// remaining held rows (wsA, wsB) settle; the revoked wsC row is excluded.
	time.Sleep(5 * time.Millisecond)
	sw := NewFinalizeSweeper(pool, ledger, "pool_royalty_mints")
	n, err := sw.RunOnce(ctx)
	if err != nil || n != 2 {
		t.Fatalf("sweeper finalized n=%d err=%v, want 2 (wsA+wsB; revoked wsC excluded)", n, err)
	}
	if bal := balanceOf(t, pool, "wsA"); bal != micro(1.0) {
		t.Fatalf("wsA spendable after finalize=%v, want %v µLENS (held→spendable via CAS on the derived key)", bal, micro(1.0))
	}
	if held := heldOf(t, pool, "wsA"); held != 0 {
		t.Fatalf("wsA held after finalize=%v, want 0", held)
	}
}

// SEC-11 corollary: an EMPTY client X-Talyvor-Request-ID still mints — the key
// is content-derived, so the header's value (or absence) cannot influence WHETHER
// or HOW a mint happens. Proves the removed h.RequestID=="" guard (which let the
// header gate a mint) is gone.
func TestMintKey_EmptyClientHeaderStillMints_Integration(t *testing.T) {
	pool, ledger := mintKeyHarness(t)
	ctx := context.Background()
	m := NewMinter(pool, ledger, 0.5, func() bool { return true })

	res, err := m.MintServedHit(ctx, ServedHit{
		RequestID: "", RequesterWorkspace: "wsR", ContributorWorkspace: "wsA",
		Layer: "exact", EntryID: "e", Provider: "openai", Model: "gpt-4o",
		AvoidedCOGSUSD: 2.0, AnswerSHA256: SHA256Hex([]byte("ans")), PromptSHA256: SHA256Hex([]byte("p")),
	})
	if err != nil || !res.Minted {
		t.Fatalf("empty client header must still mint (content-derived key): res=%+v err=%v", res, err)
	}
	if res.RequestID == "" {
		t.Fatal("Result.RequestID must be the derived key even when the client header is empty")
	}
	if held := heldOf(t, pool, "wsA"); held != micro(1.0) {
		t.Fatalf("wsA held=%v, want %v µLENS", held, micro(1.0))
	}
}

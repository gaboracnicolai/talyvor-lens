package proxy

// UNBACKED-CREDIT ROYALTY (real PG, real migrated schema).
//
// ⚠ THE CLAIM UNDER TEST. lxc_ledger rows are TYPED (purchase / admin_grant / convert_from_lens), but
// SettleLXCReservation reads a FUNGIBLE lxc_balances row with no source filter. If that holds, a
// cross-tenant pooled hit paid for with GRANTED or CONVERTED-FROM-EARNINGS credit mints a FULL royalty
// against money that never entered the system — and the loop earn → convert → spend → mint → convert
// issues new LENS with no cash behind it.
//
// ⚠ WHY THE EXISTING funding_invariant_realpg_test.go DOES NOT COVER THIS. Its fundLXC helper writes
// lxc_balances.balance with a direct INSERT. No test in the repo funds a workspace through a REAL credit
// path, so no test can distinguish one source from another — the invariant it asserts is "charge > 0 ⇒
// mint", never "cash-backed charge > 0 ⇒ mint".
//
// These tests fund through economy's OWN credit entrypoints and then drive the SAME seam production does:
// agentReserveBlocks → settlePooledServe → mintPooledRoyalty. Every assertion is a LEDGER ROW.
//
// The invariant asserted here is the one the project has held all along: value is never created from
// nothing. It is expected to FAIL on the granted and converted cases until a fix lands.

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/migrations"

	"github.com/talyvor/lens/internal/earnverify"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/mining"
	"github.com/talyvor/lens/internal/poolroyalty"
	"github.com/talyvor/lens/internal/workspace"
)

// runSalt makes every workspace id unique to this process, so re-running against the shared
// lens_test database cannot make one run read another run's ledger rows.
var runSalt = time.Now().UnixNano()

// unbackedEnv is the real-schema harness. It creates NOTHING and TRUNCATES NOTHING: it runs against a
// database the real migrations built, and every case uses fresh workspace ids, so a stale row from
// another test can neither hide a mint nor manufacture one.
type unbackedEnv struct {
	p       *Proxy
	store   *economy.DualTokenStore // lens + engine wired: Grant, Credit(LXC), Convert all available
	lens    *mining.LedgerStore
	pool    *pgxpool.Pool
	tag     string
	buyer   string
	seller  string
	agentID string
}

func newUnbackedEnv(t *testing.T, tag string) *unbackedEnv {
	t.Helper()
	admin := os.Getenv("LENS_TEST_DATABASE_URL")
	if admin == "" {
		// ⚠ FAIL, never Skip. `go test` prints ok + exits 0 on a skip, and this is a money-path
		// verification whose whole value is that it actually ran. CI always sets this
		// (.github/workflows/ci.yaml, the `go test` step), so failing here cannot silently pass.
		t.Fatal("LENS_TEST_DATABASE_URL not set — this test must RUN, not skip")
	}
	ctx := context.Background()

	// ⚠ ITS OWN MIGRATED DATABASE, not the shared lens_test.
	//
	// Sibling packages destructively reset the shared database mid-run, and this file INSERTS
	// workspaces and ledger rows without truncating — so sharing cuts both ways: their resets break
	// these assertions, and these rows break theirs (measured: 14 sibling money-path tests turned red
	// purely from co-tenancy). An isolated database removes the coupling entirely for one
	// CREATE/DROP, and is what the repo's other real-PG money tests already do.
	name := fmt.Sprintf("lens_unbacked_%d", time.Now().UnixNano())
	ac, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ac.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		_ = ac.Close(ctx)
		t.Fatal(err)
	}
	_ = ac.Close(ctx)
	u, _ := url.Parse(admin)
	u.Path = "/" + name
	mc, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dbmigrate.Run(ctx, mc, migrations.FS); err != nil {
		_ = mc.Close(ctx)
		t.Fatal(err)
	}
	_ = mc.Close(ctx)
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		c, err := pgx.Connect(context.Background(), admin)
		if err != nil {
			return
		}
		_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		_ = c.Close(context.Background())
	})

	lens := mining.NewLedgerStore(pool)
	lens.SetMintVerifier(earnverify.New(false)) // U6 floor, wired unconditionally as production does
	engine := economy.NewRateEngine(lens, pool)
	store := economy.NewDualTokenStore(lens, pool, engine)
	minter := poolroyalty.NewMinter(pool, lens, 0.5, func() bool { return true })

	p := &Proxy{}
	p.SetAgentSpender(store, func() bool { return true })
	p.SetReservation(func() bool { return true }, func() int { return 4096 })
	p.SetRoyaltyMinter(minter)
	p.SetPoolConsumerDiscount(0.30)

	// ⚠ RUN-UNIQUE. The harness truncates nothing by design, so a FIXED id would carry a previous
	// run's pool_royalty_mints rows into this run's count — the assertions would then be reading
	// history rather than what this test just did.
	tag = tag + "-" + strconv.FormatInt(runSalt, 36)
	e := &unbackedEnv{
		p: p, store: store, lens: lens, pool: pool, tag: tag,
		buyer:   "ws-buyer-" + tag,
		seller:  "ws-seller-" + tag,
		agentID: "agent-" + tag, // distinct per case: the reservation id derives from it
	}
	e.mkWorkspace(t, e.buyer, false)
	e.mkWorkspace(t, e.seller, true) // the CONTRIBUTOR can earn, so a zero mint is never the U6 floor
	return e
}

func (e *unbackedEnv) mkWorkspace(t *testing.T, id string, earnVerified bool) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(),
		`INSERT INTO workspaces (id, name, cache_prefix, earn_verified) VALUES ($1,$1,$1,$2)
		 ON CONFLICT (id) DO UPDATE SET earn_verified = EXCLUDED.earn_verified`, id, earnVerified); err != nil {
		t.Fatalf("create workspace %s: %v", id, err)
	}
}

// creditTypes returns the DISTINCT lxc_ledger types that CREDITED this workspace (amount > 0). This is the
// fixture's positive control: it proves the funding actually travelled the path the case name claims,
// rather than the test asserting against a source it never really used.
func (e *unbackedEnv) creditTypes(t *testing.T, ws string) []string {
	t.Helper()
	rows, err := e.pool.Query(context.Background(),
		`SELECT DISTINCT type FROM lxc_ledger
		  WHERE workspace_id = $1 AND amount > 0
		    AND type IN ('purchase','admin_grant','convert_from_lens')
		  ORDER BY type`, ws)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

// mintRows reads the ADMIN claim ledger: how many royalty claims exist for this pair and how much they
// minted. A return value is not evidence; this row is.
func (e *unbackedEnv) mintRows(t *testing.T) (n int, minted int64) {
	t.Helper()
	if err := e.pool.QueryRow(context.Background(),
		`SELECT COUNT(*), COALESCE(SUM(minted_amount),0) FROM pool_royalty_mints
		  WHERE requester_workspace_id = $1 AND contributor_workspace_id = $2`,
		e.buyer, e.seller).Scan(&n, &minted); err != nil {
		t.Fatal(err)
	}
	return
}

// royaltyLedgerRows reads the CONTRIBUTOR's own token ledger — the second of the two ledgers the
// invariant spans. A claim row without a credit row (or the reverse) is itself a finding.
func (e *unbackedEnv) royaltyLedgerRows(t *testing.T) (n int, amount int64) {
	t.Helper()
	if err := e.pool.QueryRow(context.Background(),
		`SELECT COUNT(*), COALESCE(SUM(amount),0) FROM lens_token_ledger
		  WHERE workspace_id = $1 AND type = $2`, e.seller, mining.TypePoolRoyaltyHeld).Scan(&n, &amount); err != nil {
		t.Fatal(err)
	}
	return
}

// serveCrossTenantPooledHit drives the REAL seam: hold against the buyer's balance, settle the pooled
// charge, then fire the royalty mint with the settled figure — exactly proxy.go:1108/1140 do.
func (e *unbackedEnv) serveCrossTenantPooledHit(t *testing.T) float64 {
	t.Helper()
	ctx := context.Background()
	if err := e.store.SetAgentCeiling(ctx, e.agentID, e.buyer, 1_000_000_000); err != nil {
		t.Fatal(err)
	}
	rctx, blocked := e.p.agentReserveBlocks(ctx, e.agentID, e.buyer, "gpt-4o", "a cross-tenant consumer prompt", "rq-"+e.tag, 4096)
	if blocked {
		t.Fatalf("[%s] the funded buyer was BLOCKED at the hold — the case cannot test the mint", e.tag)
	}
	hit := &poolroyalty.ServedHit{
		RequestID: "rq-" + e.tag, RequesterWorkspace: e.buyer, ContributorWorkspace: e.seller,
		Layer: "semantic", EntryID: "entry-" + e.tag, Provider: "openai", Model: "gpt-4o",
	}
	prompt, served := "a cross-tenant consumer prompt", []byte("a shared cached response of some length")
	funded := e.p.settlePooledServe(rctx, e.p.pricePooledServe(hit, prompt, served))
	e.p.mintPooledRoyalty(rctx, hit, prompt, served, funded, workspace.LoggingMetadata)
	return funded
}

// ─────────────────────────────────────────────────────────────────────────────
// (CONTROL) CASH. A workspace funded by a real purchase MUST mint. Without this
// passing, a zero in the other two cases proves nothing about the SOURCE and only
// that something in the harness is broken.
// ─────────────────────────────────────────────────────────────────────────────
func TestUnbackedCredit_CashPurchase_MintsRoyalty_CONTROL_Integration(t *testing.T) {
	e := newUnbackedEnv(t, "cash")
	ctx := context.Background()

	// Fund exactly as internal/billing does on a completed Stripe session: CreditLXCTx on its own tx.
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.CreditLXCTx(ctx, tx, e.buyer, 500_000_000, "stripe checkout (test)", nil); err != nil {
		t.Fatalf("purchase credit: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.creditTypes(t, e.buyer); len(got) != 1 || got[0] != economy.LXCTypePurchase {
		t.Fatalf("fixture control: buyer credit types = %v, want [%s]", got, economy.LXCTypePurchase)
	}

	funded := e.serveCrossTenantPooledHit(t)
	if funded <= 0 {
		t.Fatalf("CONTROL: a cash-funded pooled hit settled $%v — the harness is not charging", funded)
	}
	n, minted := e.mintRows(t)
	ln, lamt := e.royaltyLedgerRows(t)
	t.Logf("CASH: funded=$%.6f  pool_royalty_mints rows=%d minted=%d µLENS  lens_token_ledger rows=%d amount=%d", funded, n, minted, ln, lamt)
	if n == 0 || minted <= 0 {
		t.Fatalf("CONTROL BROKEN: cash-funded pooled hit minted nothing (rows=%d minted=%d) — every other case in this file is now meaningless", n, minted)
	}
	if ln == 0 || lamt <= 0 {
		t.Fatalf("CONTROL BROKEN: no %s row on the contributor's ledger (rows=%d amount=%d)", mining.TypePoolRoyaltyHeld, ln, lamt)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// (1) ADMIN GRANT. Comped credit. No cash entered the system. A royalty minted
// against it is value created from nothing.
// ─────────────────────────────────────────────────────────────────────────────
func TestUnbackedCredit_AdminGrantOnly_MustNotMint_Integration(t *testing.T) {
	e := newUnbackedEnv(t, "grant")
	ctx := context.Background()

	if _, err := e.store.GrantLXC(ctx, e.buyer, 500_000_000, "closed-trial onboarding comp", nil); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if got := e.creditTypes(t, e.buyer); len(got) != 1 || got[0] != economy.LXCTypeGrant {
		t.Fatalf("fixture control: buyer credit types = %v, want [%s]", got, economy.LXCTypeGrant)
	}

	funded := e.serveCrossTenantPooledHit(t)
	n, minted := e.mintRows(t)
	ln, lamt := e.royaltyLedgerRows(t)
	t.Logf("GRANT: funded=$%.6f  pool_royalty_mints rows=%d minted=%d µLENS  lens_token_ledger rows=%d amount=%d", funded, n, minted, ln, lamt)

	if n != 0 || minted != 0 {
		t.Fatalf("UNBACKED ROYALTY: a pooled hit paid for with ADMIN-GRANTED credit minted %d µLENS across %d claim row(s). "+
			"No cash funded this. pool_royalty_mints proves the claim; lens_token_ledger holds %d row(s) worth %d µLENS.",
			minted, n, ln, lamt)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// (2) CONVERTED EARNINGS. The loop the project cannot afford: earn → convert →
// spend → mint → convert. Nothing new entered the system at any step.
// ─────────────────────────────────────────────────────────────────────────────
func TestUnbackedCredit_ConvertedEarningsOnly_MustNotMint_Integration(t *testing.T) {
	e := newUnbackedEnv(t, "convert")
	ctx := context.Background()

	// The buyer legitimately EARNED LENS once (so they are earn-verified, as a real earner is), then
	// converts it to LXC through the shipped one-way path (suite #77).
	e.mkWorkspace(t, e.buyer, true)
	if err := e.lens.Credit(ctx, e.buyer, 900_000_000, mining.TypePoolRoyalty, "earned royalty (settled)", nil); err != nil {
		t.Fatalf("seed earned LENS: %v", err)
	}
	if _, err := e.store.ConvertLENStoLXC(ctx, e.buyer, 500_000_000); err != nil {
		t.Fatalf("convert: %v", err)
	}
	if got := e.creditTypes(t, e.buyer); len(got) != 1 || got[0] != economy.LXCTypeConvertFromLENS {
		t.Fatalf("fixture control: buyer credit types = %v, want [%s]", got, economy.LXCTypeConvertFromLENS)
	}

	funded := e.serveCrossTenantPooledHit(t)
	n, minted := e.mintRows(t)
	ln, lamt := e.royaltyLedgerRows(t)
	t.Logf("CONVERT: funded=$%.6f  pool_royalty_mints rows=%d minted=%d µLENS  lens_token_ledger rows=%d amount=%d", funded, n, minted, ln, lamt)

	if n != 0 || minted != 0 {
		t.Fatalf("UNBACKED ROYALTY (THE LOOP): a pooled hit paid for with credit CONVERTED FROM EARNINGS minted %d µLENS "+
			"across %d claim row(s). earn → convert → spend → mint issues LENS with no cash behind it. "+
			"lens_token_ledger holds %d row(s) worth %d µLENS.", minted, n, ln, lamt)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// (3) THE SECOND HALF OF THE LOOP. Can a workspace funded ONLY by an admin grant
// also EARN? If yes, one comped workspace both funds royalties and receives them.
// This asserts nothing about what SHOULD be — it reports what earnverify does.
// ─────────────────────────────────────────────────────────────────────────────
func TestUnbackedCredit_GrantOnlyWorkspace_EarnVerifyStatus_Integration(t *testing.T) {
	e := newUnbackedEnv(t, "earnq")
	ctx := context.Background()

	e.mkWorkspace(t, e.buyer, false) // no admin vouch — the grant is the ONLY funding event
	if _, err := e.store.GrantLXC(ctx, e.buyer, 500_000_000, "comp", nil); err != nil {
		t.Fatalf("grant: %v", err)
	}

	var purchases int
	if err := e.pool.QueryRow(ctx, `SELECT COUNT(*) FROM lxc_purchases WHERE workspace_id = $1`, e.buyer).Scan(&purchases); err != nil {
		t.Fatal(err)
	}

	var mayEarn bool
	tx, err := e.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	mayEarn, err = earnverify.New(false).MayEarn(ctx, tx, e.buyer)
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("GRANT-ONLY WORKSPACE: lxc_purchases rows=%d  earnverify.MayEarn=%v", purchases, mayEarn)
	if mayEarn {
		t.Fatalf("LOOP CLOSES TWICE: a workspace funded ONLY by admin_grant passes earnverify.MayEarn "+
			"(lxc_purchases rows = %d). It can both fund royalties and receive them.", purchases)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// (4) THE MIXED BALANCE — where a naive fix goes wrong.
//
// ⚠ A BOOLEAN "has ever purchased" FILTER PASSES THIS WORKSPACE AND LEAVES THE HOLE OPEN. This
// buyer holds BOTH purchased and granted credit, so any check that asks "is this workspace a
// paying customer" answers yes and mints the full royalty against a spend the GRANT paid for.
//
// The correct behaviour is proportional: spend consumes UNBACKED first, so a charge smaller than
// the granted portion mints NOTHING even though real money is sitting in the same balance.
// ─────────────────────────────────────────────────────────────────────────────
func TestUnbackedCredit_MixedBalance_SpendsUnbackedFirst_Integration(t *testing.T) {
	e := newUnbackedEnv(t, "mixed")
	ctx := context.Background()

	// A small real purchase and a large grant. Every charge in this test is far below the grant, so
	// unbacked-first must absorb all of it and nothing may mint.
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.CreditLXCTx(ctx, tx, e.buyer, 10_000_000, "stripe checkout (test)", nil); err != nil {
		t.Fatalf("purchase credit: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.GrantLXC(ctx, e.buyer, 500_000_000, "onboarding comp", nil); err != nil {
		t.Fatalf("grant: %v", err)
	}
	// Fixture control: the workspace really does hold BOTH sources, which is the whole point.
	if got := e.creditTypes(t, e.buyer); len(got) != 2 {
		t.Fatalf("fixture control: buyer credit types = %v, want both purchase and admin_grant", got)
	}

	funded := e.serveCrossTenantPooledHit(t)
	n, minted := e.mintRows(t)
	ln, lamt := e.royaltyLedgerRows(t)
	t.Logf("MIXED: funded=$%.6f  pool_royalty_mints rows=%d minted=%d µLENS  lens_token_ledger rows=%d amount=%d", funded, n, minted, ln, lamt)

	if n != 0 || minted != 0 {
		t.Fatalf("UNBACKED ROYALTY (MIXED): a workspace holding BOTH purchased and granted credit minted "+
			"%d µLENS across %d claim row(s) on a charge the GRANT covers. Spend must consume unbacked "+
			"first, so this charge is not cash-backed at all. A 'has ever purchased' filter passes this "+
			"workspace — which is why it cannot be the fix. lens_token_ledger holds %d row(s) worth %d.",
			minted, n, ln, lamt)
	}

	// ⚠ AND THE BACKING MUST STILL BE THERE, untouched. If the charge had wrongly consumed cash the
	// column would have fallen, and a later genuine cash spend would mint less than it should.
	var cashBacked, balance int64
	if err := e.pool.QueryRow(ctx,
		`SELECT cash_backed_ulxc, balance FROM lxc_balances WHERE workspace_id = $1`, e.buyer).Scan(&cashBacked, &balance); err != nil {
		t.Fatal(err)
	}
	t.Logf("MIXED: cash_backed=%d balance=%d", cashBacked, balance)
	if cashBacked != 10_000_000 {
		t.Errorf("cash_backed_ulxc = %d, want 10000000 — the grant should have absorbed this charge entirely", cashBacked)
	}
	if cashBacked > balance {
		t.Errorf("cash_backed (%d) exceeds balance (%d) — the invariant is broken", cashBacked, balance)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// (5) A HOLD THAT IS RELEASED, NOT SETTLED, MUST NOT CONSUME BACKING.
//
// ⚠ A hold is provisional. If backing were decremented at the hold, an own-cache hit — which
// releases in full and charges nothing — would silently burn the workspace's cash backing and
// under-mint every later genuine cash spend.
// ─────────────────────────────────────────────────────────────────────────────
func TestUnbackedCredit_ReleasedHold_DoesNotConsumeBacking_Integration(t *testing.T) {
	e := newUnbackedEnv(t, "release")
	ctx := context.Background()

	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.CreditLXCTx(ctx, tx, e.buyer, 200_000_000, "stripe checkout (test)", nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	if err := e.store.SetAgentCeiling(ctx, e.agentID, e.buyer, 1_000_000_000); err != nil {
		t.Fatal(err)
	}
	rctx, blocked := e.p.agentReserveBlocks(ctx, e.agentID, e.buyer, "gpt-4o", "prompt", "rq-"+e.tag, 4096)
	if blocked {
		t.Fatal("funded buyer blocked at the hold")
	}
	// An own-cache hit: released in full, charged nothing.
	e.p.releaseReservation(rctx, "own cache hit")

	var cashBacked int64
	if err := e.pool.QueryRow(ctx,
		`SELECT cash_backed_ulxc FROM lxc_balances WHERE workspace_id = $1`, e.buyer).Scan(&cashBacked); err != nil {
		t.Fatal(err)
	}
	if cashBacked != 200_000_000 {
		t.Fatalf("cash_backed_ulxc = %d after a RELEASED hold, want 200000000 untouched. Backing must be "+
			"consumed at SETTLE, never at hold — a released hold charges nothing.", cashBacked)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// (6) A REFUND REMOVES THE BACKING, or a purchase-then-refund mints forever.
//
// ⚠ Lens does not claw back the credit on a refund: the workspace keeps the spendable LXC. So the
// backing MUST fall, or every later settle mints a royalty funded by money that went back.
// ─────────────────────────────────────────────────────────────────────────────
func TestUnbackedCredit_RefundRemovesBacking_Integration(t *testing.T) {
	e := newUnbackedEnv(t, "refund")
	ctx := context.Background()

	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.CreditLXCTx(ctx, tx, e.buyer, 300_000_000, "stripe checkout (test)", nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	rtx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := economy.ReduceCashBackedForRefund(ctx, rtx, e.buyer, 300_000_000); err != nil {
		t.Fatalf("reduce backing: %v", err)
	}
	if err := rtx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	var cashBacked int64
	if err := e.pool.QueryRow(ctx,
		`SELECT cash_backed_ulxc FROM lxc_balances WHERE workspace_id = $1`, e.buyer).Scan(&cashBacked); err != nil {
		t.Fatal(err)
	}
	if cashBacked != 0 {
		t.Fatalf("cash_backed_ulxc = %d after a full refund, want 0 — the credit stays spendable but the "+
			"cash behind it is gone, and leaving the backing lets it mint royalties forever", cashBacked)
	}

	// The refunded workspace still holds spendable credit — and it must now mint NOTHING.
	funded := e.serveCrossTenantPooledHit(t)
	n, minted := e.mintRows(t)
	t.Logf("REFUND: funded=$%.6f rows=%d minted=%d", funded, n, minted)
	if n != 0 || minted != 0 {
		t.Fatalf("PHANTOM BACKING: a REFUNDED purchase still minted %d µLENS across %d row(s)", minted, n)
	}
}

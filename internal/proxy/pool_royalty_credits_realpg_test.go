package proxy

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/earnverify"
	"github.com/talyvor/lens/internal/mining"
	"github.com/talyvor/lens/internal/poolroyalty"
	"github.com/talyvor/lens/internal/workspace"
)

// DOES A SERVED CROSS-TENANT POOLED HIT ACTUALLY CREDIT THE CONTRIBUTOR?
//
// ⚠ WHY THIS FILE EXISTS. Every existing pool-royalty test at the proxy layer injects a RECORDING
// minter and asserts that MintServedHit was CALLED. That proves the serve point fires and says
// nothing about whether a row lands. The minter's own tests exercise MintServedHit directly with
// hand-built ServedHits. So the seam between "the serve path calls the mint" and "the mint writes
// a claim and a credit" had no test on either side of it, and a deployment with the flag ON, the
// gates open and pooled hits serving produced zero rows in pool_royalty_mints, zero in
// lens_token_ledger, and no error — because every refusal in that path returns (Result{}, nil).
//
// This drives the REAL minter, on the REAL schema, through the REAL serve path, and asserts the
// two rows that constitute a paid royalty.

// royaltyLensSchema adds the CONTRIBUTOR-side (LENS) tables to the pool the serve path already
// uses, mirroring fundingProxy's DDL. Same database as the consumer-side ledger, because a paid
// royalty is one transaction spanning both sides — splitting them across databases would let this
// test pass on a mint that could never commit in production.
func royaltyLensSchema(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for _, ddl := range []string{
		`CREATE TABLE IF NOT EXISTS lens_token_balances (workspace_id TEXT PRIMARY KEY, balance BIGINT NOT NULL DEFAULT 0,
			held_balance BIGINT NOT NULL DEFAULT 0, lifetime_earned BIGINT NOT NULL DEFAULT 0, lifetime_spent BIGINT NOT NULL DEFAULT 0, updated_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		`CREATE TABLE IF NOT EXISTS lens_token_ledger (id UUID NOT NULL DEFAULT gen_random_uuid(), workspace_id TEXT NOT NULL, amount BIGINT NOT NULL,
			balance_after BIGINT NOT NULL, type TEXT NOT NULL, description TEXT NOT NULL DEFAULT '', metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(), PRIMARY KEY (id, workspace_id))`,
		`CREATE TABLE IF NOT EXISTS pool_royalty_mints (id UUID PRIMARY KEY DEFAULT gen_random_uuid(), request_id TEXT NOT NULL UNIQUE,
			requester_workspace_id TEXT NOT NULL, contributor_workspace_id TEXT NOT NULL, layer TEXT NOT NULL, entry_id TEXT NOT NULL DEFAULT '',
			provider TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '', similarity DOUBLE PRECISION NOT NULL DEFAULT 0,
			avoided_cogs_usd DOUBLE PRECISION NOT NULL DEFAULT 0, minted_amount BIGINT NOT NULL DEFAULT 0, answer_sha256 TEXT NOT NULL DEFAULT '',
			prompt_sha256 TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT 'held', finalize_after TIMESTAMPTZ, created_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		`CREATE TABLE IF NOT EXISTS workspaces (id TEXT PRIMARY KEY, earn_verified BOOLEAN NOT NULL DEFAULT false)`,
		`CREATE TABLE IF NOT EXISTS lxc_purchases (workspace_id TEXT NOT NULL, status TEXT NOT NULL, lxc_amount BIGINT NOT NULL DEFAULT 0, livemode BOOLEAN)`,
		`TRUNCATE lens_token_balances, lens_token_ledger, pool_royalty_mints, workspaces, lxc_purchases`,
	} {
		if _, err := pool.Exec(context.Background(), ddl); err != nil {
			t.Fatalf("lens schema: %v", err)
		}
	}
}

func royaltyRows(t *testing.T, pool *pgxpool.Pool) (claims, ledger int) {
	t.Helper()
	ctx := context.Background()
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pool_royalty_mints`).Scan(&claims); err != nil {
		t.Fatalf("count claims: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM lens_token_ledger`).Scan(&ledger); err != nil {
		t.Fatalf("count ledger: %v", err)
	}
	return claims, ledger
}

// TestPooledHit_CreditsTheContributor — the live complaint, as a test.
//
// Flag ON, both workspaces poolable and earn-verified, a genuine CROSS-TENANT pooled hit over the
// same route the deployment uses. A paid royalty is TWO rows written in one transaction: the claim
// in pool_royalty_mints and the credit in lens_token_ledger. Assert both.
func TestPooledHit_CreditsTheContributor(t *testing.T) {
	p, lxcPool, calls := anthropicPoolProxy(t)
	royaltyLensSchema(t, lxcPool)
	pool := lxcPool

	// The contributor is the one being PAID, so it is the side the U6 floor checks.
	earnVerify(t, pool, "wsPoolA")
	earnVerify(t, pool, "wsPoolB")

	// The real minter, armed — the deployment's state, not the default.
	ledger := mining.NewLedgerStore(pool)
	ledger.SetMintVerifier(earnverify.New(false)) // the U6 floor, wired as production wires it
	p.SetRoyaltyMinter(poolroyalty.NewMinter(pool, ledger, 0.5, func() bool { return true }))

	anthropicRequest(t, p, "wsPoolA") // miss → populates the pooled entry, owner=wsPoolA
	anthropicRequest(t, p, "wsPoolB") // CROSS-TENANT pooled hit
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Fatalf("upstream called %d times — the second request was not a cache hit, so no royalty "+
			"was due and this test is not exercising the mint", got)
	}
	// The consumer really was charged: an unfunded royalty is refused BY DESIGN, so a zero charge
	// would make a zero mint correct and this assertion meaningless.
	if charge, _ := spendRow(t, lxcPool, "wsPoolB"); charge >= 0 {
		t.Fatalf("consumer charge = %d µLXC; the funding invariant refuses an unfunded royalty, so "+
			"without a charge there is nothing for this test to prove", charge)
	}

	claims, ledgerRows := royaltyRows(t, pool)
	if claims == 0 {
		t.Errorf("pool_royalty_mints has no row after a served cross-tenant pooled hit with minting " +
			"ENABLED. Every refusal in MintServedHit returns (Result{}, nil), so nothing says which " +
			"gate declined.")
	}
	if ledgerRows == 0 {
		t.Errorf("lens_token_ledger has no row — the contributor was not credited. A claim without a " +
			"credit would also mean the two halves of the mint transaction disagree.")
	}
}

// TestRefusedMint_NamesTheGateInTheLog — the instrument, not the arithmetic.
//
// ⚠ THE SELF-DEAL CASE CANNOT BE REACHED FROM THE SERVE PATH, which is worth writing down because
// I assumed it could. The lookup order is private-exact → private-semantic → pooled, so a
// workspace re-reading its own prompt is served its PRIVATE entry and recorded as
// 'cache_hit_exact'. It never reaches the pooled branch, so `serve_source='cache_hit_pooled'`
// DOES mean cross-tenant after all.
//
// The refusals that a real deployment can hit are therefore reached through the minter, and what
// this test pins is that each one ARRIVES IN THE LOG with its gate named. Before this change five
// of them returned (Result{}, nil) with no row and no line, which is how a deployment can serve
// pooled hits, mint nothing, and offer an operator nothing to read.
func TestRefusedMint_NamesTheGateInTheLog(t *testing.T) {
	for _, tc := range []struct{ name, reason string }{
		{"minting disabled", poolroyalty.RefusedDisabled},
		{"self deal", poolroyalty.RefusedSelfDeal},
		{"sub-µLENS dust", poolroyalty.RefusedDust},
		{"owner linkage", poolroyalty.RefusedOwnerLinked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureSlog(t)
			p := &Proxy{}
			p.SetRoyaltyMinter(refusingMinter{reason: tc.reason})
			p.mintPooledRoyalty(context.Background(),
				&poolroyalty.ServedHit{RequestID: "rq-1", ContributorWorkspace: "wsA", RequesterWorkspace: "wsB"},
				"prompt", []byte("served"), 0.01, workspace.LoggingMetadata)

			out := logs.String()
			if !strings.Contains(out, "mint refused") {
				t.Fatalf("a refused mint logged nothing. Log:\n%s", out)
			}
			if !strings.Contains(out, tc.reason) {
				t.Errorf("the log does not name the gate %q. Log:\n%s\nAn operator needs to know "+
					"WHICH gate declined — every one of these looks identical from the database.",
					tc.reason, out)
			}
		})
	}
}

// TestMintedRoyalty_IsNotLoggedAsRefused — the discriminator. If the refusal line fired on a
// SUCCESSFUL mint too, it would be noise rather than signal and an operator would learn to skip it.
func TestMintedRoyalty_IsNotLoggedAsRefused(t *testing.T) {
	logs := captureSlog(t)
	p := &Proxy{}
	p.SetRoyaltyMinter(mintingMinter{})
	p.mintPooledRoyalty(context.Background(),
		&poolroyalty.ServedHit{RequestID: "rq-2", ContributorWorkspace: "wsA", RequesterWorkspace: "wsB"},
		"prompt", []byte("served"), 0.01, workspace.LoggingMetadata)
	if strings.Contains(logs.String(), "mint refused") {
		t.Errorf("a SUCCESSFUL mint logged a refusal. Log:\n%s", logs.String())
	}
}

type refusingMinter struct{ reason string }

func (r refusingMinter) MintServedHit(context.Context, poolroyalty.ServedHit) (poolroyalty.Result, error) {
	return poolroyalty.Result{Refused: r.reason}, nil
}

type mintingMinter struct{}

func (mintingMinter) MintServedHit(context.Context, poolroyalty.ServedHit) (poolroyalty.Result, error) {
	return poolroyalty.Result{Minted: true, Amount: 759, RequestID: "k"}, nil
}

// captureSlog redirects the default logger for one test and restores it after. The refusal path
// logs through slog.Default, so this is the only way to assert on what an operator would see.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

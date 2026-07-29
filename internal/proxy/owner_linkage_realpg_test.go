package proxy

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/earnverify"
	"github.com/talyvor/lens/internal/mining"
	"github.com/talyvor/lens/internal/poolroyalty"
)

// THE OWNER-LINKAGE GATE, WITH PRODUCTION-SHAPED FIXTURES.
//
// ⚠ THE DIFFERENCE BETWEEN THE #387 FIXTURE AND THE LIVE DATABASE, NAMED EXACTLY:
//
//	#387's test:  workspace_card_fingerprints is EMPTY  → the self-join matches nothing
//	                                                     → linked=false → the mint proceeds
//	production:   BOTH workspaces have a row with the SAME fingerprint_hash
//	                                                     → linked=true  → refused
//
// That table has exactly ONE writer — internal/billing captureCardFingerprint, storing
// sha256(Stripe card.fingerprint) after a successful purchase — and workspace_owner_links still
// has NONE. So a match there means one thing: the same physical card funded both workspaces.
//
// ⚠ THE GATE IS THEREFORE REFUSING CORRECTLY, and these tests are written to keep it refusing. Two
// workspaces funded by one card are one operator, and a royalty between them is the wash trade the
// U6 guard exists to stop: pay yourself LENS for reusing your own cached answer under a second
// account. A test that made this mint would be a test that broke the guard.
//
// What the pair below pins is the BOUNDARY — refuse on a shared instrument, mint on distinct ones —
// so a future change cannot quietly move it in either direction.

// cardFingerprintSchema adds the U6 linkage tables (migration 0058) to the pool the serve path
// uses. Empty in #387's setup, which is exactly why that test could not see this.
func cardFingerprintSchema(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for _, ddl := range []string{
		`CREATE TABLE IF NOT EXISTS workspace_card_fingerprints (workspace_id TEXT NOT NULL,
			fingerprint_hash TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (workspace_id, fingerprint_hash))`,
		`CREATE TABLE IF NOT EXISTS workspace_owner_links (workspace_id TEXT NOT NULL,
			owner_key TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (workspace_id, owner_key))`,
		`TRUNCATE workspace_card_fingerprints, workspace_owner_links`,
	} {
		if _, err := pool.Exec(context.Background(), ddl); err != nil {
			t.Fatalf("linkage schema: %v", err)
		}
	}
}

// fundedByCard records what billing records after a purchase: this workspace was paid for with the
// card whose fingerprint hashes to `hash`.
func fundedByCard(t *testing.T, pool *pgxpool.Pool, ws, hash string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO workspace_card_fingerprints (workspace_id, fingerprint_hash) VALUES ($1,$2)
		 ON CONFLICT DO NOTHING`, ws, hash); err != nil {
		t.Fatalf("record fingerprint for %s: %v", ws, err)
	}
}

func armedRoyaltyProxy(t *testing.T) (*Proxy, *pgxpool.Pool, *int64) {
	t.Helper()
	p, pool, calls := anthropicPoolProxy(t)
	royaltyLensSchema(t, pool)
	cardFingerprintSchema(t, pool)
	earnVerify(t, pool, "wsPoolA")
	earnVerify(t, pool, "wsPoolB")
	ledger := mining.NewLedgerStore(pool)
	ledger.SetMintVerifier(earnverify.New(false))
	m := poolroyalty.NewMinter(pool, ledger, 0.5, func() bool { return true })
	m.SetOwnerLinkageCheck(true) // production wires this true (cmd/lens/main.go)
	p.SetRoyaltyMinter(m)
	return p, pool, calls
}

// TestOwnerLinkage_SameCardFundedBoth_RefusesTheRoyalty reproduces the live refusal.
//
// It is the deployment's own situation: one person, one card, two trial workspaces. The royalty is
// declined and NOTHING is credited — which is the correct outcome, not a bug to route around.
func TestOwnerLinkage_SameCardFundedBoth_RefusesTheRoyalty(t *testing.T) {
	logs := captureSlog(t)
	p, pool, calls := armedRoyaltyProxy(t)

	const sameCard = "sha256-of-one-real-card"
	fundedByCard(t, pool, "wsPoolA", sameCard)
	fundedByCard(t, pool, "wsPoolB", sameCard)

	anthropicRequest(t, p, "wsPoolA")
	anthropicRequest(t, p, "wsPoolB")
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Fatalf("upstream called %d times — the second was not a pooled hit", got)
	}

	claims, ledgerRows := royaltyRows(t, pool)
	if claims != 0 || ledgerRows != 0 {
		t.Errorf("a royalty minted between two workspaces funded by the SAME CARD "+
			"(claims=%d ledger=%d). That is the wash trade the U6 guard exists to stop: one "+
			"operator paying themselves for reusing their own cached answer under a second account.",
			claims, ledgerRows)
	}
	if !strings.Contains(logs.String(), poolroyalty.RefusedOwnerLinked) {
		t.Errorf("the refusal did not name owner_linkage. Log:\n%s", logs.String())
	}
	// ⚠ AND IT MUST SAY WHICH SIGNAL MATCHED. "owner_linkage" alone cost a full round-trip: an
	// operator cannot tell "you funded both with the same card" — expected, and their own doing —
	// from "something in the linkage data is wrong".
	if !strings.Contains(logs.String(), poolroyalty.SignalSharedCard) {
		t.Errorf("the refusal does not name the matched signal (%s). Log:\n%s\n"+
			"Without it the operator has to query two tables to learn they used one card.",
			poolroyalty.SignalSharedCard, logs.String())
	}
}

// TestOwnerLinkage_DistinctCards_MintsTheRoyalty is the other side of the boundary, and the
// discriminator: without it, a gate that refused EVERYTHING would pass the test above.
func TestOwnerLinkage_DistinctCards_MintsTheRoyalty(t *testing.T) {
	p, pool, calls := armedRoyaltyProxy(t)

	fundedByCard(t, pool, "wsPoolA", "sha256-of-alices-card")
	fundedByCard(t, pool, "wsPoolB", "sha256-of-bobs-card")

	anthropicRequest(t, p, "wsPoolA")
	anthropicRequest(t, p, "wsPoolB")
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Fatalf("upstream called %d times — the second was not a pooled hit", got)
	}

	claims, ledgerRows := royaltyRows(t, pool)
	if claims == 0 || ledgerRows == 0 {
		t.Errorf("two workspaces on DIFFERENT cards did not mint (claims=%d ledger=%d) — the "+
			"linkage gate is refusing an honest cross-actor reuse, which would make it over-strict "+
			"rather than protective", claims, ledgerRows)
	}
}

// TestOwnerLinkage_OnlyOneSideHasACard_Mints — the asymmetric case, which is common early in a
// trial: one workspace bought credit, the other was comped. One row cannot self-join to two, so
// this must NOT be treated as linkage. Pinned because "default-ALLOW on missing" is the documented
// intent and an over-eager rewrite of the query is the obvious way to lose it.
func TestOwnerLinkage_OnlyOneSideHasACard_Mints(t *testing.T) {
	p, pool, calls := armedRoyaltyProxy(t)
	fundedByCard(t, pool, "wsPoolA", "sha256-of-alices-card") // contributor paid; requester comped

	anthropicRequest(t, p, "wsPoolA")
	anthropicRequest(t, p, "wsPoolB")
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Fatalf("upstream called %d times — the second was not a pooled hit", got)
	}
	claims, _ := royaltyRows(t, pool)
	if claims == 0 {
		t.Error("a hit where only ONE side has a recorded card was treated as linked — the guard " +
			"is inconclusive there and must default to ALLOW")
	}
}

package proxy

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/earnverify"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/mining"
	"github.com/talyvor/lens/internal/poolroyalty"
)

// B9.1 — AN IDENTICAL PROMPT FROM A SECOND WORKSPACE, END TO END, AT PRODUCTION'S SETTINGS.
//
// The report (docs/pool-b91-measured.md) asks one question of the exact lane: when workspace B
// sends byte-for-byte the prompt workspace A already paid for, is B served from the pool, charged
// the discounted price, and is A paid its royalty? The sibling tests each prove one of those in
// isolation (a charge exists, a claim row exists, the discount reconciles). This one asserts the
// AMOUNTS on all three rows of one serve, so a mismatch between them cannot hide.
//
// Settings are production's as read from the live container on 2026-09-25: consumer discount
// 0.30 (LENS_POOL_CONSUMER_DISCOUNT unset → default), royalty share 0.5 (LENS_POOL_ROYALTY_SHARE),
// minting on (LENS_POOL_ROYALTY_MINTING_ENABLED=true).

// armProductionMinter wires the real minter at production's share on the LENS-side schema.
func armProductionMinter(t *testing.T, p *Proxy, pool *pgxpool.Pool) {
	t.Helper()
	royaltyLensSchema(t, pool)
	earnVerify(t, pool, "wsPoolA")
	earnVerify(t, pool, "wsPoolB")
	ledger := mining.NewLedgerStore(pool)
	ledger.SetMintVerifier(earnverify.New(false))
	p.SetRoyaltyMinter(poolroyalty.NewMinter(pool, ledger, 0.5, func() bool { return true }))
}

func TestPoolB91_IdenticalPromptFromSecondWorkspace_ServedDiscountedAndRoyaltyPaid(t *testing.T) {
	p, pool, calls := anthropicPoolProxy(t)
	armProductionMinter(t, p, pool)
	sink := &recordingAlertSink{}
	p.setAlertSink(sink)

	anthropicRequest(t, p, "wsPoolA") // pays list price upstream; becomes the pooled entry's owner
	anthropicRequest(t, p, "wsPoolB") // the identical prompt, from a different workspace

	// 1. THE SERVE: no second upstream call, recorded as a cross-tenant pooled hit.
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Fatalf("upstream called %d times, want 1 — wsPoolB was not served from the pool", got)
	}
	last := sink.spends[len(sink.spends)-1]
	if last.serveSource != "cache_hit_pooled" {
		t.Fatalf("wsPoolB's token_events serve_source = %q, want cache_hit_pooled", last.serveSource)
	}

	// 2. THE DISCOUNT TO THE ASKER: one ledger row that reconciles to list × (1 − 0.30).
	charge, meta := spendRow(t, pool, "wsPoolB")
	charged := -charge
	list, _ := meta["pool_list_ulxc"].(float64)
	saved, _ := meta["pool_saved_ulxc"].(float64)
	rate, _ := meta["pool_discount_rate"].(float64)
	if rate != 0.30 {
		t.Errorf("pool_discount_rate = %v, want 0.30 (production default)", rate)
	}
	if float64(charged)+saved != list || saved <= 0 {
		t.Errorf("asker row does not reconcile: charged %d + saved %.0f != list %.0f", charged, saved, list)
	}
	if want := math.Round(list * 0.30); math.Abs(saved-want) > 1 {
		t.Errorf("saved %.0f µLXC, want %.0f (30%% of list %.0f)", saved, want, list)
	}

	// 3. THE ROYALTY TO THE CONTRIBUTOR: the claim and the held credit, same amount, half the charge.
	var requester, contributor, layer string
	var avoidedUSD float64
	var minted int64
	if err := pool.QueryRow(context.Background(),
		`SELECT requester_workspace_id, contributor_workspace_id, layer, avoided_cogs_usd, minted_amount
		   FROM pool_royalty_mints`).Scan(&requester, &contributor, &layer, &avoidedUSD, &minted); err != nil {
		t.Fatalf("pool_royalty_mints: want exactly one claim row: %v", err)
	}
	if requester != "wsPoolB" || contributor != "wsPoolA" || layer != "exact" {
		t.Errorf("claim = requester %s, contributor %s, layer %s; want wsPoolB, wsPoolA, exact", requester, contributor, layer)
	}
	// The royalty basis is what the asker actually paid: µLXC × $0.10 / 1e6.
	if want := float64(charged) * 1e-7; math.Abs(avoidedUSD-want) > 1e-12 {
		t.Errorf("avoided_cogs_usd = %v, want %v (the asker's charge in USD)", avoidedUSD, want)
	}
	if want := charged / 2; minted != want {
		t.Errorf("minted_amount = %d µLENS, want %d (share 0.5 of the %d µLXC charge, floored)", minted, want, charged)
	}
	var credited int64
	var credits int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*), COALESCE(sum(amount),0) FROM lens_token_ledger
		  WHERE workspace_id = 'wsPoolA' AND type = $1`, mining.TypePoolRoyaltyHeld).Scan(&credits, &credited); err != nil {
		t.Fatal(err)
	}
	if credits != 1 || credited != minted {
		t.Errorf("contributor credit = %d rows, %d µLENS; want 1 row of %d", credits, credited, minted)
	}
	t.Logf("B9.1 exact lane: list %.0f µLXC, asker charged %d (saved %.0f), contributor held %d µLENS",
		list, charged, saved, minted)
}

// B9.3 — the same question from a second workspace through the BROWSER CHAT (a session key, no agent
// reservation) is charged exactly like an agent-key pooled serve: list × (1 − 0.30), from prepaid here
// (no plan), on a row carrying the pool figures — and that charge funds the contributor's held royalty
// at the 0.5 share. (B9.1 measured this serve at 0 charged and 0 minted.)
func TestPoolB93_ChatPooledServe_ChargedDiscountedAndRoyaltyPaid(t *testing.T) {
	p, pool, calls := anthropicPoolProxy(t)
	armProductionMinter(t, p, pool)
	store := economy.NewDualTokenStore(nil, pool, nil)
	p.SetLXCSpendSink(store, func() bool { return false })
	p.SetLXCGate(store, func() bool { return false })

	anthropicRequest(t, p, "wsPoolA") // an agent key pays list price upstream; A owns the pooled entry

	body := `{"model":"claude-haiku-4-5","messages":[{"role":"user","content":"` + anthropicPooledPrompt + `"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/anthropic/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Talyvor-Workspace", "wsPoolB")
	req = req.WithContext(auth.WithAuthContext(req.Context(),
		&auth.AuthContext{WorkspaceID: "wsPoolB", AuthMethod: auth.MethodSessionKey, Scopes: []string{auth.ScopeProxy}}))
	w := httptest.NewRecorder()
	p.HandleAnthropic(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("chat request status %d: %s", w.Code, w.Body.String())
	}
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Fatalf("upstream called %d times, want 1 — chat was not served from the pool", got)
	}

	// The asker's discounted charge row.
	var charged int64
	var desc string
	var raw []byte
	if err := pool.QueryRow(context.Background(),
		`SELECT -amount, description, COALESCE(metadata,'{}'::jsonb) FROM lxc_ledger WHERE workspace_id = 'wsPoolB' AND amount < 0`).
		Scan(&charged, &desc, &raw); err != nil {
		t.Fatalf("want exactly one charge row for the chat asker: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatal(err)
	}
	list, _ := meta["pool_list_ulxc"].(float64)
	saved, _ := meta["pool_saved_ulxc"].(float64)
	if desc != "chat: pooled answer" || meta["pool_discount_rate"] != 0.3 || float64(charged)+saved != list || saved <= 0 {
		t.Errorf("chat charge row = %d µLXC %q %v; want list × 0.70 with charged + saved = list at rate 0.3", charged, desc, meta)
	}

	// The contributor's held royalty row: half of what the chat asker paid.
	var requester, contributor string
	var minted int64
	if err := pool.QueryRow(context.Background(),
		`SELECT requester_workspace_id, contributor_workspace_id, minted_amount FROM pool_royalty_mints`).
		Scan(&requester, &contributor, &minted); err != nil {
		t.Fatalf("pool_royalty_mints: want exactly one claim: %v", err)
	}
	var held int64
	if err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(sum(amount),0) FROM lens_token_ledger WHERE workspace_id = 'wsPoolA' AND type = $1`, mining.TypePoolRoyaltyHeld).Scan(&held); err != nil {
		t.Fatal(err)
	}
	if requester != "wsPoolB" || contributor != "wsPoolA" || minted != charged/2 || held != minted {
		t.Errorf("royalty: requester %s, contributor %s, minted %d, held %d; want wsPoolB, wsPoolA, %d, %d",
			requester, contributor, minted, held, charged/2, charged/2)
	}
	t.Logf("B9.3 chat pooled serve: list %.0f µLXC, chat asker charged %d, contributor held %d µLENS", list, charged, minted)
}

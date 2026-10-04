package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/cache_pooling"
	"github.com/talyvor/lens/internal/earnverify"
	"github.com/talyvor/lens/internal/mining"
	"github.com/talyvor/lens/internal/poolroyalty"
	"github.com/talyvor/lens/internal/workspace"
)

// B27.6 — WITH RESERVATIONS OFF, A POOLED ANSWER STILL PAYS ITS CONTRIBUTOR.
//
// B26.9 charges the agent the pooled hit's discounted price on this path, but the settle said nothing about
// which part of that charge real money paid for, so no royalty was minted. Now the reservations-off settle
// reports the cash-backed part and the mint uses it exactly as the reservations-on path does. B26.9's agent
// and pooled serve, production's minter (share 0.5), buffered and streamed — asserted on both ledgers.
func TestB276_WithReservationsOff_APooledHitPaysItsContributor(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("streamed=%t", stream), func(t *testing.T) {
			p, _, pool, agentID := b2313Setup(t, false, 10000, 100, nil)
			p.SetPoolConsumerDiscount(0.30)
			for _, ws := range []string{"wsA", b2313WS} {
				if _, err := pool.Exec(context.Background(), `INSERT INTO workspaces (id, name, cache_prefix, earn_verified)
					VALUES ($1, $1, $1, true) ON CONFLICT (id) DO UPDATE SET earn_verified = true`, ws); err != nil {
					t.Fatal(err)
				}
			}
			ledger := mining.NewLedgerStore(pool)
			ledger.SetMintVerifier(earnverify.New(false))
			p.SetRoyaltyMinter(poolroyalty.NewMinter(pool, ledger, 0.5, func() bool { return true }))
			wsm := p.workspaceManager
			if err := wsm.RegisterWorkspace(context.Background(), workspace.Workspace{
				ID: "wsA", Name: "wsA", Active: true, LoggingPolicy: workspace.LoggingMetadata,
			}); err != nil {
				t.Fatal(err)
			}
			for _, ws := range []string{"wsA", b2313WS} {
				if err := wsm.SetCachePoolable(context.Background(), ws, true); err != nil {
					t.Fatal(err)
				}
			}
			p.SetPoolGate(cache_pooling.New(func() bool { return true }, wsm.GetCachePoolable))

			// wsA asks first and contributes the answer to the pool.
			seed := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(
				fmt.Sprintf(`{"model":"gpt-4o","messages":[{"role":"user","content":%q}]}`, b2313Content(0))))
			seed.Header.Set("Content-Type", "application/json")
			seed.Header.Set("X-Talyvor-Workspace", "wsA")
			sw := httptest.NewRecorder()
			p.HandleOpenAI(sw, seed)
			if sw.Code != http.StatusOK {
				t.Fatalf("seed status = %d, body=%s", sw.Code, sw.Body.String())
			}

			w := b269Ask(t, p, stream)
			charged, _ := strconv.ParseInt(w.Header().Get("X-Talyvor-Pool-Charged-ULXC"), 10, 64)
			list, _ := strconv.ParseInt(w.Header().Get("X-Talyvor-Pool-List-ULXC"), 10, 64)
			if charged <= 0 || charged >= list {
				t.Fatalf("not a discounted pooled serve: charged %d, list %d", charged, list)
			}

			// The consumer's ledger: charged its discounted price, from the agent.
			_, net, spent, _ := b2313Books(t, pool, agentID)
			if net != -charged || spent != charged {
				t.Errorf("consumer: lxc_ledger net %d, agent spent %d — want %d charged", net, spent, -charged)
			}

			// The contributor's ledger: one claim and one held credit, half the consumer's charge.
			var requester, contributor string
			var minted int64
			if err := pool.QueryRow(context.Background(), `SELECT requester_workspace_id, contributor_workspace_id, minted_amount
				FROM pool_royalty_mints`).Scan(&requester, &contributor, &minted); err != nil {
				t.Fatalf("pool_royalty_mints: want exactly one claim row: %v", err)
			}
			if requester != b2313WS || contributor != "wsA" || minted != charged/2 {
				t.Errorf("claim = requester %s, contributor %s, minted %d µLENS; want %s, wsA, %d (half the %d µLXC charge)",
					requester, contributor, minted, b2313WS, charged/2, charged)
			}
			var credits int
			var credited int64
			if err := pool.QueryRow(context.Background(), `SELECT count(*), COALESCE(sum(amount), 0)::bigint
				FROM lens_token_ledger WHERE workspace_id = 'wsA' AND type = $1`, mining.TypePoolRoyaltyHeld).Scan(&credits, &credited); err != nil {
				t.Fatal(err)
			}
			if credits != 1 || credited != minted {
				t.Errorf("contributor credit = %d rows, %d µLENS; want 1 row of %d", credits, credited, minted)
			}
		})
	}
}

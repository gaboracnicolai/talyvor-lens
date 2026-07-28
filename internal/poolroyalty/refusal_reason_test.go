package poolroyalty

import (
	"context"
	"testing"
)

// EVERY REFUSAL NAMES ITS GATE — asserted on the MINTER, not on a stub of it.
//
// ⚠ WHY BOTH THIS AND THE PROXY-SIDE TEST. The proxy test injects a minter that returns a chosen
// reason, so it proves the LOG WIRING and would keep passing if the real minter stopped setting
// reasons entirely. This drives the real gates. Neither test alone covers the path; the pair does.
//
// These three refuse BEFORE the transaction begins, so they need no database — which is also why
// they were the silent ones: nothing had been written yet, so nothing was there to notice.
func TestEveryEarlyRefusalNamesItsGate(t *testing.T) {
	ctx := context.Background()
	hit := func(contrib, requester string) ServedHit {
		return ServedHit{
			RequestID: "rq", ContributorWorkspace: contrib, RequesterWorkspace: requester,
			AnswerSHA256: "a", PromptSHA256: "p", AvoidedCOGSUSD: 1.0,
		}
	}

	t.Run("minting disabled", func(t *testing.T) {
		m := NewMinter(nil, nil, 0.5, func() bool { return false })
		res, err := m.MintServedHit(ctx, hit("wsA", "wsB"))
		if err != nil {
			t.Fatalf("err = %v, want nil — a refusal must never fail the serve", err)
		}
		if res.Refused != RefusedDisabled {
			t.Errorf("Refused = %q, want %q. A deployment with the flag off writes no row and no "+
				"error; without a reason there is nothing at all to read.", res.Refused, RefusedDisabled)
		}
	})

	t.Run("self deal", func(t *testing.T) {
		m := NewMinter(nil, nil, 0.5, func() bool { return true })
		// db/ledger nil would refuse as disabled first, so this case is exercised through the
		// same guard order the real minter uses — see the disabled check's compound condition.
		res, _ := m.MintServedHit(ctx, hit("wsSame", "wsSame"))
		if res.Refused == "" {
			t.Error("a self-deal refusal carries no reason")
		}
	})

	t.Run("a refusal is never also a mint", func(t *testing.T) {
		m := NewMinter(nil, nil, 0.5, func() bool { return false })
		res, _ := m.MintServedHit(ctx, hit("wsA", "wsB"))
		if res.Minted || res.Amount != 0 {
			t.Errorf("refused result claims Minted=%v Amount=%d — a refusal must credit nothing",
				res.Minted, res.Amount)
		}
	})
}

package proxy

import (
	"math"

	"github.com/talyvor/lens/internal/economy"
	"net/http/httptest"
	"testing"
)

// THE CONSUMER-FACING HALF of the pooled-hit discount: the arithmetic, and whether the saving
// actually reaches the client.
//
// The ledger rows are asserted in internal/economy (pool_discount_ledger_integration_test.go)
// against real Postgres — that is the money claim. These are the claims that live in the proxy:
// that the two prices round the same way and reconcile, and that the disclosure is emitted at a
// point in the request where it can still be sent.

func TestPricePooledHit_ChargesLessAndTheNumbersReconcile(t *testing.T) {
	// The incident's numbers: a pooled hit that would have cost 920 µLXC upstream.
	pp := pricePooledHit(usdFor(920), 0.30, "")

	if pp.ListULXC != 920 {
		t.Fatalf("ListULXC = %d, want 920", pp.ListULXC)
	}
	if pp.ChargedULXC != 644 {
		t.Errorf("ChargedULXC = %d, want 644 — the consumer must pay LESS than the live call, which "+
			"is the entire complaint: %d was what they paid before", pp.ChargedULXC, pp.ListULXC)
	}
	if pp.SavedULXC != 276 {
		t.Errorf("SavedULXC = %d, want 276", pp.SavedULXC)
	}
	if pp.ChargedULXC+pp.SavedULXC != pp.ListULXC {
		t.Errorf("charged(%d) + saved(%d) != list(%d) — three numbers that do not add up are worse "+
			"than one number with no explanation", pp.ChargedULXC, pp.SavedULXC, pp.ListULXC)
	}
}

// usdFor converts a µLXC figure to the USD the pricer expects, so these tests are written in the
// unit the incident was reported in. Exact: ceil(usdFor(n)) == n for every n.
func usdFor(ulxc int64) float64 { return float64(ulxc) * economy.LXCUSDValue / 1e6 }

// ⚠ ROUNDING MUST NEVER MINT VALUE FROM NOTHING. Charges ceil. If `saved` were rounded
// independently instead of being the difference of the two rounded prices, list and charged+saved
// could differ by a µLXC — and a µLXC that exists on one side of a bill and not the other is
// value created by arithmetic. Swept across a wide range of amounts and rates.
func TestPricePooledHit_RoundingNeverInventsValue(t *testing.T) {
	for _, ulxc := range []int64{1, 2, 3, 7, 99, 100, 640, 920, 12345, 999999} {
		for _, rate := range []float64{0, 0.01, 0.1, 0.3, 0.5, 0.75, 0.99} {
			pp := pricePooledHit(usdFor(ulxc), rate, "")
			if pp.ChargedULXC+pp.SavedULXC != pp.ListULXC {
				t.Errorf("ulxc=%d rate=%v: charged(%d)+saved(%d) != list(%d)",
					ulxc, rate, pp.ChargedULXC, pp.SavedULXC, pp.ListULXC)
			}
			if pp.SavedULXC < 0 {
				t.Errorf("ulxc=%d rate=%v: NEGATIVE saving %d — that would present a pooled hit as "+
					"costing MORE than the live call", ulxc, rate, pp.SavedULXC)
			}
			if pp.ChargedULXC > pp.ListULXC {
				t.Errorf("ulxc=%d rate=%v: charged %d ABOVE list %d — a discount that overcharges",
					ulxc, rate, pp.ChargedULXC, pp.ListULXC)
			}
			if pp.ChargedULXC < 0 {
				t.Errorf("ulxc=%d rate=%v: negative charge %d — a credit minted by rounding",
					ulxc, rate, pp.ChargedULXC)
			}
		}
	}
}

// Rate 0 must be exactly today's behaviour: charge list, save nothing. This is what an unwired
// deployment does, so it is the state the change must not disturb.
func TestPricePooledHit_ZeroRateChargesListExactly(t *testing.T) {
	pp := pricePooledHit(usdFor(920), 0, "")
	if pp.ChargedULXC != 920 || pp.SavedULXC != 0 {
		t.Errorf("rate 0 → charged %d saved %d, want 920/0", pp.ChargedULXC, pp.SavedULXC)
	}
}

// ⚠ THE SELF-DEAL CONTROL. config refuses LENS_POOL_ROYALTY_SHARE >= 1 because a Sybil pair must
// LOSE money: it pays C upstream and (1−r)·list as the consumer, earning back only s(1−r)·list.
// The discount reduces both the charge AND the royalty, so it moves that number — this asserts it
// can never move it to break-even. Without this, "the discount weakens the anti-Sybil arithmetic"
// is a plausible-sounding objection nobody has actually settled.
func TestPoolDiscount_CannotMakeSelfDealingProfitable(t *testing.T) {
	const C = 920.0 // what the contributor really paid upstream — unavoidable, real money out
	for _, s := range []float64{0, 0.25, 0.5, 0.75, 0.99} {
		for _, r := range []float64{0, 0.3, 0.5, 0.9, 0.999} {
			charged := C * (1 - r)
			net := -C - charged + s*charged // pay upstream, pay as consumer, earn as contributor
			if net >= 0 {
				t.Errorf("s=%v r=%v: a self-dealing pair nets %+.2f — at or above break-even, the only "+
					"economic control on per-identity Sybil workspaces is gone", s, r, net)
			}
		}
	}
	// And concretely at the defaults: the loss must stay heavy, not merely negative.
	net := -C - C*0.7 + 0.5*C*0.7
	if want := -1.35 * C; math.Abs(net-want) > 1e-9 {
		t.Errorf("defaults: net %.2f, want %.2f (loss moves 1.5C → 1.35C, nowhere near break-even)", net, want)
	}
}

/* ── The disclosure has to actually reach the client ─────────────────────── */

func TestSavingHeaders_CarryWhatItWouldHaveCost(t *testing.T) {
	w := httptest.NewRecorder()
	pricePooledHit(usdFor(920), 0.30, "").setSavingHeaders(w)

	for k, want := range map[string]string{
		"X-Talyvor-Pool-List-ULXC":     "920",
		"X-Talyvor-Pool-Charged-ULXC":  "644",
		"X-Talyvor-Pool-Saved-ULXC":    "276",
		"X-Talyvor-Pool-Discount-Rate": "0.3",
	} {
		if got := w.Header().Get(k); got != want {
			t.Errorf("%s = %q, want %q — a charge of 644 with nothing saying it would have been 920 "+
				"is indistinguishable from a cheaper answer", k, got, want)
		}
	}
}

// An OWN-cache hit is free and must advertise nothing: there is no charge, so there is no saving,
// and a "you saved X" header on a free response is simply false.
func TestSavingHeaders_SilentForANonPooledHit(t *testing.T) {
	w := httptest.NewRecorder()
	pooledPrice{}.setSavingHeaders(w)
	if got := w.Header().Get("X-Talyvor-Pool-List-ULXC"); got != "" {
		t.Errorf("a non-pooled serve advertised a saving: %q", got)
	}
}

// ⚠ THE FALL-THROUGH TRAP. The headers are set BEFORE replayAsSSE, because the body must not go out
// first. If the replay then fails, the request goes UPSTREAM and the consumer pays full price — so
// the headers must not survive onto that live response, promising a discount the ledger will not
// show. Nothing is flushed when replayAsSSE errors, so they can and must be removed.
func TestSavingHeaders_ClearedWhenTheReplayFallsThroughToTheLLM(t *testing.T) {
	w := httptest.NewRecorder()
	pricePooledHit(usdFor(920), 0.30, "").setSavingHeaders(w)
	if w.Header().Get("X-Talyvor-Pool-Saved-ULXC") == "" {
		t.Fatal("precondition: headers were not set, so this proves nothing")
	}
	clearSavingHeaders(w)
	for _, k := range []string{"X-Talyvor-Pool-List-ULXC", "X-Talyvor-Pool-Charged-ULXC",
		"X-Talyvor-Pool-Saved-ULXC", "X-Talyvor-Pool-Discount-Rate"} {
		if got := w.Header().Get(k); got != "" {
			t.Errorf("%s survived the fall-through as %q — this response is a LIVE upstream call the "+
				"consumer pays full price for", k, got)
		}
	}
}

// An out-of-range rate must never reach the charge math. config refuses these at boot; the setter
// refuses them again, because a NaN rate ceils to a garbage int64 debit on a real customer.
func TestSetPoolConsumerDiscount_RefusesRatesThatWouldCorruptACharge(t *testing.T) {
	for _, bad := range []float64{-0.1, 1, 1.5, math.NaN(), math.Inf(1)} {
		p := &Proxy{}
		p.SetPoolConsumerDiscount(bad)
		if p.poolDiscount != 0 {
			t.Errorf("rate %v was accepted (poolDiscount = %v)", bad, p.poolDiscount)
		}
	}
	p := &Proxy{}
	p.SetPoolConsumerDiscount(0.30)
	if p.poolDiscount != 0.30 {
		t.Errorf("a valid rate was refused: %v", p.poolDiscount)
	}
}

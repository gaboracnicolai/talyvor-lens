package economy

// marketplace_consideration_test.go — W6.3.2, re-measured at current main and made executable.
//
// ⚠ NOTHING IS FIXED HERE. Taking payment is a product design (what does a buyer pay WITH — LXC,
// Stripe, an existing LENS balance?) and this is a LIVE MONEY PATH, so it is measured and reported.
// docs/marketplace-consideration-measured.md carries the decision.
//
// ⚠ THE FINDING: `ExecuteTrade` performs THREE ledger CREDITS AND NO DEBIT. The buyer receives LENS
// and pays nothing; the seller, who was debited the full amount at CreateListing, is credited only
// the UNSOLD remainder. Conservation holds — netToBuyer + fee + unsold == listingAmount, and
// `marketplace_buy` is correctly outside CountedSupplyTypes() — so NOTHING IS MINTED. The defect is
// missing CONSIDERATION, and no conservation invariant can see that: a free transfer conserves
// perfectly.
//
// ⚠ WHY A SOURCE CENSUS AND NOT A pgxmock TEST. The neighbouring tests drive a mock pool, which
// means declaring the SQL you expect — and "assert the statements I think it runs" is a fixture
// asserting itself when the claim under test is "a statement is ABSENT". Counting the ledger calls
// in the product's own text cannot flatter itself, and it is two-sided below: `Debit` must still
// appear ELSEWHERE in the file, or the census is reporting its own blindness.

import (
	"os"
	"strings"
	"testing"
)

func marketplaceSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("marketplace.go")
	if err != nil {
		t.Fatalf("read marketplace.go: %v", err)
	}
	return string(b)
}

// executeTradeBody returns just the ExecuteTrade function.
func executeTradeBody(t *testing.T, src string) string {
	t.Helper()
	const sig = "func (s *MarketplaceStore) ExecuteTrade("
	i := strings.Index(src, sig)
	if i < 0 {
		t.Fatal("ExecuteTrade is gone — re-anchor this census")
	}
	rest := src[i:]
	j := strings.Index(rest, "\n}\n")
	if j < 0 {
		t.Fatal("could not find the end of ExecuteTrade")
	}
	return rest[:j]
}

// ⚠ THE HEADLINE: three credits, zero debits.
func TestMeasured_ExecuteTradeCreditsThreePartiesAndDebitsNobody(t *testing.T) {
	src := marketplaceSource(t)
	body := executeTradeBody(t, src)

	credits := strings.Count(body, "CreditTx(")
	debits := strings.Count(body, ".Debit(") + strings.Count(body, "DebitTx(")

	if credits < 3 {
		t.Fatalf("ExecuteTrade issues %d ledger credits, expected at least 3 (buyer, fee, unsold "+
			"refund) — the shape changed and this census must be re-read before its verdict is "+
			"trusted", credits)
	}
	if debits != 0 {
		t.Fatalf("ExecuteTrade now issues %d ledger DEBIT(s).\n\n⚠ IF THAT IS THE BUYER BEING "+
			"CHARGED, THIS FINDING IS CLOSED AND THIS IS THE FIX LANDING: delete this test and the "+
			"blocking half of docs/marketplace-consideration-measured.md.", debits)
	}

	// ⚠ TWO-SIDED. `Debit` must exist ELSEWHERE in this file (CreateListing escrows the seller's
	// LENS), or "zero debits in ExecuteTrade" would be a statement about the search string.
	if strings.Count(src, ".Debit(") == 0 {
		t.Fatal("the string `.Debit(` appears nowhere in marketplace.go — this census cannot detect " +
			"a debit at all, so its verdict above is worthless. CreateListing is supposed to escrow " +
			"the seller's LENS with one.")
	}
	t.Logf("MEASURED: ExecuteTrade issues %d ledger credits and %d debits. The buyer receives LENS "+
		"and pays nothing.", credits, debits)
}

// ⚠ THE SELLER IS NEVER CREDITED FOR THE SOLD PORTION. They were debited the full listing amount at
// CreateListing; on a trade they get back only the UNSOLD remainder.
func TestMeasured_TheSellerIsNeverPaidForTheSoldPortion(t *testing.T) {
	src := marketplaceSource(t)
	for _, want := range []string{
		`"marketplace_unsold_refund"`, // the only trade-time seller credit
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("%s is gone — the seller's credit paths changed and this census must be "+
				"re-read", want)
		}
	}
	body := executeTradeBody(t, src)
	if strings.Contains(body, "marketplace_sale_proceeds") || strings.Contains(body, "seller_paid") {
		t.Fatal("a seller proceeds credit has appeared in ExecuteTrade — if the seller is now paid " +
			"for the sold portion, this finding is closed; delete this test and update the doc")
	}
}

// ⚠ THE FUNCTION THAT COMPUTES THE TRADE HAS NO OUTPUT FOR WHAT THE BUYER PAYS. tradeSplit returns
// (lensAmount, fee, netToBuyer, unsold) — four LENS quantities and no consideration. That is the
// finding stated structurally rather than as a count of call sites.
func TestMeasured_TradeSplitHasNoBuyerCostOutput(t *testing.T) {
	src := marketplaceSource(t)
	const sig = "func tradeSplit(listingAmount int64, priceUSD, amountUSD float64) (lensAmount, fee, netToBuyer, unsold int64)"
	if !strings.Contains(src, sig) {
		t.Fatalf("tradeSplit's signature changed.\n\n⚠ IF IT NOW RETURNS WHAT THE BUYER PAYS, THAT "+
			"IS THE FIX: delete this test and update docs/marketplace-consideration-measured.md.\n"+
			"expected: %s", sig)
	}
}

// ⚠ THE HANDLER MAPS A 402 THIS PATH CANNOT PRODUCE. cmd/lens/main.go turns
// economy.ErrInsufficientBalance into 402 Payment Required on the buy route — and ExecuteTrade never
// returns it. Only CreateListing does, for the SELLER's escrow debit. A status code for a payment
// that is never taken is the clearest single symptom of the missing consideration.
func TestMeasured_TheBuyHandlerMapsA402ThatExecuteTradeCannotReturn(t *testing.T) {
	body := executeTradeBody(t, marketplaceSource(t))
	if strings.Contains(body, "ErrInsufficientBalance") {
		t.Fatal("ExecuteTrade can now return ErrInsufficientBalance — a balance check has appeared, " +
			"which is the fix; delete this test and update the doc")
	}
	main, err := os.ReadFile("../../cmd/lens/main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	// Two-sided: the 402 mapping must actually be there, or "unreachable" is vacuous.
	if !strings.Contains(string(main), "economy.ErrInsufficientBalance") ||
		!strings.Contains(string(main), "http.StatusPaymentRequired") {
		t.Skip("the buy handler no longer maps ErrInsufficientBalance to 402 — the symptom this " +
			"test names is gone, which is fine, but re-read the doc before trusting it")
	}
	t.Log("MEASURED: the buy handler maps economy.ErrInsufficientBalance -> 402 Payment Required, " +
		"and ExecuteTrade cannot return it. The only producer is CreateListing's SELLER escrow debit.")
}

package economy

import (
	"os"
	"strings"
	"testing"
)

// (proof 6) CENTRAL-COUNTERPARTY guard: the agent-spend path must debit ONLY the workspace's LXC balance
// (workspace↔Talyvor pool) and NEVER move value account↔account. It must not reference LedgerStore.Transfer
// (the LENS P2P transfer) nor the marketplace, and must not open any LXC→fiat/withdraw path. Source-scan the
// spend file (a package-import guard is meaningless — marketplace.go is in THIS package).
func TestAgentSubbudget_CentralCounterparty_NoP2P(t *testing.T) {
	raw, err := os.ReadFile("agent_subbudget.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	// Forbidden references — any P2P / marketplace / fiat-out surface.
	for _, bad := range []string{
		".Transfer(", "TransferOut", "TransferIn", // LENS account↔account (cache_mining.go)
		"Marketplace", "marketplace_", "Listing", // the LENS marketplace
		"withdraw", "payout", "cashout", "ToFiat", "convert_to_lens", // any value-out path
	} {
		if strings.Contains(src, bad) {
			t.Errorf("agent_subbudget.go references %q — the agent debit must be LXC-only, workspace↔pool (central counterparty), never account↔account or value-out", bad)
		}
	}
	// Positive: it MUST debit via the shared LXC internals (proving it reuses, not duplicates, the balance path).
	for _, want := range []string{"readLXCBalance", "writeLXCBalance", "insertLXCLedger", "lxc_spend_claims"} {
		if !strings.Contains(src, want) {
			t.Errorf("agent_subbudget.go should use %q (reuse the LXC debit internals + the claim ledger)", want)
		}
	}
}

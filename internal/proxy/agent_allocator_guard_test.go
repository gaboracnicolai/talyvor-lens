package proxy

import (
	"os"
	"strings"
	"testing"
)

// (proof 7) CENTRAL-COUNTERPARTY: the agent allocator debits ONLY via SpendLXCForAgent (workspace↔Talyvor
// pool). Source-scan agent_allocator.go for any account↔account / marketplace / value-out reference.
func TestAgentAllocator_CentralCounterparty_NoP2P(t *testing.T) {
	raw, err := os.ReadFile("agent_allocator.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	for _, bad := range []string{
		// Central-counterparty invariant — UNCHANGED: no P2P transfer, no marketplace, no LENS conversion,
		// no cashout/withdrawal. NOTE: "Release"/"Refund" are intentionally NOT here — the approved billing
		// redesign (hold → settle/RELEASE) refunds the customer's OWN held LXC within the SAME workspace↔pool
		// closed loop (ReleaseLXCReservation), which is not a reverse-value LEAK, so the guard permits it.
		".Transfer(", "TransferOut", "TransferIn", "Marketplace", "marketplace_", "Listing",
		"withdraw", "payout", "cashout", "ConvertToLENS", "convert_to_lens",
	} {
		if strings.Contains(src, bad) {
			t.Errorf("agent_allocator.go references %q — the agent debit must be LXC-only, workspace↔pool (central counterparty), forward-only", bad)
		}
	}
	// Positive: it MUST debit through SpendLXCForAgent (the step-A atomic, ceiling-enforcing path).
	if !strings.Contains(src, "SpendLXCForAgent") {
		t.Error("agent_allocator.go must debit via SpendLXCForAgent")
	}
	// The reservation lifecycle must route through the economy primitives (ceiling-enforcing, closed-loop),
	// never a raw balance edit here.
	for _, must := range []string{"ReserveLXCForAgent", "SettleLXCReservation", "ReleaseLXCReservation"} {
		if !strings.Contains(src, must) {
			t.Errorf("agent_allocator.go must use %s (the closed-loop reservation primitive)", must)
		}
	}
}

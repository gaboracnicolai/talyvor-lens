package monitoring

import (
	"fmt"
	"time"
)

// UnderApprovalSetting is the setting the rule's values are read from.
const UnderApprovalSetting = "LENS_MONITOR_UNDER_APPROVAL"

// UnderApproval finds an agent splitting what would need a person's approval into payments just under it: Payments
// payments out within WithinHours, each at most the agent's approval amount and no more than UnderPercent under it.
// An agent without an approval amount is not judged by it.
type UnderApproval struct {
	Payments     int `json:"payments"`
	WithinHours  int `json:"within_hours"`
	UnderPercent int `json:"under_percent"`
}

// defaultUnderApproval is the rule's starting value: 3 payments within 24 hours, each no more than 10% under the
// agent's approval amount.
var defaultUnderApproval = UnderApproval{Payments: 3, WithinHours: 24, UnderPercent: 10}

func (UnderApproval) key() string { return "under_approval" }

func (r UnderApproval) window() time.Duration { return time.Duration(r.WithinHours) * time.Hour }

func (r UnderApproval) check() error {
	if err := atLeast(UnderApprovalSetting, "payments", r.Payments, 2); err != nil {
		return err
	}
	if err := atLeast(UnderApprovalSetting, "within_hours", r.WithinHours, 1); err != nil {
		return err
	}
	if r.UnderPercent < 1 || r.UnderPercent > 99 {
		return fmt.Errorf("monitoring: %s's under_percent is %d; it is 1 to 99", UnderApprovalSetting, r.UnderPercent)
	}
	return nil
}

// justUnder says whether p is money out by an agent with an approval amount, at most that amount and no more than
// UnderPercent under it.
func (r UnderApproval) justUnder(h *history, p Payment) bool {
	limit := h.approvalUSDMicros[p.AgentID]
	if p.Direction != "out" || p.AgentID == "" || limit <= 0 || p.usdMicros <= 0 {
		return false
	}
	return p.usdMicros <= limit && p.usdMicros >= limit-limit*int64(r.UnderPercent)/100
}

func (r UnderApproval) judge(h *history, i int) (Hit, bool) {
	p := h.payments[i]
	if !r.justUnder(h, p) {
		return Hit{}, false
	}
	var idx []int
	for _, j := range h.within(i, r.window()) {
		if q := h.payments[j]; q.AgentID == p.AgentID && r.justUnder(h, q) {
			idx = append(idx, j)
		}
	}
	if len(idx) < r.Payments {
		return Hit{}, false
	}
	return Hit{Entries: entryIDs(h, idx), Summary: fmt.Sprintf(
		"%d payments by agent %s within %s, each no more than %d%% under its approval amount of %s",
		len(idx), p.AgentID, minutes(r.window()), r.UnderPercent, money(h.approvalUSDMicros[p.AgentID]/10_000, "USD"))}, true
}

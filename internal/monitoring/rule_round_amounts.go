package monitoring

import (
	"fmt"
	"time"
)

// RoundAmountsSetting is the setting the rule's values are read from.
const RoundAmountsSetting = "LENS_MONITOR_ROUND_AMOUNTS"

// RoundAmounts finds a burst of round amounts: Payments payments out within WithinMinutes, each a whole multiple of
// RoundTo in its own currency — 100 is £100, €100, $100 or 100 USDC.
type RoundAmounts struct {
	Payments      int `json:"payments"`
	WithinMinutes int `json:"within_minutes"`
	RoundTo       int `json:"round_to"`
}

// defaultRoundAmounts is the rule's starting value: 3 payments of whole hundreds within 60 minutes.
var defaultRoundAmounts = RoundAmounts{Payments: 3, WithinMinutes: 60, RoundTo: 100}

func (RoundAmounts) key() string { return "round_amounts" }

func (r RoundAmounts) window() time.Duration { return time.Duration(r.WithinMinutes) * time.Minute }

func (r RoundAmounts) check() error {
	if err := atLeast(RoundAmountsSetting, "payments", r.Payments, 2); err != nil {
		return err
	}
	if err := atLeast(RoundAmountsSetting, "within_minutes", r.WithinMinutes, 1); err != nil {
		return err
	}
	return atLeast(RoundAmountsSetting, "round_to", r.RoundTo, 1)
}

func (r RoundAmounts) round(p Payment) bool {
	return p.Direction == "out" && p.AmountMinor > 0 && p.AmountMinor%(int64(r.RoundTo)*major(p.Currency)) == 0
}

func (r RoundAmounts) judge(h *history, i int) (Hit, bool) {
	if !r.round(h.payments[i]) {
		return Hit{}, false
	}
	var idx []int
	for _, j := range h.within(i, r.window()) {
		if r.round(h.payments[j]) {
			idx = append(idx, j)
		}
	}
	if len(idx) < r.Payments {
		return Hit{}, false
	}
	return Hit{Entries: entryIDs(h, idx), Summary: fmt.Sprintf("%d payments of round amounts (whole multiples of %d) within %s",
		len(idx), r.RoundTo, minutes(r.window()))}, true
}

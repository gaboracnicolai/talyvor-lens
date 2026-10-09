package monitoring

import (
	"fmt"
	"time"
)

// InAndOutSetting is the setting the rule's values are read from.
const InAndOutSetting = "LENS_MONITOR_IN_AND_OUT"

// InAndOut finds money that comes in and goes straight back out: at least OutPercent of a payment in leaves the
// workspace, in payments out in the same currency, within WithinMinutes of it arriving. It is found at the payment out
// that takes the total over that share, so one payment in is found once.
type InAndOut struct {
	WithinMinutes int `json:"within_minutes"`
	OutPercent    int `json:"out_percent"`
}

// defaultInAndOut is the rule's starting value: half of a payment in goes out again within 60 minutes.
var defaultInAndOut = InAndOut{WithinMinutes: 60, OutPercent: 50}

func (InAndOut) key() string { return "in_and_out" }

func (r InAndOut) window() time.Duration { return time.Duration(r.WithinMinutes) * time.Minute }

func (r InAndOut) check() error {
	if err := atLeast(InAndOutSetting, "within_minutes", r.WithinMinutes, 1); err != nil {
		return err
	}
	if r.OutPercent < 1 || r.OutPercent > 100 {
		return fmt.Errorf("monitoring: %s's out_percent is %d; it is 1 to 100", InAndOutSetting, r.OutPercent)
	}
	return nil
}

func (r InAndOut) judge(h *history, i int) (Hit, bool) {
	p := h.payments[i]
	if p.Direction != "out" {
		return Hit{}, false
	}
	for _, j := range h.within(i, r.window()) {
		in := h.payments[j]
		if j == i || in.Direction != "in" || in.Currency != p.Currency {
			continue
		}
		need := (in.AmountMinor*int64(r.OutPercent) + 99) / 100
		idx, out := []int{j}, int64(0)
		for k := j + 1; k <= i; k++ {
			if q := h.payments[k]; q.Direction == "out" && q.Currency == p.Currency {
				idx, out = append(idx, k), out+q.AmountMinor
			}
		}
		if out >= need && out-p.AmountMinor < need {
			return Hit{Entries: entryIDs(h, idx), Summary: fmt.Sprintf(
				"%s came in from %s and %s went out again within %s (%d payments out)",
				money(in.AmountMinor, in.Currency), in.Counterparty, money(out, p.Currency), minutes(r.window()), len(idx)-1)}, true
		}
	}
	return Hit{}, false
}

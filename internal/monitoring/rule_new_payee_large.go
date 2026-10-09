package monitoring

import (
	"fmt"
	"slices"
)

// NewPayeeLargeSetting is the setting the rule's values are read from.
const NewPayeeLargeSetting = "LENS_MONITOR_NEW_PAYEE_LARGE"

// NewPayeeLarge finds a first payment to a new payee well above the payer's usual size: at least TimesUsual times the
// median of the agent's payments out in the same currency over the history — the company's, for its own money —
// once there are FromPayments of them to know its usual size by.
type NewPayeeLarge struct {
	TimesUsual   int `json:"times_usual"`
	FromPayments int `json:"from_payments"`
}

// defaultNewPayeeLarge is the rule's starting value: at least 3 times the usual payment, known from 3 payments.
var defaultNewPayeeLarge = NewPayeeLarge{TimesUsual: 3, FromPayments: 3}

func (NewPayeeLarge) key() string { return "new_payee_large" }

func (r NewPayeeLarge) check() error {
	if err := atLeast(NewPayeeLargeSetting, "times_usual", r.TimesUsual, 2); err != nil {
		return err
	}
	return atLeast(NewPayeeLargeSetting, "from_payments", r.FromPayments, 1)
}

func (r NewPayeeLarge) judge(h *history, i int) (Hit, bool) {
	p := h.payments[i]
	if !h.newPayee(i) {
		return Hit{}, false
	}
	var before []int64
	for _, j := range h.within(i, h.known) {
		if q := h.payments[j]; j < i && q.Direction == "out" && q.AgentID == p.AgentID && q.Currency == p.Currency {
			before = append(before, q.AmountMinor)
		}
	}
	if len(before) < r.FromPayments {
		return Hit{}, false
	}
	slices.Sort(before)
	usual := before[len(before)/2]
	if len(before)%2 == 0 {
		usual = (before[len(before)/2-1] + usual) / 2
	}
	if usual <= 0 || p.AmountMinor < usual*int64(r.TimesUsual) {
		return Hit{}, false
	}
	payer := "the company"
	if p.AgentID != "" {
		payer = "agent " + p.AgentID
	}
	return Hit{Entries: []string{p.EntryID}, Summary: fmt.Sprintf(
		"the first payment to %s, %s, is at least %d times the usual payment by %s of %s (the median of its last %d)",
		p.Counterparty, money(p.AmountMinor, p.Currency), r.TimesUsual, payer, money(usual, p.Currency), len(before))}, true
}

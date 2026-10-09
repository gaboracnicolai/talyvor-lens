package monitoring

import (
	"fmt"
	"strings"
	"time"
)

// NewPayeesSetting is the setting the rule's values are read from.
const NewPayeesSetting = "LENS_MONITOR_NEW_PAYEES"

// NewPayees finds many new payees in one day: Payees payees the workspace had not paid in the history, each first
// paid within WithinHours.
type NewPayees struct {
	Payees      int `json:"payees"`
	WithinHours int `json:"within_hours"`
}

// defaultNewPayees is the rule's starting value: 5 new payees within 24 hours.
var defaultNewPayees = NewPayees{Payees: 5, WithinHours: 24}

func (NewPayees) key() string { return "new_payees" }

func (r NewPayees) window() time.Duration { return time.Duration(r.WithinHours) * time.Hour }

func (r NewPayees) check() error {
	if err := atLeast(NewPayeesSetting, "payees", r.Payees, 2); err != nil {
		return err
	}
	return atLeast(NewPayeesSetting, "within_hours", r.WithinHours, 1)
}

func (r NewPayees) judge(h *history, i int) (Hit, bool) {
	if !h.newPayee(i) {
		return Hit{}, false
	}
	var idx []int
	var names []string
	seen := map[string]bool{}
	for _, j := range h.within(i, r.window()) {
		if name := payeeKey(h.payments[j].Counterparty); h.newPayee(j) && !seen[name] {
			seen[name] = true
			idx, names = append(idx, j), append(names, h.payments[j].Counterparty)
		}
	}
	if len(idx) < r.Payees {
		return Hit{}, false
	}
	return Hit{Entries: entryIDs(h, idx), Summary: fmt.Sprintf("%d new payees paid within %s: %s",
		len(idx), minutes(r.window()), strings.Join(names, ", "))}, true
}

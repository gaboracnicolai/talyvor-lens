// Package monitoring watches the money that moves in and out through partners for the patterns a compliance officer
// looks for (B30.7), and opens or extends a compliance case on each one it finds. It never moves money and never
// stops it: a case is for a person to review (B30.8).
//
// Five rules, each in its own file with its starting values:
//
//	rule_under_approval.go   several payments just under an agent's approval amount within 24 hours
//	rule_in_and_out.go       money in and straight out within an hour
//	rule_new_payee_large.go  a first payment to a new payee well above the agent's usual size
//	rule_round_amounts.go    a burst of round amounts
//	rule_new_payees.go       many new payees in one day
//
// Each rule's values are a setting Nicolai fills (lens.env.example, docs/compliance-defaults.md); until he does, the
// rule uses the starting value its file gives.
//
// The rules judge payments: money in or out of a workspace through a partner account, as the money ledger records it
// (B30.2). They run on each payment once it is posted (economy.DualTokenStore.SetMonitor), and nightly over the last
// day's, which finds anything a payment's own run missed. A rule judges a payment once, so the nightly run adds
// nothing the payment already raised. Test money and live money are judged apart.
package monitoring

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Payment is one payment in or out through a partner: what the rules judge. An entry that moves several currencies
// through partners is one payment per currency.
type Payment struct {
	EntryID      string    `json:"entry_id"`
	AgentID      string    `json:"agent_id,omitempty"` // the agent whose own money moved; "" for the company's
	Direction    string    `json:"direction"`          // in or out
	Counterparty string    `json:"counterparty"`       // who it is from or to
	AmountMinor  int64     `json:"amount_minor"`
	Currency     string    `json:"currency"`
	Funding      string    `json:"funding"` // test or live
	At           time.Time `json:"at"`
	// usdMicros is AmountMinor in micro-dollars, for comparing with an agent's approval amount; 0 when it is not
	// known — no rate prices it, or no rule needs it.
	usdMicros int64
}

// Hit is a rule finding its pattern at a payment.
type Hit struct {
	Rule    string   // the rule's key
	Payment Payment  // the payment the rule judged: the last of the pattern
	Entries []string // every payment the pattern is made of, oldest first
	Summary string   // why, in a sentence, for the person who reviews the case
}

// history is one workspace's payments in one funding, oldest first, and what the rules judge them against.
type history struct {
	payments []Payment
	// approvalUSDMicros is each agent's approval amount, in micro-dollars: a payment above it needs a person's approval.
	approvalUSDMicros map[string]int64
	// known is how far back a payee counts as already paid, and an agent's payments make its usual size.
	known time.Duration
	isNew []bool // newPayee's answers, worked out once
}

// within is the indices of the payments up to and including i, from the last d before payments[i], oldest first.
func (h *history) within(i int, d time.Duration) []int {
	start := h.payments[i].At.Add(-d)
	j := i
	for j > 0 && h.payments[j-1].At.After(start) {
		j--
	}
	out := make([]int, 0, i-j+1)
	for ; j <= i; j++ {
		out = append(out, j)
	}
	return out
}

// newPayee says whether payments[i] is money out to someone the workspace has not paid in the known window before it.
func (h *history) newPayee(i int) bool {
	if h.isNew == nil {
		h.isNew = make([]bool, len(h.payments))
		last := map[string]time.Time{}
		for j, p := range h.payments {
			if p.Direction != "out" {
				continue
			}
			name := payeeKey(p.Counterparty)
			paid, ok := last[name]
			h.isNew[j] = !ok || !paid.After(p.At.Add(-h.known))
			last[name] = p.At
		}
	}
	return h.isNew[i]
}

// payeeKey is a payee's name as the rules compare it: case, spacing and punctuation aside.
func payeeKey(name string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(name), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}), " ")
}

// Settings is every rule's values.
type Settings struct {
	// HistoryDays is how far back a payee counts as already paid, and an agent's payments make its usual size.
	HistoryDays   int
	UnderApproval UnderApproval
	InAndOut      InAndOut
	NewPayeeLarge NewPayeeLarge
	RoundAmounts  RoundAmounts
	NewPayees     NewPayees
}

// HistorySetting is the setting HistoryDays is read from.
const HistorySetting = "LENS_MONITOR_HISTORY_DAYS"

// defaultHistoryDays is HistoryDays' starting value: a payee paid in the last 90 days is not new.
const defaultHistoryDays = 90

// Defaults are every rule's starting values.
func Defaults() Settings {
	return Settings{HistoryDays: defaultHistoryDays, UnderApproval: defaultUnderApproval, InAndOut: defaultInAndOut,
		NewPayeeLarge: defaultNewPayeeLarge, RoundAmounts: defaultRoundAmounts, NewPayees: defaultNewPayees}
}

// rule is one pattern: judge says whether it is found at payments[i].
type rule interface {
	key() string
	judge(h *history, i int) (Hit, bool)
}

func (s Settings) rules() []rule {
	return []rule{s.UnderApproval, s.InAndOut, s.NewPayeeLarge, s.RoundAmounts, s.NewPayees}
}

// horizon is how far back of a payment the rules read.
func (s Settings) horizon() time.Duration {
	d := time.Duration(s.HistoryDays) * 24 * time.Hour
	for _, w := range []time.Duration{s.UnderApproval.window(), s.InAndOut.window(), s.RoundAmounts.window(), s.NewPayees.window()} {
		d = max(d, w)
	}
	return d
}

// judge runs every rule at each of targets, indices into h.payments.
func (s Settings) judge(h *history, targets []int) []Hit {
	var hits []Hit
	for _, i := range targets {
		for _, r := range s.rules() {
			if hit, ok := r.judge(h, i); ok {
				hit.Rule, hit.Payment = r.key(), h.payments[i]
				hits = append(hits, hit)
			}
		}
	}
	return hits
}

// Load reads every rule's values from getenv. A rule's setting is a JSON object of the values to change — like its
// default in lens.env.example — and a value it leaves out keeps its starting value.
func Load(getenv func(string) string) (Settings, error) {
	s := Defaults()
	if v := strings.TrimSpace(getenv(HistorySetting)); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return s, fmt.Errorf("monitoring: %s=%q must be a whole number of days, at least 1", HistorySetting, v)
		}
		s.HistoryDays = n
	}
	for _, f := range []struct {
		name string
		into any
	}{
		{UnderApprovalSetting, &s.UnderApproval},
		{InAndOutSetting, &s.InAndOut},
		{NewPayeeLargeSetting, &s.NewPayeeLarge},
		{RoundAmountsSetting, &s.RoundAmounts},
		{NewPayeesSetting, &s.NewPayees},
	} {
		v := strings.TrimSpace(getenv(f.name))
		if v == "" {
			continue
		}
		dec := json.NewDecoder(bytes.NewReader([]byte(v)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(f.into); err != nil {
			return s, fmt.Errorf("monitoring: %s must be a JSON object of the rule's values, like its default in lens.env.example: %w", f.name, err)
		}
	}
	for _, c := range []interface{ check() error }{s.UnderApproval, s.InAndOut, s.NewPayeeLarge, s.RoundAmounts, s.NewPayees} {
		if err := c.check(); err != nil {
			return s, err
		}
	}
	return s, nil
}

// LoadEnv reads every rule's values from this process's environment.
func LoadEnv() (Settings, error) { return Load(os.Getenv) }

// atLeast refuses a value under min, naming the setting and the field.
func atLeast(setting, field string, v, min int) error {
	if v < min {
		return fmt.Errorf("monitoring: %s's %s is %d; it is at least %d", setting, field, v, min)
	}
	return nil
}

// places is how many decimal places currency's minor unit is: millionths for USDC, hundredths for the rest.
func places(currency string) int {
	if currency == "USDC" {
		return 6
	}
	return 2
}

// major is one whole unit of currency — a pound, a euro, a dollar, a USDC — in its minor units.
func major(currency string) int64 {
	unit := int64(1)
	for range places(currency) {
		unit *= 10
	}
	return unit
}

// money writes minor units of currency as an amount a person reads: "GBP 480.00".
func money(minor int64, currency string) string {
	sign := ""
	if minor < 0 {
		sign, minor = "-", -minor
	}
	unit := major(currency)
	return fmt.Sprintf("%s %s%d.%0*d", currency, sign, minor/unit, places(currency), minor%unit)
}

// entryIDs is the entry ids of the payments at idx, in order, each once.
func entryIDs(h *history, idx []int) []string {
	out := make([]string, 0, len(idx))
	seen := map[string]bool{}
	for _, i := range idx {
		if id := h.payments[i].EntryID; !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// sortPayments puts payments in the order the rules read them: by time, then entry.
func sortPayments(ps []Payment) {
	sort.SliceStable(ps, func(a, b int) bool {
		if !ps[a].At.Equal(ps[b].At) {
			return ps[a].At.Before(ps[b].At)
		}
		return ps[a].EntryID < ps[b].EntryID
	})
}

// minutes writes a window for a summary.
func minutes(d time.Duration) string {
	if d%time.Hour == 0 {
		if d == time.Hour {
			return "an hour"
		}
		return fmt.Sprintf("%d hours", d/time.Hour)
	}
	return fmt.Sprintf("%d minutes", d/time.Minute)
}

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/economy"
)

type levelLimitsFake struct{ set []economy.LevelLimit }

func (f *levelLimitsFake) LevelLimits(context.Context) ([]economy.LevelLimit, error) {
	return f.set, nil
}
func (f *levelLimitsFake) SetLevelLimit(_ context.Context, level economy.VerificationLevel, currency string, limitMinor int64,
	operator, reference string) (economy.LevelLimit, error) {
	l := economy.LevelLimit{Level: level, Currency: currency, LimitMinor: limitMinor, Operator: operator, Reference: reference, At: time.Now()}
	f.set = append(f.set, l)
	return l, nil
}

// B30.4 — `lens verification-limits set L2 GBP 1000.00 …` records £1000.00 as 100000 pence, exactly; an amount with
// more decimals than the currency has is refused; and the listing shows each level, the capabilities that need it,
// and its limits — none where none is set.
func TestVerificationLimitsCommand_SetsAndListsLimits(t *testing.T) {
	ctx := context.Background()
	store := &levelLimitsFake{}
	var out bytes.Buffer
	if err := verificationLimitsCommand(ctx, store, []string{"set", "L2", "gbp", "1000.00", "board", "minute", "7"}, "nicolai", &out); err != nil {
		t.Fatal(err)
	}
	if len(store.set) != 1 || store.set[0] != (economy.LevelLimit{Level: economy.LevelIdentity, Currency: "GBP", LimitMinor: 100_000,
		Operator: "nicolai", Reference: "board minute 7", At: store.set[0].At}) {
		t.Fatalf("set %+v; want L2 GBP 100000 by nicolai", store.set)
	}
	if err := verificationLimitsCommand(ctx, store, []string{"set", "L2", "GBP", "10.001", "x"}, "nicolai", &out); err == nil {
		t.Fatal("an amount in tenths of a penny was accepted")
	}
	out.Reset()
	if err := verificationLimitsCommand(ctx, store, nil, "nicolai", &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"L2\tidentity checked\tneeded by: pay_another_owner,", "\tGBP\t1000.00 GBP per movement",
		"L3\tcompany checked\tneeded by: loans_between_companies,", "\tEUR\tnone — no live money"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("the listing has no %q:\n%s", want, out.String())
		}
	}
}

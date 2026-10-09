package monitoring

import (
	"bufio"
	"os"
	"strings"
	"testing"
)

// B30.7 — the values lens.env.example gives the rules are the starting values each rule's file holds; a malformed one
// is refused, so Lens does not start on it.
func TestSettings_LensEnvExampleGivesEachRulesStartingValues(t *testing.T) {
	f, err := os.Open("../../lens.env.example")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	env := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if name, value, ok := strings.Cut(strings.TrimPrefix(sc.Text(), "# "), "="); ok && strings.HasPrefix(name, "LENS_MONITOR_") {
			env[name] = value
		}
	}
	if len(env) != 6 {
		t.Fatalf("lens.env.example gives %d LENS_MONITOR_ settings (%v); want the history and the five rules", len(env), env)
	}
	got, err := Load(func(k string) string { return env[k] })
	if err != nil || got != Defaults() {
		t.Fatalf("lens.env.example's values load as %+v, %v; want the starting values %+v", got, err, Defaults())
	}
	part, err := Load(func(k string) string { return map[string]string{RoundAmountsSetting: `{"payments":4}`}[k] })
	if err != nil || part.RoundAmounts.Payments != 4 || part.RoundAmounts.RoundTo != defaultRoundAmounts.RoundTo {
		t.Fatalf("a setting naming one value loads as %+v, %v; want that value changed and the rest kept", part.RoundAmounts, err)
	}
	for _, bad := range []map[string]string{
		{InAndOutSetting: `{"out_percent":0}`},
		{NewPayeesSetting: `{"payes":5}`},
		{UnderApprovalSetting: `3`},
		{HistorySetting: `0`},
	} {
		if _, err := Load(func(k string) string { return bad[k] }); err == nil {
			t.Fatalf("%v loaded; want it refused", bad)
		}
	}
}

package main

import (
	"testing"

	"github.com/talyvor/lens/internal/config"
	"github.com/talyvor/lens/internal/econflags"
)

// The override is the only thing standing between "the operator asked for the unbilled lane" and
// "the unbilled lane is open". Those are different facts (newBatchReg's own words), and the config
// struct can only express the first — so a readout built from the struct alone would report an
// open lane that is closed. These tests pin the three states from the GATE the process actually
// wired, not from a hand-built override value.
func TestBatchFlagOverrideReportsTheGateNotTheFlag(t *testing.T) {
	t.Run("asked for and refused ⇒ forced_off_at_runtime", func(t *testing.T) {
		cfg := &config.Config{EconomyEnabled: true, BatchEnabled: true}
		// settleWired=false is what main.go passes today: no production caller wires SetSettleHook.
		gate := newBatchReg(cfg.BatchEnabled, false)
		if gate.on {
			t.Fatal("precondition: an unhooked lane must be refused, else this test proves nothing")
		}

		snap := econflags.Report(cfg, "abc1234", batchFlagOverride(cfg, gate)()...)
		f := findFlag(t, snap, "BatchEnabled")
		if f.State != econflags.StateForcedOffAtRuntime {
			t.Errorf("state = %q, want %q — the operator set LENS_BATCH_ENABLED and the lane is "+
				"CLOSED; reporting anything else tells them an unbilled lane is open when it is not",
				f.State, econflags.StateForcedOffAtRuntime)
		}
		if f.Value {
			t.Error("effective value is true for a refused lane")
		}
		if f.Configured == nil || !*f.Configured {
			t.Error("configured value must be present and true — its presence is the signal that " +
				"an override is in force, and without it the disagreement is invisible")
		}
		if f.Note == "" {
			t.Error("no reason given: 'off' without a reason is indistinguishable from a lane " +
				"nobody enabled, which is the confusion econflags exists to remove")
		}
	})

	t.Run("asked for and wired ⇒ on, no override", func(t *testing.T) {
		cfg := &config.Config{EconomyEnabled: true, BatchEnabled: true}
		gate := newBatchReg(cfg.BatchEnabled, true)
		if !gate.on {
			t.Fatal("precondition: a hooked lane the operator asked for must open")
		}
		if got := batchFlagOverride(cfg, gate)(); got != nil {
			t.Errorf("override = %v, want nil — config and behaviour agree, and an override that "+
				"repeats the configured value is noise on a money surface", got)
		}
		snap := econflags.Report(cfg, "abc1234", batchFlagOverride(cfg, gate)()...)
		if f := findFlag(t, snap, "BatchEnabled"); f.State != econflags.StateOn {
			t.Errorf("state = %q, want %q", f.State, econflags.StateOn)
		}
	})

	t.Run("not asked for ⇒ off, no override", func(t *testing.T) {
		cfg := &config.Config{EconomyEnabled: true, BatchEnabled: false}
		gate := newBatchReg(cfg.BatchEnabled, false)
		if got := batchFlagOverride(cfg, gate)(); got != nil {
			t.Errorf("override = %v, want nil for a lane nobody enabled", got)
		}
		snap := econflags.Report(cfg, "abc1234", batchFlagOverride(cfg, gate)()...)
		if f := findFlag(t, snap, "BatchEnabled"); f.State != econflags.StateOff {
			t.Errorf("state = %q, want %q", f.State, econflags.StateOff)
		}
	})
}

// The fiat door has no runtime override — it is the plain struct value — so what needs pinning is
// that it is REPORTED AT ALL and reports the variable that actually sets it. It was absent from
// this readout entirely until rule D found it.
func TestBillingFlagIsReportedBothWays(t *testing.T) {
	for _, on := range []bool{true, false} {
		cfg := &config.Config{EconomyEnabled: false, BillingEnabled: on}
		snap := econflags.Report(cfg, "abc1234")
		f := findFlag(t, snap, "BillingEnabled")
		if f.Value != on {
			t.Errorf("BillingEnabled reported %v, want %v", f.Value, on)
		}
		// EconomyEnabled is FALSE here on purpose: fiat billing is deliberately outside the
		// force-off block, so a readout that lumped it in with the economy would answer
		// "forced_off" about a door that is wide open.
		want := econflags.StateOff
		if on {
			want = econflags.StateOn
		}
		if f.State != want {
			t.Errorf("BillingEnabled state = %q with EconomyEnabled=false, want %q — fiat billing "+
				"is independent of the economy master switch and must not be reported as forced off",
				f.State, want)
		}
		if f.Env != "LENS_BILLING_ENABLED" {
			t.Errorf("env = %q, want LENS_BILLING_ENABLED", f.Env)
		}
	}
}

func findFlag(t *testing.T, snap econflags.Snapshot, name string) econflags.Flag {
	t.Helper()
	if !snap.Observed {
		t.Fatal("snapshot not observed")
	}
	for _, f := range snap.Flags {
		if f.Name == name {
			return f
		}
	}
	t.Fatalf("%s is not reported at all — the money readout is silent about it", name)
	return econflags.Flag{}
}

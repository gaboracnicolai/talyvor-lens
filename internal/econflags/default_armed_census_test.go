package econflags

// RULE F — WHAT IS ARMED WHEN AN OPERATOR SETS NOTHING.
//
// ⚠ WHY A FIFTH RULE, AND WHY IT IS NOT ANOTHER NAME. Rules A–E all key on how a
// flag is WRITTEN: A and B on config.go's force-off block, C on a Mint-or-LXC name
// shape, D on cmd/lens's own gate types, E on the pinned reported set. forceoff_transcription_test.go's doc
// comment already conceded C's weakness and named two flags it could not see. The
// queue framed the widening as needing "a rule for what a money-path flag IS", and
// inventing that taxonomy as the work.
//
// MEASURED, IT DOES NOT NEED ONE. There is a property that needs no taxonomy and no
// name: whether the flag is TRUE in a process whose environment says nothing. That
// is not a source shape to be parsed — it is an OBSERVATION, taken the way #440
// took it, by clearing every LENS_* variable, calling config.Load() and reading the
// struct. It is also the property this whole package exists for: its own doc
// comment says the available answer was "read config.go and infer the default,
// which is inference, not observation", and a DEFAULT-ARMED flag is exactly the one
// where inferring wrong is most expensive, because an operator who did nothing has
// it running.
//
// WHAT IT CAUGHT, on the commit that introduced it: 60 bool fields, 14 armed by
// default, and SEVEN of the 14 outside the readout — the handover had named ONE.
// Five of the seven are the settlement chain and are now reported:
// SettlementFailClosedEnabled (13 read sites; off ⇒ a royalty mint the ring
// detector never examined settles anyway), DetectorSweepEnabled (the examination
// half of that same decision), KeelRoyaltyHaircutEnabled (a reduce-only haircut on
// what a contributor is paid), KeelHardenedEnabled (the detector it consumes) and
// KeelEnabled (their enclosing condition).
//
// ⚠ AND NOTE WHICH INSTRUMENTS WOULD STILL HAVE MISSED SettlementFailClosedEnabled.
// It is not in the force-off block, it is not Mint-or-LXC named, it gates no route
// registration — and it is not even written with parseBoolEnvDefaultTrue, the
// helper a grep for "default true" would find: config.go assigns `c.X = true` and
// then conditionally re-reads the env. Only reading the loaded struct sees it.
//
// THE EXCLUSIONS BELOW CARRY REASONS, NOT JUST NAMES. An armed flag may be absent
// from the readout only with a stated argument, so the next session argues with the
// reason rather than rediscovering the flag.

import (
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/config"
)

// FLOORS. Every assertion below is a set comparison, and a set comparison over an
// EMPTY set passes. These stand between "every armed flag is accounted for" and "my
// reflection read nothing and reported a clean product". Measured at the commit that
// introduced this file: 60 bool fields, 14 armed. Set below today's numbers so an
// ordinary addition does not red them, far enough above zero that a Load() that
// silently produced a zero struct does.
const (
	floorConfigBoolFields = 50
	floorArmedByDefault   = 10
)

// notMoneyPath is the ARMED-BUT-UNREPORTED allowlist. A flag here is armed on a
// fresh deployment and deliberately absent from the readout; the string is the
// argument for that, quoted from or measured against the source that decides it.
//
// ⚠ A NAME HERE IS A CLAIM, AND THE TEST BELOW CHECKS IT IS STILL A LIVE ONE: an
// entry that is no longer armed by default (renamed, flipped, deleted) reds, because
// a stale exemption is how an allowlist quietly grows to cover something new.
var notMoneyPath = map[string]string{
	"ModelWatchEnabled": "the catalog-drift poller. config.go: 'DETECTION ONLY — it never sets a price, " +
		"for the reasons in that package's doc comment (no provider publishes rates in any API)'. It alerts " +
		"on a model the catalog cannot price; it changes no charge, arms no mint and registers no route.",
	"RoutingDecisionCaptureEnabled": "the descriptive route-decision capture. config.go: 'DESCRIPTIVE, " +
		"MINT-FREE ... the sink has no ledger handle'. It records the Advisor's baseline vs served model as " +
		"the go/no-go SUBSTRATE for a mint that does not exist yet — a future mint's evidence is not a money " +
		"path, and reporting it here would say the opposite.",
}

// loadWithClearedEnv observes the defaults: every LENS_* variable removed, then the
// five config.Load() refuses to start without supplied as inert placeholders.
//
// ⚠ THE PLACEHOLDERS ARE NOT FLAGS AND CANNOT ARM ONE — they are a Redis/Postgres/
// NATS URL and two provider keys, none of which is a bool. If that ever stops being
// true this test's premise is gone, so the bool census below is taken over the whole
// struct rather than over a curated list.
func loadWithClearedEnv(t *testing.T) *config.Config {
	t.Helper()
	for _, kv := range os.Environ() {
		i := strings.IndexByte(kv, '=')
		if i <= 0 || !strings.HasPrefix(kv[:i], "LENS_") {
			continue
		}
		t.Setenv(kv[:i], "") // registers the restore
		if err := os.Unsetenv(kv[:i]); err != nil {
			t.Fatalf("unset %s: %v", kv[:i], err)
		}
	}
	for _, kv := range [][2]string{
		{"LENS_REDIS_URL", "redis://127.0.0.1:6379"},
		{"LENS_DATABASE_URL", "postgres://u:p@127.0.0.1:5432/d?sslmode=disable"},
		{"LENS_NATS_URL", "nats://127.0.0.1:4222"},
		{"LENS_OPENAI_API_KEY", "sk-not-a-real-key"},
		{"LENS_ANTHROPIC_API_KEY", "sk-not-a-real-key"},
	} {
		t.Setenv(kv[0], kv[1])
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load with a cleared environment: %v", err)
	}
	return cfg
}

// armedByDefault returns every bool field of the loaded config that is TRUE.
func armedByDefault(t *testing.T, cfg *config.Config) (armed []string, totalBools int) {
	t.Helper()
	v := reflect.ValueOf(*cfg)
	ty := v.Type()
	for i := 0; i < ty.NumField(); i++ {
		if ty.Field(i).Type.Kind() != reflect.Bool {
			continue
		}
		totalBools++
		if v.Field(i).Bool() {
			armed = append(armed, ty.Field(i).Name)
		}
	}
	sort.Strings(armed)
	return armed, totalBools
}

// RULE F. Every flag the process arms by default is either in the readout or
// carries a written reason for not being.
func TestRuleF_EveryDefaultArmedFlagIsReportedOrReasonedAway(t *testing.T) {
	cfg := loadWithClearedEnv(t)
	armed, totalBools := armedByDefault(t, cfg)

	if totalBools < floorConfigBoolFields {
		t.Fatalf("FLOOR: reflected %d bool fields, want >= %d — the struct was not read, so every "+
			"assertion below would be vacuous", totalBools, floorConfigBoolFields)
	}
	if len(armed) < floorArmedByDefault {
		t.Fatalf("FLOOR: %d flags armed by default, want >= %d — a Load() that produced a zero struct "+
			"would report a clean product here", len(armed), floorArmedByDefault)
	}

	reported := map[string]bool{}
	for _, f := range Report(cfg, "dev").Flags {
		reported[f.Name] = true
	}

	for _, name := range armed {
		if reported[name] {
			continue
		}
		if _, excused := notMoneyPath[name]; excused {
			continue
		}
		t.Errorf("%s is TRUE on a deployment whose environment says nothing about it, and the readout "+
			"does not mention it. Add it to the entries table, or add it to notMoneyPath with the "+
			"argument for why an armed flag is not worth an operator's attention.", name)
	}
}

// The allowlist must not rot. An exemption for a flag that is no longer armed by
// default is dead prose that quietly widens: the next flag to take that name, or a
// rename that lands on it, inherits an exemption nobody argued for.
func TestRuleF_EveryExemptionIsStillLive(t *testing.T) {
	cfg := loadWithClearedEnv(t)
	armed, _ := armedByDefault(t, cfg)
	isArmed := map[string]bool{}
	for _, n := range armed {
		isArmed[n] = true
	}
	for name, reason := range notMoneyPath {
		if !isArmed[name] {
			t.Errorf("notMoneyPath excuses %q, but it is not armed by default any more — delete the "+
				"exemption rather than leaving it to cover something else. Reason on file: %s", name, reason)
		}
		if reportedNames(t)[name] {
			t.Errorf("notMoneyPath excuses %q AND the readout reports it — one of the two is wrong, and "+
				"an exemption that excuses nothing reads as an argument that was made", name)
		}
		if len(strings.TrimSpace(reason)) < 40 {
			t.Errorf("notMoneyPath[%q] has no real argument (%q) — a name without a reason is the "+
				"allowlist growing silently", name, reason)
		}
	}
}

// THE SETTLEMENT CHAIN IS PINNED BY NAME, because this is the set whose absence was
// the finding. Rule F above would go green again the moment someone moved these into
// notMoneyPath with a sentence; these five are money and the reason they are money is
// recorded in the entries table beside them.
func TestRuleF_TheSettlementChainIsReported(t *testing.T) {
	reported := reportedNames(t)
	for _, name := range []string{
		"SettlementFailClosedEnabled",
		"DetectorSweepEnabled",
		"KeelRoyaltyHaircutEnabled",
		"KeelHardenedEnabled",
		"KeelEnabled",
	} {
		if !reported[name] {
			t.Errorf("%s is not in the readout — it decides whether, or for how much, a cross-tenant "+
				"reuse-royalty mint settles", name)
		}
	}
}

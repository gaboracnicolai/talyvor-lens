// Package econflags reports the LIVE value of the economy, minting and LXC flags as the running
// binary holds it.
//
// ⚠ WHAT "THE FLAGS" MEANS HERE IS ENFORCED, NOT PROMISED. The sentence above used to read
// "every economy and minting flag", and measured against config.go that was false: four
// money-path flags were absent, one of them named ...MintingEnabled and one of them the ADMIN
// LXC GRANT — the only switch in the process that lets credit come into existence without
// revenue. The population is now pinned by forceoff_transcription_test.go, which derives it from
// config.go's own force-off block plus a Mint-or-LXC name rule, and that file records in its own
// doc comment what the name rule CANNOT see (KeelRoyaltyHaircutEnabled, NodeAutoRouteEnabled).
// Read this readout as: the economy master switch, everything config.go force-offs with it, the
// default-on mint arming loop, the adjacent gates named below, every Mint-or-LXC flag, every
// flag that gates whether a MONEY SURFACE is registered at all (route_gate_census_test.go, rule D),
// and every flag the process ARMS BY DEFAULT unless a written reason excuses it
// (default_armed_census_test.go, rule F).
//
// ⚠ RULE F EXISTS BECAUSE RULES A-E ALL KEY ON HOW A FLAG IS WRITTEN. A force-off block, a name
// shape, a gate type, a pinned set: each is a property of the SOURCE, and none can express the one
// an operator actually needs — is this thing running on a box where nobody set anything? Rule F
// answers it by OBSERVATION (clear every LENS_*, call config.Load, read the struct) rather than by
// parsing, and on the commit that added it that found SEVEN default-armed flags outside this
// readout where the handover had named one. Five were the settlement chain and are now reported;
// the other two carry written reasons in rule F's own allowlist. ⚠ NOTE WHAT IT MEANS THAT
// SettlementFailClosedEnabled WAS ONE OF THEM: it is not in the force-off block, is not Mint-or-LXC
// named, gates no route, and is not even written with parseBoolEnvDefaultTrue — so it was invisible
// to every rule AND to a grep. Only reading the loaded struct sees a flag like that.
//
// ⚠ RULE D EXISTS BECAUSE THE READOUT WAS SILENT ABOUT WHETHER THE PROCESS COULD TAKE MONEY.
// BillingEnabled gates the FIAT intake path — the Stripe webhook that credits a paid purchase and
// the checkout that charges for it. It is outside the force-off block BY DESIGN (fiat billing must
// run with the economy off), so rules A and B cannot see it, and "Billing" matches neither "Mint"
// nor "LXC", so rule C cannot either. A readout of mint flags that says nothing about the fiat
// door reads as "the money paths are quiet" while the only path that takes revenue is unaccounted
// for. Rule D keys on cmd/lens's own gate types rather than on a name, and it caught this one.
//
// WHY IT EXISTS. Nothing on the box showed whether a money-path flag was on. The available
// answer was "read config.go and infer the default", which is inference, not observation — and
// inference is exactly what fails when an env var, a force-off, or a stale deploy makes the
// running process disagree with the source. This reads the actual *config.Config the process is
// using.
//
// THREE RULES, each of which the tests pin:
//
//  1. NEVER REPORT A DEFAULT AS IF IT WERE OBSERVED. If the live struct cannot be read, the
//     report says so and carries NO flag values. A readout that falls back to defaults is
//     worse than no readout: it looks like observation and is not.
//
//  2. "OFF" AND "FORCED OFF" ARE DIFFERENT STATES. config.Load overwrites the whole force-off
//     set with false when EconomyEnabled=false, so by the time anything reads the struct both
//     states are literally `false` and indistinguishable from the value alone. The distinction
//     is derived: a flag is FORCED OFF when EconomyEnabled is false and the flag is in
//     config.go's force-off block. That also tells an operator their env var is not in effect,
//     which the raw value cannot.
//
//  3. NAME THE BINARY. lensVersion is ldflag-stamped with the short commit and defaults to
//     "dev". An unstamped binary is reported as UNSTAMPED rather than as a commit named "dev" —
//     the same discipline as rule 1, applied to identity instead of configuration.
package econflags

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/talyvor/lens/internal/config"
)

// State is a flag's observed condition.
type State string

const (
	StateOn  State = "on"
	StateOff State = "off"
	// StateForcedOff means: the value is false AND it is false because EconomyEnabled is
	// false, regardless of what the environment asked for. Distinct from StateOff, which
	// means the flag is simply not enabled.
	StateForcedOff State = "forced_off"
	// StateForcedOffAtRuntime means the environment asked for this flag and the process is
	// NOT honouring it, because of a decision taken while running rather than at config load.
	//
	// ⚠ WHY THIS IS A SEPARATE STATE. StateForcedOff is derivable from the config struct;
	// this one is NOT visible there at all. Cross-tenant pooling is held off by a live gate
	// when the embedding configuration is not the one that passed `lens poolcheck`, and
	// cfg.CachePoolableEnabled stays TRUE throughout — so reporting from the struct alone
	// would say "on" about a flag that is doing nothing. That is the precise failure this
	// package exists to prevent, applied to itself.
	StateForcedOffAtRuntime State = "forced_off_at_runtime"
)

// Flag is one observed flag.
type Flag struct {
	Name string `json:"name"`
	Env  string `json:"env"`
	// Value is the EFFECTIVE value: what the process is doing, not what it was asked to do.
	Value bool `json:"value"`
	// Configured appears ONLY when the configured value differs from the effective one, so
	// its presence is itself the signal that an override is in force.
	Configured *bool `json:"configured,omitempty"`
	State      State `json:"state"`
	// Note explains a non-obvious state: why the configured value is not the effective one.
	Note string `json:"note,omitempty"`
}

// Override reports a flag whose EFFECTIVE value differs from its configured value because of a
// runtime decision the config struct cannot express. Reason must say why, in terms an operator
// can act on — "pooling off" without a reason is indistinguishable from pooling never having
// been switched on, which is the confusion this whole package exists to remove.
type Override struct {
	Name      string
	Effective bool
	Reason    string
}

// Binary identifies the running binary.
type Binary struct {
	Commit  string `json:"commit,omitempty"`
	Stamped bool   `json:"stamped"`
	Note    string `json:"note,omitempty"`
}

// Groups is the human view: every flag in exactly one bucket.
type Groups struct {
	On                 []string `json:"on"`
	Off                []string `json:"off"`
	ForcedOff          []string `json:"forced_off"`
	ForcedOffAtRuntime []string `json:"forced_off_at_runtime"`
}

// Snapshot is the whole readout.
type Snapshot struct {
	// Observed is false when the live struct could not be read. Callers must treat false as
	// "no information", never as "the defaults apply".
	Observed   bool   `json:"observed"`
	Unreadable string `json:"unreadable,omitempty"`
	Binary     Binary `json:"binary"`
	Flags      []Flag `json:"flags,omitempty"`
	Groups     Groups `json:"groups"`
}

// forceOff is config.go's force-off block, transcribed. A flag here is set to false whenever
// EconomyEnabled is false, whatever the environment said.
//
// ⚠ LXCGatingEnabled and LXCShadowSpendEnabled are DELIBERATELY ABSENT: config.go excludes
// them from the force-off block because LXC is the fiat-pegged usage credit, not the token
// economy, so a fiat-SaaS deployment can meter paid credit with the economy off. Adding them
// here would report a state the binary does not have.
var forceOff = map[string]bool{
	"PatternMiningEnabled":            true,
	"PatternCaptureEnabled":           true,
	"PatternEarningEnabled":           true,
	"PoolRoyaltyMintingEnabled":       true,
	"POVIMintingEnabled":              true,
	"AnnotationMintingEnabled":        true,
	"TrustfulComputeMintEnabled":      true,
	"CacheSharingEnabled":             true,
	"CachePoolableEnabled":            true,
	"DistillPoolableEnabled":          true,
	"RoutingIntelligenceEnabled":      true,
	"RoutingTierCohortsEnabled":       true,
	"EvalContributionMintingEnabled":  true,
	"RoutingPredictionMintingEnabled": true,
	"LatencyMintingEnabled":           true,
	"ConfidentialMintingEnabled":      true,
}

// Report reads cfg and returns the snapshot. cfg==nil ⇒ Observed=false and no values.
//
// binaryVersion is main.lensVersion, passed in rather than imported so this package has no
// dependency on the command.
func Report(cfg *config.Config, binaryVersion string, overrides ...Override) Snapshot {
	snap := Snapshot{Binary: describeBinary(binaryVersion)}

	if cfg == nil {
		snap.Observed = false
		snap.Unreadable = "the running configuration could not be read from this process, so no " +
			"flag values are reported; a default is not an observation"
		return snap
	}
	snap.Observed = true

	// Every flag is listed by NAME against a pointer into the live struct — so the value here
	// is the value the process is using, not a re-read of the environment and not a default.
	type entry struct {
		name string
		env  string
		val  bool
	}
	entries := []entry{
		{"EconomyEnabled", "LENS_ECONOMY_ENABLED", cfg.EconomyEnabled},

		// The default-on loop (config.go): true unless the env overrides, then force-off'd.
		{"PoolRoyaltyMintingEnabled", "LENS_POOL_ROYALTY_MINTING_ENABLED", cfg.PoolRoyaltyMintingEnabled},
		{"CachePoolableEnabled", "LENS_CACHE_POOLABLE_ENABLED", cfg.CachePoolableEnabled},
		{"DistillPoolableEnabled", "LENS_DISTILL_POOLABLE_ENABLED", cfg.DistillPoolableEnabled},
		{"PatternEarningEnabled", "LENS_PATTERN_EARNING_ENABLED", cfg.PatternEarningEnabled},

		// The rest of the force-off block.
		{"PatternMiningEnabled", "LENS_PATTERN_MINING_ENABLED", cfg.PatternMiningEnabled},
		{"PatternCaptureEnabled", "LENS_PATTERN_CAPTURE_ENABLED", cfg.PatternCaptureEnabled},
		{"POVIMintingEnabled", "LENS_POVI_MINTING_ENABLED", cfg.POVIMintingEnabled},
		{"AnnotationMintingEnabled", "LENS_ANNOTATION_MINTING_ENABLED", cfg.AnnotationMintingEnabled},
		{"TrustfulComputeMintEnabled", "LENS_TRUSTFUL_COMPUTE_MINT_ENABLED", cfg.TrustfulComputeMintEnabled},
		{"CacheSharingEnabled", "LENS_CACHE_SHARING_ENABLED", cfg.CacheSharingEnabled},
		{"RoutingIntelligenceEnabled", "LENS_ROUTING_INTELLIGENCE_ENABLED", cfg.RoutingIntelligenceEnabled},
		{"RoutingTierCohortsEnabled", "LENS_ROUTING_TIER_COHORTS_ENABLED", cfg.RoutingTierCohortsEnabled},
		{"EvalContributionMintingEnabled", "LENS_EVAL_CONTRIBUTION_MINTING_ENABLED", cfg.EvalContributionMintingEnabled},
		{"RoutingPredictionMintingEnabled", "LENS_ROUTING_PREDICTION_MINTING_ENABLED", cfg.RoutingPredictionMintingEnabled},
		{"LatencyMintingEnabled", "LENS_LATENCY_MINTING_ENABLED", cfg.LatencyMintingEnabled},
		{"ConfidentialMintingEnabled", "LENS_CONFIDENTIAL_MINTING_ENABLED", cfg.ConfidentialMintingEnabled},

		// Adjacent capability gate the four P-o-I mints are ANDed with — off here means those
		// four cannot pay even when their own flag is on, which a reader needs to see.
		{"ProofOfImprovementEnabled", "LENS_PROOF_OF_IMPROVEMENT_ENABLED", cfg.ProofOfImprovementEnabled},

		// SHADOW MODE. On means the six unproven mints record hypotheticals and credit nothing —
		// so a reader seeing POVIMintingEnabled=on AND ShadowMintsEnabled=on must understand that
		// POVI is NOT paying. Reported adjacent to the mints it neutralises for exactly that reason.
		{"ShadowMintsEnabled", "LENS_SHADOW_MINTS_ENABLED", cfg.ShadowMintsEnabled},

		// A MINT MODULATOR, not a mint gate: the reputation bond can only REDUCE or block a
		// PoVI/pool-royalty mint the floor and rate cap already allowed. config.go keeps it out
		// of the force-off block for that reason. It is reported because an operator asking
		// "what is affecting the mint amount" gets a wrong answer from the mint gates alone —
		// the ledger reads it live (cmd/lens/main.go SetReputationGate).
		{"ReputationBondedMintingEnabled", "LENS_REPUTATION_BONDED_MINTING_ENABLED", cfg.ReputationBondedMintingEnabled},

		// LXC: fiat usage credit, NOT force-off'd with the economy. Reported so the readout is
		// complete, and so its absence from the forced-off group is visible rather than assumed.
		{"LXCGatingEnabled", "LENS_LXC_GATING_ENABLED", cfg.LXCGatingEnabled},
		{"LXCShadowSpendEnabled", "LENS_LXC_SHADOW_SPEND_ENABLED", cfg.LXCShadowSpendEnabled},
		// The LXC CREATION path. GrantLXC is the same atomic ledger+balance move as a purchase,
		// recorded as an admin_grant — so this is the one flag in the readout that lets credit
		// come into existence without revenue. Off ⇒ the route is never registered. Reporting the
		// two LXC flags above and not this one described the spending of LXC while staying silent
		// about its minting.
		{"AdminLXCGrantEnabled", "LENS_ADMIN_LXC_GRANT_ENABLED", cfg.AdminLXCGrantEnabled},
		// The two DEFAULT-TRUE LXC spend-path flags. Both are money behaviour an operator reading
		// this page would otherwise have to infer from source: Reservation decides whether the
		// customer is billed the DELIVERED cost or a pre-serve estimate, and AgentAllocation is
		// the per-scoped-key sub-budget bound. Their default-on state is precisely why omitting
		// them mattered — a readout of mostly-off flags reads as "the money paths are quiet".
		{"LXCReservationEnabled", "LENS_LXC_RESERVATION_ENABLED", cfg.LXCReservationEnabled},
		{"LXCAgentAllocationEnabled", "LENS_LXC_AGENT_ALLOCATION_ENABLED", cfg.LXCAgentAllocationEnabled},

		// THE FIAT DOOR. Off ⇒ cmd/lens never registers POST /v1/billing/webhook (the Stripe
		// callback that CREDITS a paid purchase), the checkout that CHARGES for it, or the admin
		// purchases list — chi answers 404. This is the one flag in the readout that decides
		// whether REVENUE can arrive, the mirror of AdminLXCGrantEnabled above (credit without
		// revenue). Deliberately NOT force-off'd with the economy: a pure fiat-SaaS deployment
		// runs EconomyEnabled=false + BillingEnabled=true, which is exactly why every other rule
		// in this package is blind to it.
		{"BillingEnabled", "LENS_BILLING_ENABLED", cfg.BillingEnabled},
		// THE UNBILLED BATCH LANE. batch_routes.go opens it only when the operator asked AND a
		// billing settle hook is wired, because a completed job on an unhooked lane "would debit
		// nothing while Talyvor pays the provider". The flag alone is therefore NOT the effective
		// state — cmd/lens passes the gate's real decision as an Override, so an operator who set
		// LENS_BATCH_ENABLED=true and got a refused lane reads forced_off_at_runtime here instead
		// of an "on" that is not true.
		{"BatchEnabled", "LENS_BATCH_ENABLED", cfg.BatchEnabled},

		// ── THE SETTLEMENT CHAIN. Five DEFAULT-ARMED flags that decide whether a cross-tenant
		// reuse-royalty mint ever settles, and for how much. Every one of them was outside every
		// rule in this package until rule E (default_armed_census_test.go) measured the ARMED set
		// by clearing the environment and calling Load() instead of reading a source shape.
		//
		// ⚠ WHY THE OMISSION WAS THE EXPENSIVE KIND: rules A–D all key on how a flag is WRITTEN
		// (config.go's force-off block, a Mint-or-LXC name, cmd/lens's gate types). None of them
		// can express the property an operator actually needs — "is this armed when I set
		// nothing?" — and these five are armed for a fresh deployment. A readout of mostly-off
		// flags reads as "the money paths are quiet"; five of the loudest were simply not on it.

		// Off ⇒ the finalize sweeper settles 'held' rows as before, so a royalty mint the ring
		// detector NEVER EXAMINED settles anyway. On ⇒ only 'cleared' rows settle, and an
		// unexamined hold never does. Read at 13 sites in cmd/lens. This is the flag that answers
		// "can money move without being looked at", and the readout could not answer it.
		{"SettlementFailClosedEnabled", "LENS_SETTLEMENT_FAIL_CLOSED_ENABLED", cfg.SettlementFailClosedEnabled},
		// The EXAMINATION half of the same decision, and the two are only meaningful together.
		// Off ⇒ the leader-elected ring-detector sweep does not run, so with fail-closed ON
		// nothing is ever examined, nothing is ever cleared, and every held mint holds forever.
		// config.go calls this "only a manual off-switch" — which is exactly the operator action
		// whose consequence this readout exists to make visible.
		{"DetectorSweepEnabled", "LENS_DETECTOR_SWEEP_ENABLED", cfg.DetectorSweepEnabled},
		// KE-2: a REDUCE-ONLY haircut that LOWERS a reuse-royalty mint on a sustained hardened
		// drift finding. It can never slash, never increase, never go below the floor — but it
		// changes what a contributor is paid, on an admittedly UNCALIBRATED placeholder threshold.
		// It matches neither the force-off block nor a Mint-or-LXC name, which is how it stayed off
		// a readout of mint flags while modulating a mint.
		{"KeelRoyaltyHaircutEnabled", "LENS_KEEL_ROYALTY_HAIRCUT_ENABLED", cfg.KeelRoyaltyHaircutEnabled},
		// The DETECTOR whose findings the haircut above consumes. Reported because "the haircut is
		// armed" and "anything can produce a finding for it to act on" are different facts, and the
		// first alone is the more reassuring half.
		{"KeelHardenedEnabled", "LENS_KEEL_HARDENED_ENABLED", cfg.KeelHardenedEnabled},
		// ⚠ THE PARENT GATE, AND IT IS REPORTED FOR THIS PACKAGE'S OWN RULE 1 REASON. The two Keel
		// entries above are nested inside `if cfg.KeelEnabled` in cmd/lens, so reporting them ON
		// while this is OFF would describe two armed mechanisms that are doing nothing — the
		// precise failure StateForcedOffAtRuntime exists to prevent, one level up. config.go
		// declares this one mint-free and read-only; it is here as the enclosing condition of two
		// flags that are not.
		{"KeelEnabled", "LENS_KEEL_ENABLED", cfg.KeelEnabled},
	}

	byName := make(map[string]Override, len(overrides))
	for _, o := range overrides {
		byName[o.Name] = o
	}

	for _, e := range entries {
		f := Flag{Name: e.name, Env: e.env, Value: e.val}
		// A runtime override wins over everything derived from the struct, because it IS the
		// effective behaviour and the struct cannot see it.
		if o, ok := byName[e.name]; ok && o.Effective != e.val {
			configured := e.val
			f.Value, f.Configured, f.Note = o.Effective, &configured, o.Reason
			if o.Effective {
				f.State = StateOn
			} else {
				f.State = StateForcedOffAtRuntime
			}
			snap.Flags = append(snap.Flags, f)
			if f.State == StateOn {
				snap.Groups.On = append(snap.Groups.On, f.Name)
			} else {
				snap.Groups.ForcedOffAtRuntime = append(snap.Groups.ForcedOffAtRuntime, f.Name)
			}
			continue
		}
		switch {
		case e.val:
			f.State = StateOn
		case !cfg.EconomyEnabled && forceOff[e.name]:
			f.State = StateForcedOff
			f.Note = "false because EconomyEnabled is false; config.Load overwrites this flag " +
				"in the force-off block, so any configured value is NOT in effect"
		default:
			f.State = StateOff
		}
		snap.Flags = append(snap.Flags, f)

		switch f.State {
		case StateOn:
			snap.Groups.On = append(snap.Groups.On, f.Name)
		case StateForcedOff:
			snap.Groups.ForcedOff = append(snap.Groups.ForcedOff, f.Name)
		default:
			snap.Groups.Off = append(snap.Groups.Off, f.Name)
		}
	}
	return snap
}

// describeBinary reports the build identity. "dev" is the ldflags default, i.e. UNSTAMPED —
// reporting it as a commit would be the same defect as reporting a config default as observed.
func describeBinary(v string) Binary {
	t := strings.TrimSpace(v)
	if t == "" || t == "dev" {
		return Binary{
			Stamped: false,
			Note: "this binary carries no commit stamp (lensVersion is unset or the \"dev\" " +
				"placeholder), so the code that produced this readout cannot be identified",
		}
	}
	return Binary{Commit: t, Stamped: true}
}

// Handler serves the snapshot as JSON.
//
// It closes over the LIVE *config.Config pointer the process was built with — not a copy taken
// at startup, so a value mutated at runtime is reported as it currently is. cfg==nil produces
// the Observed=false payload rather than a 500: "I cannot read this" is the honest answer and
// is more useful than an error.
//
// ⚠ MOUNT THIS BEHIND requireAdmin. It describes the money path and is not public. The route
// registration in cmd/lens/main.go is the only place that decides, and a test there pins it.
// overrides, when supplied, is evaluated PER REQUEST rather than captured once: the pooling
// gate re-reads its attestation on a timer, so a snapshot taken at wiring time would go stale
// in exactly the situation this reports on.
func Handler(cfg *config.Config, binaryVersion string, overrides ...func() []Override) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			w.WriteHeader(http.StatusMethodNotAllowed)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "method not allowed"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// No-store: this is a point-in-time observation of mutable state. A cached copy read
		// later would be exactly the stale inference the endpoint replaces.
		w.Header().Set("Cache-Control", "no-store")
		var live []Override
		for _, fn := range overrides {
			if fn != nil {
				live = append(live, fn()...)
			}
		}
		_ = json.NewEncoder(w).Encode(Report(cfg, binaryVersion, live...))
	}
}

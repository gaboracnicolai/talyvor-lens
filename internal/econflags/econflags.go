// Package econflags reports the LIVE value of every economy and minting flag as the running
// binary holds it.
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
)

// Flag is one observed flag.
type Flag struct {
	Name  string `json:"name"`
	Env   string `json:"env"`
	Value bool   `json:"value"`
	State State  `json:"state"`
	// Note explains a non-obvious state — currently only forced_off, where the configured
	// value is not the effective one.
	Note string `json:"note,omitempty"`
}

// Binary identifies the running binary.
type Binary struct {
	Commit  string `json:"commit,omitempty"`
	Stamped bool   `json:"stamped"`
	Note    string `json:"note,omitempty"`
}

// Groups is the human view: every flag in exactly one bucket.
type Groups struct {
	On        []string `json:"on"`
	Off       []string `json:"off"`
	ForcedOff []string `json:"forced_off"`
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
func Report(cfg *config.Config, binaryVersion string) Snapshot {
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

		// LXC: fiat usage credit, NOT force-off'd with the economy. Reported so the readout is
		// complete, and so its absence from the forced-off group is visible rather than assumed.
		{"LXCGatingEnabled", "LENS_LXC_GATING_ENABLED", cfg.LXCGatingEnabled},
		{"LXCShadowSpendEnabled", "LENS_LXC_SHADOW_SPEND_ENABLED", cfg.LXCShadowSpendEnabled},
	}

	for _, e := range entries {
		f := Flag{Name: e.name, Env: e.env, Value: e.val}
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
func Handler(cfg *config.Config, binaryVersion string) http.HandlerFunc {
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
		_ = json.NewEncoder(w).Encode(Report(cfg, binaryVersion))
	}
}

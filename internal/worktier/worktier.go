// Package worktier is the DESCRIPTIVE work classifier (Master Plan WorkTier).
// It tiers the UNIT OF WORK (each served request) along four axes, post-serve,
// from NON-CONTENT signals. It is NEVER incentivized — nothing mints from a tier
// (the descriptive-never-incentivized doctrine, distillattrib/store.go:8). The
// classifier here is a PURE function (numbers + booleans in, a WorkTier out), so
// this file imports NOTHING that could reach a ledger; the persistence layer
// (store.go) holds only an Exec/Query handle, never a credit path. Mint-free by
// construction.
//
// CONTRACT (the pre-freeze API surface): the four axis NAMES and their enum
// VALUE SETS are a near-permanent contract — consumers (the routing Advisor,
// dashboards, analytics) switch on them, so renaming/removing a value is
// breaking. The THRESHOLDS behind the buckets are implementation: store.go
// persists the RAW signal behind EVERY axis, so historical rows are re-bucketable
// offline when a threshold or the complexity scorer evolves. Freeze the
// interface, not the implementation.
//
// Complexity is the one SOFTER axis: its enum maps to an EVOLVING prompt scorer
// (router.AnalyseComplexity), not a stable physical unit like tokens or dollars.
// Consumers should treat Complexity as ADVISORY; Size/Cost/Sensitivity map to
// durable quantities/categories.
package worktier

type (
	SizeBucket  string
	CostBucket  string
	Complexity  string
	Sensitivity string
)

const (
	// Size — total-token magnitude (input+output). Tokens are a universal unit.
	SizeSmall  SizeBucket = "small"  // < 1,000 total tokens
	SizeMedium SizeBucket = "medium" // 1,000 – 9,999
	SizeLarge  SizeBucket = "large"  // 10,000 – 99,999
	SizeXLarge SizeBucket = "xlarge" // >= 100,000

	// Cost — USD magnitude. THE quality-per-dollar axis; decoupled from Size.
	CostTrivial  CostBucket = "trivial"  // < $0.001
	CostLow      CostBucket = "low"      // $0.001 – $0.0099
	CostModerate CostBucket = "moderate" // $0.01 – $0.099
	CostHigh     CostBucket = "high"     // >= $0.10

	// Complexity — from router.AnalyseComplexity().Score() ∈ [0,5]. The SOFT axis.
	ComplexityTrivial  Complexity = "trivial"  // score 0
	ComplexitySimple   Complexity = "simple"   // score 1–2
	ComplexityModerate Complexity = "moderate" // score 3–4
	ComplexityComplex  Complexity = "complex"  // score 5

	// Sensitivity — privacy/safety weight. Precedence: restricted > elevated > normal.
	SensitivityNormal     Sensitivity = "normal"
	SensitivityElevated   Sensitivity = "elevated"   // PII present OR a guardrail fired
	SensitivityRestricted Sensitivity = "restricted" // workspace logging policy == "none"
)

// WorkTier is the descriptive classification of one served request. Each axis
// answers a distinct routing question: Size = magnitude, Cost = the $ axis,
// Complexity = could-a-cheaper-model, Sensitivity = can-it-go-pooled.
type WorkTier struct {
	Size        SizeBucket  `json:"size"`
	Cost        CostBucket  `json:"cost"`
	Complexity  Complexity  `json:"complexity"`
	Sensitivity Sensitivity `json:"sensitivity"`
}

// Classify derives the WorkTier from post-serve, NON-CONTENT signals. PURE — no
// I/O, no imports that could mint. complexityScore is router.AnalyseComplexity().
// Score() (the proxy computes it on the same input the router analyzed, so the
// tier's complexity equals the routing decision's). loggingPolicy is the
// workspace's policy string ("none" / "metadata" / "full").
func Classify(inputTokens, outputTokens int, costUSD float64, complexityScore int, piiDetected, guardrailFired bool, loggingPolicy string) WorkTier {
	return WorkTier{
		Size:        sizeBucket(inputTokens + outputTokens),
		Cost:        costBucket(costUSD),
		Complexity:  complexityBucket(complexityScore),
		Sensitivity: sensitivityFor(piiDetected, guardrailFired, loggingPolicy),
	}
}

func sizeBucket(total int) SizeBucket {
	switch {
	case total >= 100_000:
		return SizeXLarge
	case total >= 10_000:
		return SizeLarge
	case total >= 1_000:
		return SizeMedium
	default:
		return SizeSmall
	}
}

func costBucket(usd float64) CostBucket {
	switch {
	case usd >= 0.10:
		return CostHigh
	case usd >= 0.01:
		return CostModerate
	case usd >= 0.001:
		return CostLow
	default:
		return CostTrivial
	}
}

func complexityBucket(score int) Complexity {
	switch {
	case score >= 5:
		return ComplexityComplex
	case score >= 3:
		return ComplexityModerate
	case score >= 1:
		return ComplexitySimple
	default:
		return ComplexityTrivial
	}
}

// ComplexityBucketFor is the EXPORTED bucketer (the same mapping Classify uses). It is the
// single source of truth shared by the routing tier-cohort consumer: pattern-capture stamps
// routing_patterns.complexity_bucket with it (the WRITE bucket) and the serve path passes it to
// the Advisor (the LOOKUP bucket), so write-bucket == lookup-bucket by construction. Pure +
// mint-free (this package imports nothing that reaches a ledger).
func ComplexityBucketFor(score int) Complexity { return complexityBucket(score) }

// sensitivityFor applies precedence restricted > elevated > normal: a no-logging
// workspace is restricted regardless of PII/guardrail; otherwise PII OR a
// guardrail trip is elevated.
func sensitivityFor(pii, guardrail bool, loggingPolicy string) Sensitivity {
	if loggingPolicy == "none" {
		return SensitivityRestricted
	}
	if pii || guardrail {
		return SensitivityElevated
	}
	return SensitivityNormal
}

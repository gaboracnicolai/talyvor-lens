package config

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

type Config struct {
	ListenAddr        string
	RedisURL          string
	DatabaseURL       string
	NatsURL           string
	OpenAIAPIKey      string
	AnthropicAPIKey   string
	GoogleAPIKey      string
	EmbeddingModel    string
	EmbeddingBaseURL  string // LENS_EMBEDDING_BASE_URL override; empty = OpenAI default. Process-env only — no request input can set it (mirrors VLLMBaseURL; offline trial harness).
	SemanticThreshold float64
	MaxCacheTTL       time.Duration
	LogLevel          string
	OllamaURL         string

	// SemanticCacheRetention is the single sliding window for Postgres
	// semantic-cache rows (prompt_embeddings). Every cache hit bumps
	// updated_at = NOW(), so the window restarts on each use. It governs BOTH
	// halves of a row's life:
	//   - SERVE: semanticSelectSQL only returns rows with
	//     updated_at > NOW() − retention, so an entry stays servable for the
	//     full window and each hit resets it.
	//   - STORE: a background sweeper DELETEs any row past the window, bounding
	//     the otherwise-unbounded growth of the table and of its ivfflat
	//     similarity index from one-off prompts that are never reused.
	// Because serving and storage share one window, a cached answer can be
	// served until it is this old — a deliberate reuse-over-freshness tradeoff;
	// lower this single value to tighten both together.
	//
	// Default 21 days (504h). Env: LENS_SEMANTIC_CACHE_RETENTION (Go duration;
	// units h/m/s — NOT "d"; use "504h" for 21 days). A value <= 0 DISABLES
	// both halves: rows are served regardless of age and never swept (kept
	// indefinitely — the pre-retention behavior).
	SemanticCacheRetention time.Duration

	// SemanticCacheSweepInterval is how often the retention sweeper runs.
	// Default 1h. Must be > 0. Env: LENS_SEMANTIC_CACHE_SWEEP_INTERVAL (Go
	// duration). Only consulted when SemanticCacheRetention > 0.
	SemanticCacheSweepInterval time.Duration

	// U14 audit-trail integrity. All DEFAULT OFF.
	//
	// AuditRetention is the sliding window for the token_events retention sweeper
	// (token_events ONLY — there is no table knob; it can never touch a ledger).
	// Env LENS_AUDIT_RETENTION (Go duration). <= 0 / unset DISABLES the sweeper.
	AuditRetention time.Duration

	// AuditExportURL / AuditExportInterval drive the leader-elected off-box export
	// loop (reuses the audit webhook exporter). Empty URL = no loop (default off).
	// Env: LENS_AUDIT_EXPORT_URL, LENS_AUDIT_EXPORT_INTERVAL (Go duration, default 1h).
	AuditExportURL      string
	AuditExportInterval time.Duration

	// AuditRequireExportBeforePrune (U14 #187) gates the retention sweeper so it
	// deletes a token_events row ONLY once it is at/below the off-box export
	// watermark (audit_export_state.last_exported_at) — proof-of-export-before-
	// delete. When export is disabled, the sweep is SKIPPED entirely (it never
	// prunes un-exportable rows). Env LENS_AUDIT_REQUIRE_EXPORT_BEFORE_PRUNE;
	// default false = today's age-only pruning (no behaviour change).
	AuditRequireExportBeforePrune bool

	// WorkspaceReloadInterval (U7b) is how often each replica rebuilds its
	// in-memory workspace-config cache from Postgres, bounding cross-replica
	// staleness of logging policy + the cache-pooling privacy flag. Env:
	// LENS_WORKSPACE_RELOAD_INTERVAL (Go duration). Default 30s (tighter than the
	// budgets 60s — cache-pooling is a privacy decision; the reload is one cheap
	// SELECT). MUST be > 0 (a time.Ticker panics on a non-positive interval).
	WorkspaceReloadInterval time.Duration

	// WorkspaceMaxStaleness is the fail-closed bound for the workspace cache: once a
	// replica has gone this long without a SUCCESSFUL reload (a prolonged DB outage),
	// the consent/privacy accessors (cache_poolable, distill_poolable, distill_policy,
	// logging) return their conservative value instead of a possibly-revoked
	// permissive one. Env: LENS_WORKSPACE_MAX_STALENESS (Go duration). Default 3× the
	// reload interval (90s at the 30s default). MUST be > WorkspaceReloadInterval — a
	// bound <= the interval would trip during normal operation.
	WorkspaceMaxStaleness time.Duration

	// GuardrailsReloadInterval (#189) is how often each replica rebuilds its
	// in-memory custom guardrail-policy cache from Postgres (guardrail_policies),
	// bounding cross-replica staleness of a SECURITY control. Env:
	// LENS_GUARDRAILS_RELOAD_INTERVAL (Go duration). Default 30s. MUST be > 0.
	GuardrailsReloadInterval time.Duration

	// AWS Bedrock — all four fields are optional; an empty AccessKeyID
	// keeps HandleBedrock in 503 graceful-degradation mode. SessionToken
	// is only set when the deployment uses STS / assumed-role creds.
	AWSRegion          string
	AWSAccessKeyID     string
	AWSSecretAccessKey string
	AWSSessionToken    string

	// OpenAI-compatible providers: Mistral, Groq, vLLM. All optional.
	// Empty mistral/groq key → 503 on those routes. Empty VLLMBaseURL
	// → 503 on the vLLM route (vLLM has no default endpoint — the
	// operator chooses where to run the inference server).
	MistralAPIKey string
	GroqAPIKey    string
	VLLMBaseURL   string
	VLLMAPIKey    string

	// QualityAutoRetry enables a one-shot retry when a response
	// scores below quality.AutoRetryThreshold. Off by default;
	// the operator opts in via LENS_QUALITY_AUTO_RETRY=true.
	QualityAutoRetry bool

	// LocalEndpoints holds the LENS_LOCAL_ENDPOINTS env value,
	// in the format consumed by localrouter.ParseEndpointsConfig:
	//   provider:url:model1,model2;provider:url:model3
	// Empty string means "no multi-endpoint local routing" —
	// the legacy LENS_OLLAMA_URL single-endpoint path still works.
	LocalEndpoints string

	// JWT auth. JWTPrivateKey is a PEM-encoded EC P-256 private key used to
	// sign and verify tokens (ES256). Generate one with:
	//
	//   openssl ecparam -name prime256v1 -genkey -noout | \
	//     openssl pkcs8 -topk8 -nocrypt -out ec-private.pem
	//   export LENS_JWT_PRIVATE_KEY=$(cat ec-private.pem)
	//
	// Empty string is allowed — Lens generates an ephemeral key at startup for
	// single-instance dev; production deployments must persist the key so
	// tokens remain valid across restarts and across instances in an HA cluster.
	// Env: LENS_JWT_PRIVATE_KEY.
	JWTPrivateKey string
	TokenTTL      time.Duration

	// Global rate limits (Item 8). Zero = no global cap; the
	// per-workspace tier in MultiTierLimiter still applies.
	GlobalRPM       int
	GlobalTPM       int
	BurstMultiplier float64

	// Retry / circuit-breaker tuning (Item 9). Zero values fall
	// back to the library defaults in internal/retry/policy.go.
	RetryMaxAttempts  int
	RetryInitialDelay time.Duration
	RetryMaxDelay     time.Duration
	CBThreshold       int
	CBResetTimeout    time.Duration

	// Cache contribution mining (Batch 2 Item 1). Opt-in —
	// when false, cross-workspace cache hits earn only the
	// same-workspace tiny reward (effectively "no sharing
	// economy", but mining still runs for own-cache hits).
	CacheSharingEnabled bool

	// CachePoolableEnabled is the GLOBAL switch for the Phase-2 Stage 2.0
	// shared-cache governance gate (exact cache). Default false: cross-tenant
	// pooling is impossible unless this is on AND both the contributing and
	// requesting workspaces have cache_poolable=true. Inert by default — the
	// request path is byte-for-byte unchanged when this is off.
	CachePoolableEnabled bool

	// DistillPoolableEnabled is the GLOBAL switch for cross-tenant DISTILL-cache
	// sharing (the document-artifact analogue of CachePoolableEnabled). Default
	// false: a distill artifact is served only within its producing workspace
	// unless this is on AND both the owner and requester have
	// distill_poolable=true. Inert by default — with this off the distill cache
	// is strictly per-workspace (LENS_DISTILL_POOLABLE_ENABLED).
	DistillPoolableEnabled bool

	// Pattern mining (Batch 2 Item 5). Deployment-level gate;
	// must AND with per-workspace opt-in for earnings to fire.
	PatternMiningEnabled bool

	// POVIMintingEnabled gates PROVISIONAL receipt-based LENS minting
	// (Token Economy Phase 1, Part 1). DEFAULT FALSE and intentionally so:
	// minting from a signed receipt is UNSAFE on receipt-alone — a node can
	// sign a fabricated trace and this layer can't catch it. Real safety needs
	// stake + random challenge-and-slash (Parts 2/3). When false (default),
	// PoVI verifies + records receipts for audit but mints NOTHING. Only flip
	// LENS_POVI_MINTING_ENABLED in a test/throwaway context — never production
	// until Parts 2/3 land.
	POVIMintingEnabled bool

	// PoolRoyaltyMintingEnabled gates the Phase-2 Stage 2.1 Pool-B royalty
	// mint: a served cross-tenant pooled cache hit mints s × avoided_COGS to
	// the contributing tenant, exactly once per serving request. DEFAULT
	// FALSE — with this off, pooled hits serve exactly as Stage 2.0 left
	// them and NOTHING mints. Minting additionally requires the pooling
	// gate itself (LENS_CACHE_POOLABLE_ENABLED + both workspaces opted in)
	// to produce a pooled hit in the first place.
	PoolRoyaltyMintingEnabled bool

	// LXCShadowSpendEnabled gates the Phase-2 Stage 2.4/2.5 SHADOW LXC spend
	// path — the first live-serving-path change. When ON, an AI call records a
	// shadow LXC debit (cost_usd / LXCUSDValue) AFTER the response is served,
	// alongside (never replacing) the cost_usd write. OBSERVATIONAL ONLY: the
	// debit is non-gating, post-serve, and error-swallowed, so it cannot block,
	// delay, or alter any request. DEFAULT FALSE — with this off the shadow
	// path is fully inert (no SpendLXC call at all). Env:
	// LENS_LXC_SHADOW_SPEND_ENABLED.
	LXCShadowSpendEnabled bool

	// LXCGatingEnabled gates the Phase-2 Stage 2.4/2.5 LXC GATING step — the
	// pre-serve check that BLOCKS a request (402) when the workspace's LXC
	// balance can't cover the estimated cost. The first gate that can alter
	// whether a request succeeds. DEFAULT FALSE. COHERENCE: gating is inert
	// unless LXCShadowSpendEnabled is ALSO on — gating means "block then
	// debit," but the only serving-path debit is the shadow path, so gating
	// without shadow would block with no accounting. Two-flag staging:
	// shadow=observe, shadow+gating=enforce. Env: LENS_LXC_GATING_ENABLED.
	LXCGatingEnabled bool

	// PatternCaptureEnabled gates the Phase-3 routing-pattern CAPTURE WRITE —
	// the producer for the already-live routing Advisor. When ON, a served
	// model call persists an anonymized routing observation (post-serve, void)
	// for OPTED-IN workspaces only. CAPTURE-ONLY and structurally MINT-FREE:
	// it never reaches ledger.Credit (earning is a separate later stage).
	// DEFAULT FALSE. Env: LENS_PATTERN_CAPTURE_ENABLED. Separate from
	// PatternMiningEnabled (which gates the opt-in HTTP route, unchanged).
	PatternCaptureEnabled bool

	// PatternEarningEnabled gates the Phase-3 routing-pattern EARNING wire-up
	// (S4) — the FIRST pattern-earning stage that touches the serve path. When
	// ON, an opted-in, authenticated workspace's served request routes into
	// RecordPattern (credits LENS) instead of the mint-free capture write.
	// DEFAULT FALSE — flag-off, the serve path is byte-identical to capture-only
	// today. SEPARATE from PatternCaptureEnabled (capture and earn gate
	// independently). Env: LENS_PATTERN_EARNING_ENABLED.
	PatternEarningEnabled bool

	// PoolMintCapPerPair is the Pool-B mint cap (2.3b primitive #1): the max
	// royalty mints per (requester, contributor) pair per rolling window.
	// 0 (default) = cap disabled. The cap is what bounds any gaming vector's
	// worst case to cap × s × avoided_COGS per pair per window — the
	// arithmetic behind the Option-C deterrence + bounded-exposure posture.
	// Env: LENS_POOL_MINT_CAP_PER_PAIR.
	PoolMintCapPerPair int

	// PoolMintCapPerEntry is the Pool-B per-ENTRY mint cap (2.3b follow-up):
	// max royalty mints per entry_id per rolling window, across ALL
	// contributors (semantic ownership churn lets one entry_id accrue mints
	// under different contributor identities over the window, so the per-pair
	// cap alone doesn't bound per-entry exposure). 0 (default) = disabled.
	// Shares PoolMintCapWindow. Env: LENS_POOL_MINT_CAP_PER_ENTRY.
	PoolMintCapPerEntry int

	// PoolMintCapWindow is the rolling window the per-pair cap counts over
	// (no period anchor, no state — matches the SpendLimitUSD rolling-window
	// precedent). Only consulted when the cap is enabled. Default 24h.
	// Env: LENS_POOL_MINT_CAP_WINDOW (Go duration, e.g. 48h).
	PoolMintCapWindow time.Duration

	// DistillMintCap* are the PR1 distill reuse-royalty mint caps — the same
	// deflationary primitive as PoolMintCap* but over distill_royalty_mints (a
	// SEPARATE budget; distill is its own table). PerPair caps mints per
	// (owner, requester); PerContent caps mints per content_hash across requesters.
	// Default 0/0 (off) + 24h. Env: LENS_DISTILL_MINT_CAP_PER_PAIR /
	// LENS_DISTILL_MINT_CAP_PER_CONTENT / LENS_DISTILL_MINT_CAP_WINDOW. NOT in the
	// economy force-off block — a cap only DENIES a mint, never enables one.
	DistillMintCapPerPair    int
	DistillMintCapPerContent int
	DistillMintCapWindow     time.Duration

	// PatternEarnCapPerWorkspace is the S2 routing-pattern EARN cap: max
	// pattern-mine credits per workspace per rolling window. DELIBERATELY
	// DEFAULTS to a REAL limit (50000), NOT 0 — pattern-earning is a
	// self-generated, gamed surface, so it must not be able to exist uncapped
	// behind a single flag (diverges from Pool-B's default-0 consciously). The
	// ceiling is anchored to the ATTACK case: with S1's corroboration floor an
	// attacker can't self-manufacture the premium, so pure-volume abuse earns
	// only base (0.001) → ~50 LENS/ws/24h max. 0 = explicit ops-disable, never
	// the default. Inert until S4 wires the earn path. Env:
	// LENS_PATTERN_EARN_CAP_PER_WORKSPACE.
	PatternEarnCapPerWorkspace int

	// PatternEarnCapWindow is the rolling window for the pattern earn cap
	// (matches Pool-B's window precedent). Default 24h. Env:
	// LENS_PATTERN_EARN_CAP_WINDOW (Go duration).
	PatternEarnCapWindow time.Duration

	// MintRateCapLENS24h is the U6 PR2 per-identity earning-rate cap: the max
	// LENS a single workspace may MINT (across ALL mint types: cache/compute/
	// embedding/annotation/pattern/PoVI/pool-royalty-held) in a rolling 24h
	// window, enforced at the ledger chokepoint. It is the universal steady-state
	// bound on Sybil wash yield — verification (the floor) raised the entry bar
	// but not the steady-state yield, so this caps it. DEFAULT 1000 (≈ $100/day
	// realizable at the $0.10 peg) — generous for a legitimate contributor,
	// bounding a washed identity; tighten at flip-on. 0 = explicit ops-disable
	// (never the default; default-ON-with-a-ceiling keeps inaction safe). A
	// safety restriction — NOT force-off'd by the economy kill. Env:
	// LENS_MINT_RATE_CAP_LENS_24H.
	MintRateCapLENS24h float64

	// PoolHoldbackWindow is the Stage-2.3a holdback: a pool-royalty mint
	// credits HELD balance, and only becomes spendable (and supply-counted)
	// when the finalize sweeper settles it after this window. Default 72h —
	// sized so the statistical gaming vectors (detectable in hours from the
	// durable claim rows) are comfortably inside it. TRIGGER-AGNOSTIC by
	// design: the ledger ops (CreditHeldTx/FinalizeHeldTx/RevokeHeldTx) don't
	// know what fires them — the timed sweeper is just the initial trigger,
	// and billing settlement can replace it later without ledger changes.
	// Env: LENS_POOL_HOLDBACK_WINDOW (Go duration).
	PoolHoldbackWindow time.Duration

	// Detector thresholds (Stage 2.3b) — tune the FLAGGED boolean of the
	// on-demand DetectorReader; they never affect the raw metrics it returns
	// and never trigger any action (detection only surfaces; revoke stays the
	// deliberate 2.3a operator decision). All have sensible defaults so the
	// detectors are usable out of the box.
	DetectVolumeMinMints      int     // LENS_DETECT_VOLUME_MIN_MINTS (default 50)
	DetectVolumeMaxRequesters int     // LENS_DETECT_VOLUME_MAX_REQUESTERS (default 2)
	DetectBilateralMinFrac    float64 // LENS_DETECT_BILATERAL_MIN_FRAC (default 0.9, [0,1])
	DetectBilateralMinMints   int     // LENS_DETECT_BILATERAL_MIN_MINTS (default 20)
	DetectSimilarityMinSample int     // LENS_DETECT_SIMILARITY_MIN_SAMPLE (default 30, >=1)
	DetectSimilarityMaxStddev float64 // LENS_DETECT_SIMILARITY_MAX_STDDEV (default 0.02, >=0)

	// DetectorSweep* gate the scheduled "smoke detector" — the leader-elected sweep that
	// runs the cache+distill detectors on a cadence and records flagged findings. It runs
	// iff EconomyEnabled AND DetectorSweepEnabled. DetectorSweepEnabled defaults TRUE so
	// detection accompanies minting automatically (the EconomyEnabled gate already makes
	// it free when idle — no mints, nothing to scan); the flag is only a manual off-switch.
	DetectorSweepEnabled  bool          // LENS_DETECTOR_SWEEP_ENABLED (default TRUE)
	DetectorSweepInterval time.Duration // LENS_DETECTOR_SWEEP_INTERVAL (default 1h)

	// LXCAgentAllocationEnabled gates the F4-capstone per-scoped-key LXC sub-budget path (step A). Defaults
	// TRUE (owner's call — the capstone ships armed): when on, an agent's LXC spend is bounded by its
	// sub-budget ceiling (default 50 LXC) and debited exactly-once; a zero-balance agent still spends nothing.
	// When false, the sub-budget path is bypassed (today's plain SpendLXC behavior). It is a SPEND bound, not
	// a mint — deliberately NOT in the economy force-off block (turning it off REMOVES a spend guard, so off
	// is the less-safe state; it is not a mint gate). Env: LENS_LXC_AGENT_ALLOCATION_ENABLED (default TRUE).
	LXCAgentAllocationEnabled bool          // LENS_LXC_AGENT_ALLOCATION_ENABLED (default TRUE)
	DetectorSweepWindow       time.Duration // LENS_DETECTOR_SWEEP_WINDOW (default 24h)

	// Keel* gate the U25 cross-tenant DRIFT-ATTRIBUTION sweep — DEFAULT-OFF capability flag (mint-free,
	// descriptive, read-only; NOT force-off because it never touches money). Thresholds are PLACEHOLDERS —
	// calibrate at N3 turn-on; synthetic data proved only the mechanism, never these values.
	KeelEnabled        bool          // LENS_KEEL_ENABLED (default FALSE)
	KeelDeviationSigma float64       // LENS_KEEL_DEVIATION_SIGMA (default 3.0, placeholder)
	KeelWindowSeconds  int64         // LENS_KEEL_WINDOW_SECONDS (default 3600, placeholder)
	KeelInterval       time.Duration // LENS_KEEL_INTERVAL (sweep tick, default 1h)
	KeelLookback       time.Duration // LENS_KEEL_LOOKBACK (corpus read-back, default 48h)

	// PoolRoyaltyShare is s, the contributor's share of avoided_COGS
	// (Stage 2.1). Env: LENS_POOL_ROYALTY_SHARE. Default 0.5. Must be in
	// [0,1] so Talyvor's net (1−s) × avoided_COGS stays ≥ 0 (the
	// Burn-and-Mint invariant).
	PoolRoyaltyShare float64

	// POVIMinStake is the minimum LENS a node must lock as collateral to be
	// minting-eligible (Part 2). Env: LENS_POVI_MIN_STAKE. Default 100.
	POVIMinStake float64

	// POVIUnbondPeriod is how long staked collateral stays locked AND SLASHABLE
	// after a node begins unbonding — the anti-yank delay (a node can't cheat
	// then instantly withdraw before a challenge slashes it). Env:
	// LENS_POVI_UNBOND_PERIOD as a Go duration (units h/m/s — NOT "d"; use
	// "168h" for 7 days). Default 7*24h.
	POVIUnbondPeriod time.Duration

	// POVIChallengeRate is the fraction of verified receipts challenged per
	// scheduler round (Part 3). Env: LENS_POVI_CHALLENGE_RATE in [0,1].
	// Default 0.1. The economic deterrent: expected cost of cheating =
	// P(challenge) × slash_amount must exceed the gain. Higher rate = stronger
	// deterrent + more overhead; a low rate leaves some bad receipts
	// unchallenged (probabilistic, not absolute).
	POVIChallengeRate float64

	// POVISlashFraction is the fraction of a node's stake burned on a failed
	// challenge. Env: LENS_POVI_SLASH_FRACTION in (0,1]. Default 0.5.
	POVISlashFraction float64

	// POVIChallengeKey is the base64 ed25519 PRIVATE key Lens signs challenges
	// with (env: LENS_POVI_CHALLENGE_KEY). If empty, Lens generates an
	// ephemeral key at startup — fine for a single-instance dev run, but set a
	// stable key in production so a Lens restart doesn't invalidate the pubkey
	// nodes have pinned (which would make honest nodes fail challenges).
	POVIChallengeKey string

	// TrustfulComputeMintEnabled gates the LEGACY trust-based compute mint
	// (ComputeMiner.RecordServedRequest mints LENS per served request with no
	// receipt, on a caller-asserted token count, with no idempotency). Env:
	// LENS_TRUSTFUL_COMPUTE_MINT_ENABLED. DEFAULT FALSE (U6 Sybil floor): an
	// unprotected mint path must be opt-IN, not on-by-accident — receipt-minting
	// (LENS_POVI_MINTING_ENABLED, now safe with PoVI Parts 1+2+3) is the intended
	// successor. Set true only to deliberately re-enable the blind trust-mint.
	TrustfulComputeMintEnabled bool

	// GuardrailsEnabled gates the Upgrade 13 OUTPUT guardrails (CheckOutput:
	// output PII, JSON/length/regex validation) + the per-workspace config
	// API for them. OFF by default: when false the INPUT guardrails behave
	// exactly as today and no output guardrails run. Even when on, output
	// block-actions are explicit opt-in (default flag/observe). Opt in via
	// LENS_GUARDRAILS_ENABLED=true.
	GuardrailsEnabled bool

	// WorkTierEnabled gates the WorkTier descriptive work classifier — a
	// post-serve, NON-CONTENT classification of each served request (size / cost
	// / complexity / sensitivity), persisted for the future routing Advisor +
	// analytics. CAPABILITY flag, default false: off = no classification, no
	// rows, zero behavior change. DESCRIPTIVE and mint-free, so it is NOT a
	// safety restriction and is deliberately NOT in the economy force-off block
	// (the inverse of the economy gates: off = no feature = safe). Env:
	// LENS_WORKTIER_ENABLED.
	WorkTierEnabled bool

	// NodeLatencyCaptureEnabled gates the DESCRIPTIVE proof-of-latency capture (P3 #6) — a post-serve,
	// off-path record of gateway-measured node serve latency into a per-(node,cohort) EWMA aggregate
	// (node_cohort_latency_stats), the substrate a LATER mint reads. CAPABILITY flag, default false: off =
	// no capture, no rows, zero behavior change. DESCRIPTIVE and mint-free (writes no ledger), so — like
	// WorkTier — it is deliberately NOT in the economy force-off block. Env: LENS_NODE_LATENCY_CAPTURE_ENABLED.
	NodeLatencyCaptureEnabled bool

	// NodeAttestationVerifyEnabled gates the Proof-of-Confidential-Compute VERIFY sweep (step b): the gateway
	// dials each node's /attestation, verifies the NVIDIA NRAS EAT (JWT sig + x5c chain to the pinned NVIDIA
	// root + claims), and records the verified hardware class to node_attestations. CAPABILITY flag, default
	// false: off = no sweep, no dials, no rows. Mint-free (records a class, credits nothing), so — like
	// NodeLatencyCapture — it is deliberately NOT in the economy force-off block. Also needs the pinned root
	// CA (LENS_NVIDIA_ROOT_CA_PEM) + the JWKS URL to be configured. Env: LENS_NODE_ATTESTATION_VERIFY_ENABLED.
	NodeAttestationVerifyEnabled bool

	// RoutingIntelligenceEnabled gates Upgrade 22 — feeding aggregated
	// pattern-mining intelligence back into model selection. OFF by default:
	// when false, routing behaves byte-for-byte as before. Even when on, it
	// only influences requests that explicitly opt into auto-routing
	// (model "auto" or X-Talyvor-Auto-Route) and only within the
	// workspace's allowed models. Opt in via
	// LENS_ROUTING_INTELLIGENCE_ENABLED=true.
	RoutingIntelligenceEnabled bool

	// RoutingTierCohortsEnabled refines RoutingIntelligence (Shape 3): when on, the Advisor
	// conditions its cohorts on the request's complexity tier (worktier.ComplexityBucketFor),
	// picking the best model PER work-type, with a tiered→non-tiered→default fallback. OFF by
	// default and in the kill-switch force-off block: when off, routing is byte-for-byte as
	// before (the complexity dimension is ignored). Only meaningful when RoutingIntelligence is
	// also on. Opt in via LENS_ROUTING_TIER_COHORTS_ENABLED=true.
	RoutingTierCohortsEnabled bool

	// NodeAutoRouteEnabled (blocker 6) gates gateway auto-routing of normal inference traffic to a
	// registered inference node (which then auto-signs + submits its own receipt). DEFAULT FALSE.
	// It is a ROUTING flag, NOT a mint gate — minting stays gated downstream by stake +
	// earn_verified — so it is deliberately NOT in the kill-switch force-off block. Off → the serve
	// path is byte-identical to today. Env: LENS_NODE_AUTOROUTE_ENABLED.
	NodeAutoRouteEnabled bool

	// ReputationBondedMintingEnabled (P1 #9) gates the reputation BOND on PoVI-receipt + pool-royalty
	// mints: a contributor's reputation R modulates the mint amount via f(R)∈[0,1] (never amplifies)
	// and gates to 0 below the access floor. DEFAULT FALSE. It can only REDUCE or BLOCK a mint the U6
	// floor/stake/rate-cap already allowed — never enable one — so it is deliberately NOT in the
	// kill-switch force-off block (a mint-reducer, not a mint-enabler; when the economy is off all
	// bonded mints are already force-off'd). Off → the mint path is byte-identical to today (no
	// reputation read, no emitter). Env: LENS_REPUTATION_BONDED_MINTING_ENABLED.
	ReputationBondedMintingEnabled bool

	// ProofOfBenchmarkEnabled (P1 #10, PR-A) gates the proof-of-benchmark probe SCHEDULER: a verifier
	// draws an unpredictable eval item from a private pool, sends only the input to a node, and scores
	// the answer against held ground truth into a per-node quality signal. DEFAULT FALSE. MEASUREMENT
	// only — the score feeds NO routing and NO mint, so it is NOT in the kill-switch force-off block.
	// Off → no probes drawn, no scheduler. Env: LENS_PROOF_OF_BENCHMARK_ENABLED.
	ProofOfBenchmarkEnabled bool

	// ProofOfImprovementEnabled (Proof-of-Improvement rail, piece 1) is a CAPABILITY flag for selecting
	// a non-cost reward anchor (e.g. the held-benchmark anchor) in a FUTURE eval-contribution mint.
	// DEFAULT FALSE. It can never outrun the U6 floor/stake/rate-cap (those gate the amount downstream),
	// so it is NOT in the kill-switch force-off block. This PR wires NO reachable selection — the cost
	// anchor stands unconditionally — so the flag is byte-identical on or off today. Env:
	// LENS_PROOF_OF_IMPROVEMENT_ENABLED.
	ProofOfImprovementEnabled bool

	// EvalContributionMintingEnabled (Proof-of-Improvement instance 1) is the dedicated EARNING flag for
	// the proof-of-eval-contribution mint. DEFAULT FALSE. Unlike ProofOfImprovementEnabled (the
	// capability that authorizes selecting the held-benchmark anchor), this gates the mint FIRING — so
	// it is a state-creation gate and IS in the kill-switch force-off block (mirrors POVIMintingEnabled).
	// The mint fires only when BOTH this AND ProofOfImprovementEnabled are on AND a positive rate is set.
	// Env: LENS_EVAL_CONTRIBUTION_MINTING_ENABLED.
	EvalContributionMintingEnabled bool

	// RoutingPredictionEnabled (Proof-of-Improvement piece 3, PR-1) is a CAPABILITY flag gating SUBMISSION
	// of routing predictions (the attributable "cohort C → model M" unit). DEFAULT FALSE — the
	// routing_predictions table stays provably empty until the capability is deliberately enabled. This is
	// NOT an earning flag (the proof-of-routing-prediction mint + its force-off flag land in PR-4): PR-1 is
	// an inert data substrate with NO mint, so this is NOT in the kill-switch force-off block. Env:
	// LENS_ROUTING_PREDICTION_ENABLED.
	RoutingPredictionEnabled bool

	// RoutingPredictionScoringEnabled (Proof-of-Improvement piece 3, PR-3a) gates the routing-prediction
	// SCORER sweeper (run model M + the baseline on a cohort's held eval slice → skill-above-baseline
	// score). DEFAULT FALSE. It is a CAPABILITY/MEASUREMENT flag (it produces a score, mints nothing), so
	// it is NOT in the kill-switch force-off block — like LENS_PROOF_OF_BENCHMARK_ENABLED. INERT in PR-3a
	// regardless: the scorer is wired with NO real Inferer, so even flag-on runs no inference until PR-3b
	// supplies one. Env: LENS_ROUTING_PREDICTION_SCORING_ENABLED.
	RoutingPredictionScoringEnabled bool

	// RoutingPredictionMintingEnabled (Proof-of-Improvement instance 2, PR-4) gates the proof-of-routing-
	// prediction mint FIRING (pays a contributor whose prediction was proven skill-above-baseline). Like
	// EvalContributionMintingEnabled it is a state-creation gate (mints LENS) and IS in the kill-switch
	// force-off block. The mint fires only when BOTH this AND ProofOfImprovementEnabled are on AND a positive
	// rate is set. Env: LENS_ROUTING_PREDICTION_MINTING_ENABLED.
	RoutingPredictionMintingEnabled bool

	// EvalContributionRatePerPoint is the LENS-per-discrimination-point rate for the proof-of-eval-
	// contribution mint (amount = rate × clamp01(discrimination)). DEFAULT 0 ⇒ INERT: NewHeldBenchmarkAnchor
	// refuses a non-positive rate, so the minter's anchor is nil and RunOnce is a total no-op even with
	// both flags on. The rate is a deliberate later flip, set only when the economy goes live. Env:
	// LENS_EVAL_CONTRIBUTION_RATE_PER_POINT.
	EvalContributionRatePerPoint float64

	// RoutingPredictionRatePerPoint is the LENS-per-skill-margin-point rate for the proof-of-routing-
	// prediction mint (amount = rate × clamp01(skill_margin)). DEFAULT 0 ⇒ INERT: NewHeldBenchmarkAnchor
	// refuses a non-positive rate, so the minter's anchor is nil and RunOnce is a total no-op even with both
	// flags on. A deliberate later flip, set only when the economy goes live. Env:
	// LENS_ROUTING_PREDICTION_RATE_PER_POINT.
	RoutingPredictionRatePerPoint float64

	// LatencyRatePerPoint is the LENS-per-latency-skill-point rate for the proof-of-latency-locality mint
	// (amount = rate × clamp01(latency_skill)). DEFAULT 0 ⇒ INERT: NewHeldBenchmarkAnchor refuses a
	// non-positive rate, so the minter's anchor is nil and RunOnce is a total no-op even with both flags on.
	// A deliberate later flip, set only when the economy goes live. Env: LENS_LATENCY_RATE_PER_POINT.
	LatencyRatePerPoint float64

	// GatewayPriceCeiling (F4-capstone step B) is the per-token price cap StrategyPriceAware clamps the
	// node-declared price to: effective_price = min(node_declared, ceiling). It bounds the ranking signal AND
	// (in step C) the agent's charge, so a node cannot set what an agent pays. DEFAULT 0.50 = 10× the 0.050
	// node default, so legitimate nodes are never clamped, only inflated declarations. A ROUTING bound, not a
	// mint gate — NOT in the economy force-off block. Env: LENS_GATEWAY_PRICE_CEILING.
	GatewayPriceCeiling float64

	// LatencyMintingEnabled (Proof-of-Improvement instance 3) gates the proof-of-latency-locality mint FIRING
	// (pays a NODE for cohort-relative, quality-gated fast service). Like the other two P-o-I mints it is a
	// state-creation gate (mints LENS) and IS in the kill-switch force-off block. Fires only when BOTH this
	// AND ProofOfImprovementEnabled are on AND a positive rate is set. Env: LENS_LATENCY_MINTING_ENABLED.
	LatencyMintingEnabled bool

	// ConfidentialRatePerPoint is the LENS-per-epoch FLAT rate for the proof-of-confidential-compute mint
	// (amount = rate × 1.0 per eligible (node,class,epoch)). DEFAULT 0 ⇒ INERT (the anchor refuses).
	// Env: LENS_CONFIDENTIAL_RATE_PER_POINT.
	ConfidentialRatePerPoint float64

	// ConfidentialMintingEnabled (Proof-of-Improvement instance 4) gates the proof-of-confidential-compute
	// mint FIRING (pays a NODE for VERIFIED confidential capacity). State-creation gate (mints LENS) and IS in
	// the kill-switch force-off block. Fires only when BOTH this AND ProofOfImprovementEnabled are on AND a
	// positive rate is set — and even then mints ZERO until a key_bound=true attestation exists (CC hardware).
	// Env: LENS_CONFIDENTIAL_MINTING_ENABLED.
	ConfidentialMintingEnabled bool

	// EconomyEnabled (U3) is the MASTER economy kill-switch. Env:
	// LENS_ECONOMY_ENABLED, default TRUE (explicit opt-out). When false, Load()
	// force-OFFs every economy state-creation gate below (regardless of its own
	// env value), and main.go does not register the economy route surface — the
	// deployment runs as pure fiat SaaS. NOT economy (untouched by this switch):
	// LENS_HA_ENABLED, LENS_GUARDRAILS_ENABLED.
	EconomyEnabled bool

	// BillingEnabled (U18b) gates the FIAT Stripe billing surface (checkout +
	// webhook + admin purchases). Env: LENS_BILLING_ENABLED, default FALSE. It is
	// FIAT, NOT economy — independent of EconomyEnabled (a pure fiat-SaaS
	// deployment runs EconomyEnabled=false + BillingEnabled=true). When false,
	// main.go does not register the billing routes (unregistered ⇒ 404).
	BillingEnabled bool

	// StripeSecretKey / StripeWebhookSecret are read ONLY here (the
	// NoDirectEnvReads invariant extends to them). SECRETS — never logged. If
	// BillingEnabled is true and either is empty, Load() fails startup.
	StripeSecretKey     string
	StripeWebhookSecret string

	// BillingSuccessURL / BillingCancelURL are the Stripe Checkout redirect URLs
	// (NOT secrets). Stripe requires both; defaulted, override per deployment.
	BillingSuccessURL string
	BillingCancelURL  string

	// ROIIncludeEngineerBreakdown gates the per-engineer (author) cost
	// section of the executive ROI report (Upgrade 24). OFF by default:
	// attributing cost to named people is SENSITIVE and easily misread as a
	// productivity/surveillance metric. It is a cost ATTRIBUTION, not a
	// performance judgment. Operators opt in explicitly via
	// LENS_ROI_INCLUDE_ENGINEER_BREAKDOWN=true.
	ROIIncludeEngineerBreakdown bool

	// TLS / HTTPS (Let's Encrypt autocert).
	//
	// TLSDomain is the public hostname Lens provisions a certificate for.
	// When set, the server binds on :443 (TLS) and :80 (HTTP→HTTPS redirect).
	// When empty, Lens runs plain HTTP on ListenAddr — safe for local dev or
	// when TLS is terminated upstream (e.g. nginx, a load balancer, Cloudflare).
	// Env: LENS_TLS_DOMAIN.
	//
	// TLSCacheDir is where autocert stores the Let's Encrypt certificate and
	// private key across restarts. The directory must be writable by the Lens
	// process. Defaults to /var/cache/lens-tls.
	// Env: LENS_TLS_CACHE_DIR.
	TLSDomain   string
	TLSCacheDir string

	// CORSAllowedOrigins is a comma-separated list of origins permitted to make
	// cross-origin requests to the Lens API. Env: LENS_CORS_ALLOWED_ORIGINS.
	// Default empty — CORS is disabled and browsers enforce the same-origin
	// policy, which is correct for server-to-server API gateway deployments.
	//
	// Use "*" to allow any origin (fully-public API mode). Use a comma-separated
	// list to restrict to known frontend deployments, e.g.:
	//   LENS_CORS_ALLOWED_ORIGINS=https://app.example.com,https://admin.example.com
	CORSAllowedOrigins string

	// RedisTLS controls TLS for the Redis connection.
	// Env: LENS_REDIS_TLS. Default false.
	//
	// Two ways to enable Redis TLS:
	//   1. Use rediss:// in LENS_REDIS_URL — go-redis detects the scheme
	//      automatically and enables TLS without any extra configuration.
	//   2. Set LENS_REDIS_TLS=true — Lens forces TLS on the connection even
	//      when the URL scheme is redis://. Useful when the URL is supplied
	//      by a secrets manager that always emits redis://.
	//
	// Lens logs a startup warning when neither is set, because Redis carries
	// semantic cache data (which may include LLM prompt fragments),
	// rate-limit counters, HA heartbeats, and circuit-breaker state.
	RedisTLS bool

	// RedisTLSSkipVerify disables Redis server certificate verification.
	// Env: LENS_REDIS_TLS_SKIP_VERIFY. Default false.
	// Only appropriate for self-signed / internal-CA certificates in
	// controlled environments. Has no effect when Redis TLS is not active.
	RedisTLSSkipVerify bool

	// NatsTLS controls TLS for the NATS connection.
	// Env: LENS_NATS_TLS. Default false.
	//
	// Two ways to enable NATS TLS:
	//   1. Use tls:// or nats+tls:// in LENS_NATS_URL — the nats client
	//      detects the scheme automatically and enables TLS without any extra
	//      configuration.
	//   2. Set LENS_NATS_TLS=true — Lens forces TLS on the connection even
	//      when the URL scheme is nats://. Useful when the URL is supplied
	//      by a secrets manager that always emits nats://.
	//
	// Lens logs a startup warning when neither is set, because NATS carries
	// alert events, session turn events, learner signals, and status updates —
	// all of which may contain prompt content or workspace metadata.
	NatsTLS bool

	// NatsTLSSkipVerify disables NATS server certificate verification.
	// Env: LENS_NATS_TLS_SKIP_VERIFY. Default false.
	// Only appropriate for self-signed / internal-CA certificates in
	// controlled environments. Has no effect when NATS TLS is not active.
	NatsTLSSkipVerify bool

	// NodeTLSSkipVerify disables TLS certificate verification when Lens
	// contacts registered nodes (inference, embedding, PoVI challenge).
	// Env: LENS_NODE_TLS_SKIP_VERIFY. Default false.
	// Required when nodes present self-signed certificates — the
	// recommended default for LAN deployments (see NODE_TLS_CERT /
	// CACHE_NODE_TLS_CERT / EMBED_NODE_TLS_CERT). Has no effect when
	// nodes serve plain HTTP.
	NodeTLSSkipVerify bool

	// DBSSLMode controls TLS for the Postgres connection.
	// Env: LENS_DB_SSL_MODE. Default "require".
	//
	// Valid values mirror the libpq sslmode parameter:
	//   disable     — no TLS; only appropriate for localhost / trusted private
	//                 networks.  Logs a startup warning when set.
	//   allow       — try plain TCP first, upgrade to TLS if the server
	//                 demands it.  Not recommended (MITM-capable).
	//   prefer      — try TLS first, fall back to plain TCP.  pgx default
	//                 when sslmode is omitted; not suitable for production.
	//   require     — always use TLS; skip server certificate verification.
	//                 Safe for most managed databases (RDS, Supabase, Neon,
	//                 etc.) that present TLS certs signed by a private CA.
	//   verify-ca   — require TLS and verify the server cert is signed by a
	//                 trusted CA (set sslrootcert in the DSN or via PGSSLROOTCERT).
	//   verify-full — require TLS, verify CA, and verify the server hostname.
	//                 Strongest option; requires a valid cert chain.
	DBSSLMode string

	// DBPgBouncer switches pgxpool to simple-query protocol (no prepared
	// statements), required when Lens connects through PgBouncer in
	// transaction pooling mode. Env: LENS_DB_PGBOUNCER. Default false.
	// The Helm chart sets this automatically when pgbouncer.enabled=true.
	DBPgBouncer bool

	// DBMaxConns caps the pgxpool at this many open server connections.
	// Env: LENS_DB_MAX_CONNS. Default 25 (direct Postgres); raise to ~100
	// when a PgBouncer in transaction mode sits in front because PgBouncer
	// multiplexes many cheap client connections onto a smaller pool of real
	// server connections.
	DBMaxConns int32

	// DBMinConns is the number of idle connections pgxpool keeps open so
	// the first burst of requests after a quiet period isn't bottlenecked
	// on connection establishment. Env: LENS_DB_MIN_CONNS. Default 2.
	DBMinConns int32

	// DBReplicaURL is an OPTIONAL read-replica DSN (U8/U9). When set, the
	// recon-confirmed analytics/display reads (forecast, ROI, dashboard
	// stats, admin read-only lists, rate/history) are routed to it; ALL
	// money/authz/transaction reads stay on the PRIMARY by construction —
	// the money/authz structs are never handed this handle. Empty (default)
	// → every read uses the primary pool, byte-identical to today. A
	// malformed URL or an unreachable replica at boot falls back to the
	// primary with a WARN: the replica is an optimization, never a boot
	// dependency. When LENS_DB_PGBOUNCER is set, the replica pool also uses
	// simple protocol. Env: LENS_DB_REPLICA_URL.
	DBReplicaURL string

	// ObsWriteMaxConcurrent caps how many post-serve observational writes
	// (attribution records + routing-pattern capture) may run at once.
	// Past the cap, writes are DROPPED (they are observational by contract)
	// rather than queued — unbounded, they convert overload into
	// pool-connection churn that can exhaust PgBouncer's max_client_conn
	// (#122; observed empirically 2026-06-11). Env:
	// LENS_OBS_WRITE_MAX_CONCURRENT. Default 32 (≈ a third of the
	// PgBouncer-mode pool). 0 disables the bound entirely.
	ObsWriteMaxConcurrent int

	// High Availability (Upgrade 7). HA is strictly opt-in via
	// LENS_HA_ENABLED; when false (the default) the process runs as a
	// single instance exactly as it did before HA existed. Enabling HA
	// requires Redis (LENS_REDIS_URL) — which Lens already requires — for
	// the instance registry, shared rate limiter, and breaker gossip.
	HAEnabled bool
	// HAHeartbeat is how often this instance refreshes its registry key.
	// HAInstanceTTL is the TTL on that key, so a crashed instance
	// disappears after a few missed heartbeats. HADrainTimeout bounds
	// graceful shutdown — in-flight requests get this long to finish.
	HAHeartbeat    time.Duration
	HAInstanceTTL  time.Duration
	HADrainTimeout time.Duration

	// DistillWorkerBin is the path to the compiled distill-worker binary that
	// distill.ProcessIsolator spawns to run document conversion in a killable,
	// memory-limited subprocess (the stage-3 resource-isolation envelope). Env:
	// LENS_DISTILL_WORKER_BIN. Defaults to the distill-worker binary sitting
	// beside the running lens executable, so the Docker image (which ships both
	// binaries in the same directory) works with no config. Nothing in the
	// serving path uses it yet — the request-path integration that constructs
	// the isolator with this path is a later PR.
	DistillWorkerBin string
}

func Load() (*Config, error) {
	c := &Config{
		ListenAddr:        getEnv("LENS_LISTEN_ADDR", "0.0.0.0:8080"),
		RedisURL:          os.Getenv("LENS_REDIS_URL"),
		DatabaseURL:       os.Getenv("LENS_DATABASE_URL"),
		DBReplicaURL:      os.Getenv("LENS_DB_REPLICA_URL"),
		NatsURL:           os.Getenv("LENS_NATS_URL"),
		OpenAIAPIKey:      os.Getenv("LENS_OPENAI_API_KEY"),
		AnthropicAPIKey:   os.Getenv("LENS_ANTHROPIC_API_KEY"),
		GoogleAPIKey:      os.Getenv("LENS_GOOGLE_API_KEY"),
		EmbeddingModel:    getEnv("LENS_EMBEDDING_MODEL", "text-embedding-3-small"),
		EmbeddingBaseURL:  os.Getenv("LENS_EMBEDDING_BASE_URL"),
		SemanticThreshold: 0.92,
		MaxCacheTTL:       24 * time.Hour,
		LogLevel:          getEnv("LENS_LOG_LEVEL", "info"),
		OllamaURL:         getEnv("LENS_OLLAMA_URL", "http://localhost:11434"),

		AWSRegion:          getEnv("LENS_AWS_REGION", "us-east-1"),
		AWSAccessKeyID:     os.Getenv("LENS_AWS_ACCESS_KEY_ID"),
		AWSSecretAccessKey: os.Getenv("LENS_AWS_SECRET_ACCESS_KEY"),
		AWSSessionToken:    os.Getenv("LENS_AWS_SESSION_TOKEN"),

		MistralAPIKey: os.Getenv("LENS_MISTRAL_API_KEY"),
		GroqAPIKey:    os.Getenv("LENS_GROQ_API_KEY"),
		VLLMBaseURL:   os.Getenv("LENS_VLLM_BASE_URL"),
		VLLMAPIKey:    os.Getenv("LENS_VLLM_API_KEY"),

		QualityAutoRetry: parseBoolEnv("LENS_QUALITY_AUTO_RETRY"),

		CacheSharingEnabled:    parseBoolEnv("LENS_CACHE_SHARING_ENABLED"),
		CachePoolableEnabled:   parseBoolEnv("LENS_CACHE_POOLABLE_ENABLED"),
		DistillPoolableEnabled: parseBoolEnv("LENS_DISTILL_POOLABLE_ENABLED"),
		PatternMiningEnabled:   parseBoolEnv("LENS_PATTERN_MINING_ENABLED"),
		POVIMintingEnabled:     parseBoolEnv("LENS_POVI_MINTING_ENABLED"),

		PoolRoyaltyMintingEnabled: parseBoolEnv("LENS_POOL_ROYALTY_MINTING_ENABLED"),
		LXCShadowSpendEnabled:     parseBoolEnv("LENS_LXC_SHADOW_SPEND_ENABLED"),
		LXCGatingEnabled:          parseBoolEnv("LENS_LXC_GATING_ENABLED"),
		PatternCaptureEnabled:     parseBoolEnv("LENS_PATTERN_CAPTURE_ENABLED"),
		PatternEarningEnabled:     parseBoolEnv("LENS_PATTERN_EARNING_ENABLED"),

		ROIIncludeEngineerBreakdown: parseBoolEnv("LENS_ROI_INCLUDE_ENGINEER_BREAKDOWN"),

		GuardrailsEnabled:            parseBoolEnv("LENS_GUARDRAILS_ENABLED"),
		WorkTierEnabled:              parseBoolEnv("LENS_WORKTIER_ENABLED"),
		NodeLatencyCaptureEnabled:    parseBoolEnv("LENS_NODE_LATENCY_CAPTURE_ENABLED"),
		NodeAttestationVerifyEnabled: parseBoolEnv("LENS_NODE_ATTESTATION_VERIFY_ENABLED"),

		RoutingIntelligenceEnabled:      parseBoolEnv("LENS_ROUTING_INTELLIGENCE_ENABLED"),
		RoutingTierCohortsEnabled:       parseBoolEnv("LENS_ROUTING_TIER_COHORTS_ENABLED"),
		NodeAutoRouteEnabled:            parseBoolEnv("LENS_NODE_AUTOROUTE_ENABLED"),
		ReputationBondedMintingEnabled:  parseBoolEnv("LENS_REPUTATION_BONDED_MINTING_ENABLED"),
		ProofOfBenchmarkEnabled:         parseBoolEnv("LENS_PROOF_OF_BENCHMARK_ENABLED"),
		ProofOfImprovementEnabled:       parseBoolEnv("LENS_PROOF_OF_IMPROVEMENT_ENABLED"),
		EvalContributionMintingEnabled:  parseBoolEnv("LENS_EVAL_CONTRIBUTION_MINTING_ENABLED"),
		RoutingPredictionMintingEnabled: parseBoolEnv("LENS_ROUTING_PREDICTION_MINTING_ENABLED"),
		LatencyMintingEnabled:           parseBoolEnv("LENS_LATENCY_MINTING_ENABLED"),
		ConfidentialMintingEnabled:      parseBoolEnv("LENS_CONFIDENTIAL_MINTING_ENABLED"),
		RoutingPredictionEnabled:        parseBoolEnv("LENS_ROUTING_PREDICTION_ENABLED"),
		RoutingPredictionScoringEnabled: parseBoolEnv("LENS_ROUTING_PREDICTION_SCORING_ENABLED"),

		BillingEnabled:      parseBoolEnv("LENS_BILLING_ENABLED"),
		StripeSecretKey:     os.Getenv("LENS_STRIPE_SECRET_KEY"),
		StripeWebhookSecret: os.Getenv("LENS_STRIPE_WEBHOOK_SECRET"),
		BillingSuccessURL:   getEnv("LENS_BILLING_SUCCESS_URL", "https://app.talyvor.com/billing/success?session_id={CHECKOUT_SESSION_ID}"),
		BillingCancelURL:    getEnv("LENS_BILLING_CANCEL_URL", "https://app.talyvor.com/billing/cancel"),

		LocalEndpoints: os.Getenv("LENS_LOCAL_ENDPOINTS"),

		TLSDomain:   os.Getenv("LENS_TLS_DOMAIN"),
		TLSCacheDir: getEnv("LENS_TLS_CACHE_DIR", "/var/cache/lens-tls"),

		CORSAllowedOrigins: os.Getenv("LENS_CORS_ALLOWED_ORIGINS"),

		RedisTLS:           parseBoolEnv("LENS_REDIS_TLS"),
		RedisTLSSkipVerify: parseBoolEnv("LENS_REDIS_TLS_SKIP_VERIFY"),

		NatsTLS:           parseBoolEnv("LENS_NATS_TLS"),
		NatsTLSSkipVerify: parseBoolEnv("LENS_NATS_TLS_SKIP_VERIFY"),

		NodeTLSSkipVerify: parseBoolEnv("LENS_NODE_TLS_SKIP_VERIFY"),

		JWTPrivateKey: os.Getenv("LENS_JWT_PRIVATE_KEY"),
		TokenTTL:      24 * time.Hour,

		HAEnabled: parseBoolEnv("LENS_HA_ENABLED"),

		DistillWorkerBin: getEnv("LENS_DISTILL_WORKER_BIN", defaultDistillWorkerBin()),

		DBSSLMode:   getEnv("LENS_DB_SSL_MODE", "require"),
		DBPgBouncer: parseBoolEnv("LENS_DB_PGBOUNCER"),
		DBMaxConns:  25,
		DBMinConns:  2,

		ObsWriteMaxConcurrent: 32,
	}

	// Pool size overrides.
	if v := os.Getenv("LENS_DB_MAX_CONNS"); v != "" {
		// ParseInt with bitSize 32 rejects values that don't fit int32, so the
		// int32(n) cast below can't silently overflow (a huge value errors here
		// instead of wrapping to a negative/garbage pool size).
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("invalid LENS_DB_MAX_CONNS (must be an integer in [1, %d]): %s", math.MaxInt32, v)
		}
		c.DBMaxConns = int32(n)
	}
	if v := os.Getenv("LENS_DB_MIN_CONNS"); v != "" {
		// bitSize 32 → the int32(n) cast can't overflow (see LENS_DB_MAX_CONNS).
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("invalid LENS_DB_MIN_CONNS (must be an integer in [0, %d]): %s", math.MaxInt32, v)
		}
		c.DBMinConns = int32(n)
	}
	if v := os.Getenv("LENS_OBS_WRITE_MAX_CONCURRENT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("invalid LENS_OBS_WRITE_MAX_CONCURRENT (must be ≥ 0; 0 disables the bound): %s", v)
		}
		c.ObsWriteMaxConcurrent = n
	}

	if err := ValidateDBSSLMode(c.DBSSLMode); err != nil {
		return nil, err
	}

	// U18b: billing moves fiat money — refuse to start half-configured. The error
	// names the env vars but NEVER echoes a key value.
	if c.BillingEnabled && (c.StripeSecretKey == "" || c.StripeWebhookSecret == "") {
		return nil, errors.New(
			"LENS_BILLING_ENABLED=true requires both LENS_STRIPE_SECRET_KEY and " +
				"LENS_STRIPE_WEBHOOK_SECRET (one or both are unset)")
	}

	// HA timers, expressed in whole seconds. Defaults match the
	// documented values; a non-positive override is a misconfiguration.
	c.HAHeartbeat = 5 * time.Second
	c.HAInstanceTTL = 15 * time.Second
	c.HADrainTimeout = 30 * time.Second
	for _, hf := range []struct {
		env string
		dst *time.Duration
	}{
		{"LENS_HA_HEARTBEAT_SEC", &c.HAHeartbeat},
		{"LENS_HA_INSTANCE_TTL_SEC", &c.HAInstanceTTL},
		{"LENS_HA_DRAIN_TIMEOUT_SEC", &c.HADrainTimeout},
	} {
		if v := os.Getenv(hf.env); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				return nil, fmt.Errorf("invalid %s (must be ≥ 1): %s", hf.env, v)
			}
			*hf.dst = time.Duration(n) * time.Second
		}
	}

	if v := os.Getenv("LENS_TOKEN_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("invalid LENS_TOKEN_TTL: %w", err)
		}
		c.TokenTTL = d
	}

	// Migration guard: LENS_JWT_SECRET (old HS256) is no longer accepted.
	// Operators must switch to an EC P-256 private key.
	if os.Getenv("LENS_JWT_SECRET") != "" {
		return nil, errors.New(
			"LENS_JWT_SECRET is no longer supported — Lens now uses ES256 (EC P-256). " +
				"Generate a key with:\n" +
				"  openssl ecparam -name prime256v1 -genkey -noout | \\\n" +
				"    openssl pkcs8 -topk8 -nocrypt -out ec-private.pem\n" +
				"  export LENS_JWT_PRIVATE_KEY=$(cat ec-private.pem)\n" +
				"Existing HS256 tokens will not be accepted after the upgrade. " +
				"Rotate all tokens on the first deployment with the new key",
		)
	}

	if v := os.Getenv("LENS_GLOBAL_RPM"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("invalid LENS_GLOBAL_RPM: %s", v)
		}
		c.GlobalRPM = n
	}
	if v := os.Getenv("LENS_GLOBAL_TPM"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("invalid LENS_GLOBAL_TPM: %s", v)
		}
		c.GlobalTPM = n
	}
	c.BurstMultiplier = 1.5
	if v := os.Getenv("LENS_BURST_MULTIPLIER"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 1.0 {
			return nil, fmt.Errorf("invalid LENS_BURST_MULTIPLIER (must be ≥ 1.0): %s", v)
		}
		c.BurstMultiplier = f
	}

	c.POVIMinStake = 100.0
	if v := os.Getenv("LENS_POVI_MIN_STAKE"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 {
			return nil, fmt.Errorf("invalid LENS_POVI_MIN_STAKE (must be ≥ 0): %s", v)
		}
		c.POVIMinStake = f
	}
	// EvalContributionRatePerPoint: LENS per discrimination-point. Default 0 ⇒ INERT (the held-benchmark
	// anchor refuses a non-positive rate → the eval-contribution minter is a total no-op). A deliberate
	// later flip.
	c.EvalContributionRatePerPoint = 0
	if v := os.Getenv("LENS_EVAL_CONTRIBUTION_RATE_PER_POINT"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 {
			return nil, fmt.Errorf("invalid LENS_EVAL_CONTRIBUTION_RATE_PER_POINT (must be ≥ 0): %s", v)
		}
		c.EvalContributionRatePerPoint = f
	}

	// RoutingPredictionRatePerPoint — DEFAULT 0 (INERT: the held-benchmark anchor refuses a non-positive
	// rate → the routing-prediction minter is a total no-op). A deliberate later flip.
	c.RoutingPredictionRatePerPoint = 0
	if v := os.Getenv("LENS_ROUTING_PREDICTION_RATE_PER_POINT"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 {
			return nil, fmt.Errorf("invalid LENS_ROUTING_PREDICTION_RATE_PER_POINT (must be ≥ 0): %s", v)
		}
		c.RoutingPredictionRatePerPoint = f
	}

	// LatencyRatePerPoint — DEFAULT 0 (INERT: the held-benchmark anchor refuses a non-positive rate → the
	// latency minter is a total no-op). A deliberate later flip, set only when the economy goes live.
	c.LatencyRatePerPoint = 0
	if v := os.Getenv("LENS_LATENCY_RATE_PER_POINT"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 {
			return nil, fmt.Errorf("invalid LENS_LATENCY_RATE_PER_POINT (must be ≥ 0): %s", v)
		}
		c.LatencyRatePerPoint = f
	}

	// GatewayPriceCeiling — DEFAULT 0.50 (10× the 0.050 node band; legitimate nodes never clamped). A routing
	// bound used only by StrategyPriceAware; must be > 0. Env: LENS_GATEWAY_PRICE_CEILING.
	c.GatewayPriceCeiling = 0.50
	if v := os.Getenv("LENS_GATEWAY_PRICE_CEILING"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f <= 0 {
			return nil, fmt.Errorf("invalid LENS_GATEWAY_PRICE_CEILING (must be > 0): %s", v)
		}
		c.GatewayPriceCeiling = f
	}

	// ConfidentialRatePerPoint — DEFAULT 0 (INERT: the anchor refuses a non-positive rate → the confidential
	// minter is a total no-op). A deliberate later flip, set only at CC-hardware go-live.
	c.ConfidentialRatePerPoint = 0
	if v := os.Getenv("LENS_CONFIDENTIAL_RATE_PER_POINT"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 {
			return nil, fmt.Errorf("invalid LENS_CONFIDENTIAL_RATE_PER_POINT (must be ≥ 0): %s", v)
		}
		c.ConfidentialRatePerPoint = f
	}

	c.POVIUnbondPeriod = 7 * 24 * time.Hour
	if v := os.Getenv("LENS_POVI_UNBOND_PERIOD"); v != "" {
		d, err := time.ParseDuration(v) // Go duration units (h/m/s); e.g. 168h = 7 days
		if err != nil || d < 0 {
			return nil, fmt.Errorf("invalid LENS_POVI_UNBOND_PERIOD (Go duration, e.g. 168h): %s", v)
		}
		c.POVIUnbondPeriod = d
	}
	c.POVIChallengeRate = 0.1
	if v := os.Getenv("LENS_POVI_CHALLENGE_RATE"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 || f > 1 {
			return nil, fmt.Errorf("invalid LENS_POVI_CHALLENGE_RATE (must be in [0,1]): %s", v)
		}
		c.POVIChallengeRate = f
	}
	c.PoolMintCapPerPair = 0
	if v := os.Getenv("LENS_POOL_MINT_CAP_PER_PAIR"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("invalid LENS_POOL_MINT_CAP_PER_PAIR (must be ≥ 0; 0 disables): %s", v)
		}
		c.PoolMintCapPerPair = n
	}
	c.PoolMintCapPerEntry = 0
	if v := os.Getenv("LENS_POOL_MINT_CAP_PER_ENTRY"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("invalid LENS_POOL_MINT_CAP_PER_ENTRY (must be >= 0; 0 disables): %s", v)
		}
		c.PoolMintCapPerEntry = n
	}
	c.PoolMintCapWindow = 24 * time.Hour
	if v := os.Getenv("LENS_POOL_MINT_CAP_WINDOW"); v != "" {
		d, err := time.ParseDuration(v) // Go duration units, e.g. 48h
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("invalid LENS_POOL_MINT_CAP_WINDOW (Go duration > 0, e.g. 48h): %s", v)
		}
		c.PoolMintCapWindow = d
	}
	// PR1 distill mint caps (separate budget from the cache caps; default 0/0 off + 24h).
	c.DistillMintCapPerPair = 0
	if v := os.Getenv("LENS_DISTILL_MINT_CAP_PER_PAIR"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("invalid LENS_DISTILL_MINT_CAP_PER_PAIR (must be >= 0; 0 disables): %s", v)
		}
		c.DistillMintCapPerPair = n
	}
	c.DistillMintCapPerContent = 0
	if v := os.Getenv("LENS_DISTILL_MINT_CAP_PER_CONTENT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("invalid LENS_DISTILL_MINT_CAP_PER_CONTENT (must be >= 0; 0 disables): %s", v)
		}
		c.DistillMintCapPerContent = n
	}
	c.DistillMintCapWindow = 24 * time.Hour
	if v := os.Getenv("LENS_DISTILL_MINT_CAP_WINDOW"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("invalid LENS_DISTILL_MINT_CAP_WINDOW (Go duration > 0, e.g. 48h): %s", v)
		}
		c.DistillMintCapWindow = d
	}
	// S2 routing-pattern earn cap — REAL default (50000), not 0 (see the field
	// comment); 0 = explicit ops-disable.
	c.PatternEarnCapPerWorkspace = 50000
	if v := os.Getenv("LENS_PATTERN_EARN_CAP_PER_WORKSPACE"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("invalid LENS_PATTERN_EARN_CAP_PER_WORKSPACE (must be ≥ 0; 0 disables): %s", v)
		}
		c.PatternEarnCapPerWorkspace = n
	}
	c.PatternEarnCapWindow = 24 * time.Hour
	if v := os.Getenv("LENS_PATTERN_EARN_CAP_WINDOW"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("invalid LENS_PATTERN_EARN_CAP_WINDOW (Go duration > 0, e.g. 48h): %s", v)
		}
		c.PatternEarnCapWindow = d
	}
	// U6 PR2 per-identity mint rate cap — REAL default (1000 LENS/24h), not 0
	// (default-ON-with-a-ceiling; inaction stays safe). 0 = explicit ops-disable;
	// negative is rejected. A safety restriction, NOT economy-killswitched.
	c.MintRateCapLENS24h = 1000
	if v := os.Getenv("LENS_MINT_RATE_CAP_LENS_24H"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 {
			return nil, fmt.Errorf("invalid LENS_MINT_RATE_CAP_LENS_24H (must be ≥ 0; 0 disables): %s", v)
		}
		c.MintRateCapLENS24h = f
	}
	c.PoolHoldbackWindow = 72 * time.Hour
	if v := os.Getenv("LENS_POOL_HOLDBACK_WINDOW"); v != "" {
		d, err := time.ParseDuration(v) // Go duration units, e.g. 72h
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("invalid LENS_POOL_HOLDBACK_WINDOW (Go duration > 0, e.g. 72h): %s", v)
		}
		c.PoolHoldbackWindow = d
	}
	// Detector thresholds (Stage 2.3b) — defaults, then env overrides with
	// range validation (the NaN/negative lesson).
	c.DetectVolumeMinMints = 50
	if v := os.Getenv("LENS_DETECT_VOLUME_MIN_MINTS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("invalid LENS_DETECT_VOLUME_MIN_MINTS (must be >= 0): %s", v)
		}
		c.DetectVolumeMinMints = n
	}
	c.DetectVolumeMaxRequesters = 2
	if v := os.Getenv("LENS_DETECT_VOLUME_MAX_REQUESTERS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("invalid LENS_DETECT_VOLUME_MAX_REQUESTERS (must be >= 0): %s", v)
		}
		c.DetectVolumeMaxRequesters = n
	}
	c.DetectBilateralMinFrac = 0.9
	if v := os.Getenv("LENS_DETECT_BILATERAL_MIN_FRAC"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || math.IsNaN(f) || f < 0 || f > 1 {
			return nil, fmt.Errorf("invalid LENS_DETECT_BILATERAL_MIN_FRAC (must be in [0,1]): %s", v)
		}
		c.DetectBilateralMinFrac = f
	}
	c.DetectBilateralMinMints = 20
	if v := os.Getenv("LENS_DETECT_BILATERAL_MIN_MINTS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("invalid LENS_DETECT_BILATERAL_MIN_MINTS (must be >= 0): %s", v)
		}
		c.DetectBilateralMinMints = n
	}
	c.DetectSimilarityMinSample = 30
	if v := os.Getenv("LENS_DETECT_SIMILARITY_MIN_SAMPLE"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("invalid LENS_DETECT_SIMILARITY_MIN_SAMPLE (must be >= 1): %s", v)
		}
		c.DetectSimilarityMinSample = n
	}
	c.DetectSimilarityMaxStddev = 0.02
	if v := os.Getenv("LENS_DETECT_SIMILARITY_MAX_STDDEV"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || math.IsNaN(f) || f < 0 {
			return nil, fmt.Errorf("invalid LENS_DETECT_SIMILARITY_MAX_STDDEV (must be >= 0): %s", v)
		}
		c.DetectSimilarityMaxStddev = f
	}

	// Detector sweep — DEFAULT-TRUE (the smoke detector accompanies minting automatically);
	// runs iff EconomyEnabled too (wired in main.go). The flag is a manual off-switch only.
	c.DetectorSweepEnabled = true
	if os.Getenv("LENS_DETECTOR_SWEEP_ENABLED") != "" {
		c.DetectorSweepEnabled = parseBoolEnv("LENS_DETECTOR_SWEEP_ENABLED")
	}

	// Keel (U25) cross-tenant drift attribution — DEFAULT-OFF; thresholds are placeholders.
	c.KeelEnabled = parseBoolEnv("LENS_KEEL_ENABLED") // default false
	c.KeelDeviationSigma = 3.0
	if v := os.Getenv("LENS_KEEL_DEVIATION_SIGMA"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			c.KeelDeviationSigma = f
		}
	}
	c.KeelWindowSeconds = 3600
	if v := os.Getenv("LENS_KEEL_WINDOW_SECONDS"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			c.KeelWindowSeconds = n
		}
	}
	c.KeelInterval = time.Hour
	c.KeelLookback = 48 * time.Hour

	// LXC agent allocation — DEFAULT-TRUE (owner's call; the capstone ships armed). Off removes a spend
	// guard, so the safe default is on. The flag is a manual off-switch (falls back to plain SpendLXC).
	c.LXCAgentAllocationEnabled = true
	if os.Getenv("LENS_LXC_AGENT_ALLOCATION_ENABLED") != "" {
		c.LXCAgentAllocationEnabled = parseBoolEnv("LENS_LXC_AGENT_ALLOCATION_ENABLED")
	}
	c.DetectorSweepInterval = time.Hour
	if v := os.Getenv("LENS_DETECTOR_SWEEP_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("invalid LENS_DETECTOR_SWEEP_INTERVAL (Go duration > 0, e.g. 1h): %s", v)
		}
		c.DetectorSweepInterval = d
	}
	c.DetectorSweepWindow = 24 * time.Hour
	if v := os.Getenv("LENS_DETECTOR_SWEEP_WINDOW"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("invalid LENS_DETECTOR_SWEEP_WINDOW (Go duration > 0, e.g. 24h): %s", v)
		}
		c.DetectorSweepWindow = d
	}
	c.PoolRoyaltyShare = 0.5
	if v := os.Getenv("LENS_POOL_ROYALTY_SHARE"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		// math.IsNaN is load-bearing: ParseFloat("NaN") succeeds and NaN
		// compares false to every bound, so the range checks alone would
		// let a balance-poisoning NaN share through. (±Inf is caught by
		// the range checks.)
		if err != nil || math.IsNaN(f) || f < 0 || f > 1 {
			return nil, fmt.Errorf("invalid LENS_POOL_ROYALTY_SHARE (must be in [0,1]): %s", v)
		}
		c.PoolRoyaltyShare = f
	}
	c.POVISlashFraction = 0.5
	if v := os.Getenv("LENS_POVI_SLASH_FRACTION"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f <= 0 || f > 1 {
			return nil, fmt.Errorf("invalid LENS_POVI_SLASH_FRACTION (must be in (0,1]): %s", v)
		}
		c.POVISlashFraction = f
	}
	c.POVIChallengeKey = os.Getenv("LENS_POVI_CHALLENGE_KEY")
	// U6 Sybil floor: the legacy trust-mint defaults FALSE — an unprotected mint
	// path (no receipt, caller-asserted tokens, no idempotency) must be opt-IN.
	c.TrustfulComputeMintEnabled = false
	if v := os.Getenv("LENS_TRUSTFUL_COMPUTE_MINT_ENABLED"); v != "" {
		c.TrustfulComputeMintEnabled = parseBoolEnv("LENS_TRUSTFUL_COMPUTE_MINT_ENABLED")
	}

	if v := os.Getenv("LENS_RETRY_MAX_ATTEMPTS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("invalid LENS_RETRY_MAX_ATTEMPTS (must be ≥ 1): %s", v)
		}
		c.RetryMaxAttempts = n
	}
	if v := os.Getenv("LENS_RETRY_INITIAL_DELAY"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("invalid LENS_RETRY_INITIAL_DELAY: %w", err)
		}
		c.RetryInitialDelay = d
	}
	if v := os.Getenv("LENS_RETRY_MAX_DELAY"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("invalid LENS_RETRY_MAX_DELAY: %w", err)
		}
		c.RetryMaxDelay = d
	}
	if v := os.Getenv("LENS_CB_THRESHOLD"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("invalid LENS_CB_THRESHOLD: %s", v)
		}
		c.CBThreshold = n
	}
	if v := os.Getenv("LENS_CB_RESET_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("invalid LENS_CB_RESET_TIMEOUT: %w", err)
		}
		c.CBResetTimeout = d
	}

	if v := os.Getenv("LENS_SEMANTIC_THRESHOLD"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid LENS_SEMANTIC_THRESHOLD: %w", err)
		}
		c.SemanticThreshold = f
	}

	if v := os.Getenv("LENS_MAX_CACHE_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("invalid LENS_MAX_CACHE_TTL: %w", err)
		}
		c.MaxCacheTTL = d
	}

	// Semantic-cache retention sweeper. Retention <= 0 disables the sweeper
	// (rows kept indefinitely); the sweep interval must be > 0 because a
	// time.Ticker panics on a non-positive duration.
	c.SemanticCacheRetention = 21 * 24 * time.Hour
	if v := os.Getenv("LENS_SEMANTIC_CACHE_RETENTION"); v != "" {
		d, err := time.ParseDuration(v) // Go duration units (h/m/s); e.g. 504h = 21 days
		if err != nil {
			return nil, fmt.Errorf("invalid LENS_SEMANTIC_CACHE_RETENTION (Go duration, e.g. 504h for 21 days): %w", err)
		}
		c.SemanticCacheRetention = d
	}
	c.SemanticCacheSweepInterval = time.Hour

	// U14 audit retention — token_events sweeper. <= 0 / unset disables (default).
	if v := os.Getenv("LENS_AUDIT_RETENTION"); v != "" {
		d, err := time.ParseDuration(v) // Go duration units (h/m/s); e.g. 8760h ≈ 1 year
		if err != nil {
			return nil, fmt.Errorf("invalid LENS_AUDIT_RETENTION (Go duration, e.g. 8760h for ~1 year): %w", err)
		}
		c.AuditRetention = d
	}
	// U14 off-box export loop — empty URL disables (default). Interval default 1h.
	c.AuditExportURL = os.Getenv("LENS_AUDIT_EXPORT_URL")
	c.AuditExportInterval = time.Hour
	if v := os.Getenv("LENS_AUDIT_EXPORT_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("invalid LENS_AUDIT_EXPORT_INTERVAL (Go duration): %w", err)
		}
		if d <= 0 {
			return nil, fmt.Errorf("LENS_AUDIT_EXPORT_INTERVAL must be > 0 (a time.Ticker panics on non-positive): %s", v)
		}
		c.AuditExportInterval = d
	}
	// U14 #187 export-before-prune gate. Default false = age-only (today's behaviour);
	// on = prune only at/below the export watermark, skip the sweep if export is off.
	c.AuditRequireExportBeforePrune = parseBoolEnv("LENS_AUDIT_REQUIRE_EXPORT_BEFORE_PRUNE")
	// U7b workspace-config reload interval — default 30s, MUST be > 0.
	c.WorkspaceReloadInterval = 30 * time.Second
	if v := os.Getenv("LENS_WORKSPACE_RELOAD_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("invalid LENS_WORKSPACE_RELOAD_INTERVAL (Go duration, e.g. 30s): %w", err)
		}
		if d <= 0 {
			return nil, fmt.Errorf("LENS_WORKSPACE_RELOAD_INTERVAL must be > 0 (a time.Ticker panics on non-positive): %s", v)
		}
		c.WorkspaceReloadInterval = d
	}
	// Workspace fail-closed staleness bound — default 3× the reload interval, MUST
	// exceed it (a bound <= the interval would fail closed during normal operation).
	c.WorkspaceMaxStaleness = 3 * c.WorkspaceReloadInterval
	if v := os.Getenv("LENS_WORKSPACE_MAX_STALENESS"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("invalid LENS_WORKSPACE_MAX_STALENESS (Go duration, e.g. 90s): %w", err)
		}
		c.WorkspaceMaxStaleness = d
	}
	if c.WorkspaceMaxStaleness <= c.WorkspaceReloadInterval {
		return nil, fmt.Errorf("LENS_WORKSPACE_MAX_STALENESS (%s) must be > LENS_WORKSPACE_RELOAD_INTERVAL (%s): a bound <= the reload interval trips during normal operation", c.WorkspaceMaxStaleness, c.WorkspaceReloadInterval)
	}
	// #189 guardrails-policy reload interval — default 30s, MUST be > 0.
	c.GuardrailsReloadInterval = 30 * time.Second
	if v := os.Getenv("LENS_GUARDRAILS_RELOAD_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("invalid LENS_GUARDRAILS_RELOAD_INTERVAL (Go duration, e.g. 30s): %w", err)
		}
		if d <= 0 {
			return nil, fmt.Errorf("LENS_GUARDRAILS_RELOAD_INTERVAL must be > 0 (a time.Ticker panics on non-positive): %s", v)
		}
		c.GuardrailsReloadInterval = d
	}
	if v := os.Getenv("LENS_SEMANTIC_CACHE_SWEEP_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("invalid LENS_SEMANTIC_CACHE_SWEEP_INTERVAL (Go duration, e.g. 1h): %w", err)
		}
		if d <= 0 {
			return nil, fmt.Errorf("invalid LENS_SEMANTIC_CACHE_SWEEP_INTERVAL (must be > 0): %s", v)
		}
		c.SemanticCacheSweepInterval = d
	}

	var missing []string
	if c.RedisURL == "" {
		missing = append(missing, "LENS_REDIS_URL")
	}
	if c.DatabaseURL == "" {
		missing = append(missing, "LENS_DATABASE_URL")
	}
	if c.NatsURL == "" {
		missing = append(missing, "LENS_NATS_URL")
	}
	if c.OpenAIAPIKey == "" {
		missing = append(missing, "LENS_OPENAI_API_KEY")
	}
	if c.AnthropicAPIKey == "" {
		missing = append(missing, "LENS_ANTHROPIC_API_KEY")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%w: %v", ErrMissingEnv, missing)
	}

	// U3 master economy kill-switch. Default TRUE (explicit opt-out, mirroring the
	// TrustfulCompute default-true-then-override pattern above). When false, force
	// OFF every economy state-creation gate — regardless of its individual env —
	// so no mint/earn/pool/LXC path can fire. The route SURFACE is unregistered
	// separately in main.go (registerEconomyRoutes). Adding a NEW economy gate?
	// add it to this block AND to the manifest in cmd/lens/economy_killswitch_test.go.
	c.EconomyEnabled = true
	if os.Getenv("LENS_ECONOMY_ENABLED") != "" {
		c.EconomyEnabled = parseBoolEnv("LENS_ECONOMY_ENABLED")
	}
	if !c.EconomyEnabled {
		c.PatternMiningEnabled = false
		c.PatternCaptureEnabled = false
		c.PatternEarningEnabled = false
		c.PoolRoyaltyMintingEnabled = false
		c.POVIMintingEnabled = false
		c.TrustfulComputeMintEnabled = false // U6: now defaults false; still force-off'd if an operator opted in
		c.CacheSharingEnabled = false
		c.CachePoolableEnabled = false
		c.DistillPoolableEnabled = false
		c.RoutingIntelligenceEnabled = false
		c.RoutingTierCohortsEnabled = false
		c.EvalContributionMintingEnabled = false  // P-o-I instance 1: the eval-contribution EARNING gate (mints LENS) — force-off with the economy
		c.RoutingPredictionMintingEnabled = false // P-o-I instance 2: the routing-prediction EARNING gate (mints LENS) — force-off with the economy
		c.LatencyMintingEnabled = false           // P-o-I instance 3: the latency-locality EARNING gate (mints LENS) — force-off with the economy
		c.ConfidentialMintingEnabled = false      // P-o-I instance 4: the confidential-compute EARNING gate (mints LENS) — force-off with the economy
		// U18: LXCGatingEnabled / LXCShadowSpendEnabled are DELIBERATELY NOT
		// forced off here — LXC is the fiat-pegged usage credit, not token
		// economy. They keep their own env values so a fiat-SaaS deployment can
		// still meter + gate paid LXC credit with the economy off. (Both still
		// default false, so default enterprise-mode behavior is unchanged.) The
		// LENS→LXC conversion route stays economy-gated in main.go.
	}

	return c, nil
}

var ErrMissingEnv = errors.New("missing required environment variables")

// ValidateDBSSLMode returns an error if mode is not one of the six values
// accepted by libpq / pgx for the sslmode parameter.  It is exported so that
// code paths that bypass Load (e.g. the migrate sub-command) can apply the
// same validation and produce the same clear error message.
func ValidateDBSSLMode(mode string) error {
	switch mode {
	case "disable", "allow", "prefer", "require", "verify-ca", "verify-full":
		return nil
	default:
		return fmt.Errorf(
			"invalid LENS_DB_SSL_MODE %q: must be one of disable, allow, prefer, require, verify-ca, verify-full",
			mode,
		)
	}
}

// defaultDistillWorkerBin resolves the distill-worker path to a binary named
// "distill-worker" sitting beside the running executable (e.g. /distill-worker
// next to /lens in the Docker image). Falls back to a bare "distill-worker"
// (PATH lookup) if the executable path can't be determined.
func defaultDistillWorkerBin() string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "distill-worker")
	}
	return "distill-worker"
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// parseBoolEnv recognises the common "true" forms (1, true, yes,
// on) case-insensitively. Anything else (including empty) is
// false so the feature stays opt-in by default.
func parseBoolEnv(key string) bool {
	v := os.Getenv(key)
	if v == "" {
		return false
	}
	switch v {
	case "1", "true", "TRUE", "True", "yes", "YES", "Yes", "on", "ON", "On":
		return true
	}
	return false
}

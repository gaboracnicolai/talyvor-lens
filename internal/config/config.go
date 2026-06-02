package config

import (
	"errors"
	"fmt"
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
	SemanticThreshold float64
	MaxCacheTTL       time.Duration
	LogLevel          string
	OllamaURL         string

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

	// JWT auth (Item 7). Secret must be ≥32 bytes; empty string
	// disables JWT minting/validation entirely (the auth manager
	// will reject any /v1/auth/token call but still accept API
	// key authentication).
	JWTSecret string
	TokenTTL  time.Duration

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
	// receipt). Env: LENS_TRUSTFUL_COMPUTE_MINT_ENABLED. DEFAULT TRUE
	// (preserves current behavior). This is the RETIREMENT SWITCH: once
	// receipt-minting is enabled (LENS_POVI_MINTING_ENABLED=true, now safe with
	// Parts 1+2+3), set this false to retire the blind trust-mint. Operator
	// decision — do NOT flip the default here.
	TrustfulComputeMintEnabled bool

	// GuardrailsEnabled gates the Upgrade 13 OUTPUT guardrails (CheckOutput:
	// output PII, JSON/length/regex validation) + the per-workspace config
	// API for them. OFF by default: when false the INPUT guardrails behave
	// exactly as today and no output guardrails run. Even when on, output
	// block-actions are explicit opt-in (default flag/observe). Opt in via
	// LENS_GUARDRAILS_ENABLED=true.
	GuardrailsEnabled bool

	// RoutingIntelligenceEnabled gates Upgrade 22 — feeding aggregated
	// pattern-mining intelligence back into model selection. OFF by default:
	// when false, routing behaves byte-for-byte as before. Even when on, it
	// only influences requests that explicitly opt into auto-routing
	// (model "auto" or X-Talyvor-Auto-Route) and only within the
	// workspace's allowed models. Opt in via
	// LENS_ROUTING_INTELLIGENCE_ENABLED=true.
	RoutingIntelligenceEnabled bool

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
		NatsURL:           os.Getenv("LENS_NATS_URL"),
		OpenAIAPIKey:      os.Getenv("LENS_OPENAI_API_KEY"),
		AnthropicAPIKey:   os.Getenv("LENS_ANTHROPIC_API_KEY"),
		GoogleAPIKey:      os.Getenv("LENS_GOOGLE_API_KEY"),
		EmbeddingModel:    getEnv("LENS_EMBEDDING_MODEL", "text-embedding-3-small"),
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

		CacheSharingEnabled:  parseBoolEnv("LENS_CACHE_SHARING_ENABLED"),
		PatternMiningEnabled: parseBoolEnv("LENS_PATTERN_MINING_ENABLED"),
		POVIMintingEnabled:   parseBoolEnv("LENS_POVI_MINTING_ENABLED"),

		ROIIncludeEngineerBreakdown: parseBoolEnv("LENS_ROI_INCLUDE_ENGINEER_BREAKDOWN"),

		GuardrailsEnabled: parseBoolEnv("LENS_GUARDRAILS_ENABLED"),

		RoutingIntelligenceEnabled: parseBoolEnv("LENS_ROUTING_INTELLIGENCE_ENABLED"),

		LocalEndpoints: os.Getenv("LENS_LOCAL_ENDPOINTS"),

		TLSDomain:   os.Getenv("LENS_TLS_DOMAIN"),
		TLSCacheDir: getEnv("LENS_TLS_CACHE_DIR", "/var/cache/lens-tls"),

		JWTSecret: os.Getenv("LENS_JWT_SECRET"),
		TokenTTL:  24 * time.Hour,

		HAEnabled: parseBoolEnv("LENS_HA_ENABLED"),

		DistillWorkerBin: getEnv("LENS_DISTILL_WORKER_BIN", defaultDistillWorkerBin()),

		DBSSLMode:   getEnv("LENS_DB_SSL_MODE", "require"),
		DBPgBouncer: parseBoolEnv("LENS_DB_PGBOUNCER"),
		DBMaxConns:  25,
		DBMinConns:  2,
	}

	// Pool size overrides.
	if v := os.Getenv("LENS_DB_MAX_CONNS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("invalid LENS_DB_MAX_CONNS (must be ≥ 1): %s", v)
		}
		c.DBMaxConns = int32(n)
	}
	if v := os.Getenv("LENS_DB_MIN_CONNS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("invalid LENS_DB_MIN_CONNS (must be ≥ 0): %s", v)
		}
		c.DBMinConns = int32(n)
	}

	if err := ValidateDBSSLMode(c.DBSSLMode); err != nil {
		return nil, err
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

	// Enforce minimum entropy for HS256 signing. Empty secret
	// is allowed — it just disables the JWT mint path; only a
	// *too-short* secret is a misconfiguration.
	if c.JWTSecret != "" && len(c.JWTSecret) < 32 {
		return nil, fmt.Errorf("LENS_JWT_SECRET must be at least 32 bytes (got %d)", len(c.JWTSecret))
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
	c.POVISlashFraction = 0.5
	if v := os.Getenv("LENS_POVI_SLASH_FRACTION"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f <= 0 || f > 1 {
			return nil, fmt.Errorf("invalid LENS_POVI_SLASH_FRACTION (must be in (0,1]): %s", v)
		}
		c.POVISlashFraction = f
	}
	c.POVIChallengeKey = os.Getenv("LENS_POVI_CHALLENGE_KEY")
	// Retirement switch defaults TRUE (preserve current trust-mint behavior).
	c.TrustfulComputeMintEnabled = true
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

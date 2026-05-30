package config

import (
	"errors"
	"fmt"
	"os"
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

		ROIIncludeEngineerBreakdown: parseBoolEnv("LENS_ROI_INCLUDE_ENGINEER_BREAKDOWN"),

		RoutingIntelligenceEnabled: parseBoolEnv("LENS_ROUTING_INTELLIGENCE_ENABLED"),

		LocalEndpoints: os.Getenv("LENS_LOCAL_ENDPOINTS"),

		JWTSecret: os.Getenv("LENS_JWT_SECRET"),
		TokenTTL:  24 * time.Hour,

		HAEnabled: parseBoolEnv("LENS_HA_ENABLED"),
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

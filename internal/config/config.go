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
	GlobalRPM        int
	GlobalTPM        int
	BurstMultiplier  float64
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

		LocalEndpoints: os.Getenv("LENS_LOCAL_ENDPOINTS"),

		JWTSecret: os.Getenv("LENS_JWT_SECRET"),
		TokenTTL:  24 * time.Hour,
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

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

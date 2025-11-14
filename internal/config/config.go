// Package config provides configuration management for the music bot
package config

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// Config holds all configuration for the music bot
type Config struct {
	TelegramToken          string
	Debug                  bool
	MaxConcurrentFetches   int
	MaxConcurrentMessages  int
	RequestTimeout         time.Duration
	RetryAttempts          int
	RetryMinDelay          time.Duration
	RetryMaxDelay          time.Duration
	CacheTTL               time.Duration
	NegativeCacheTTL       time.Duration // Cache failed searches separately
	FuzzyMatchThreshold    float64       // Levenshtein distance threshold (0-1)
	CircuitBreakerMaxFails int           // Open circuit after N failures
	CircuitBreakerTimeout  time.Duration // Circuit breaker timeout
	TextMirrorURL          string        // Text mirror service (r.jina.ai)
}

// Load creates a new Config from environment variables with sensible defaults
func Load() (*Config, error) {
	telegramToken := os.Getenv("TELEGRAM_BOT_TOKEN")
	if telegramToken == "" {
		return nil, fmt.Errorf("TELEGRAM_BOT_TOKEN environment variable is required")
	}

	cfg := &Config{
		TelegramToken:          telegramToken,
		Debug:                  envBool("BOT_DEBUG", false),
		MaxConcurrentFetches:   8,
		MaxConcurrentMessages:  32,
		RequestTimeout:         12 * time.Second,
		RetryAttempts:          2,
		RetryMinDelay:          150 * time.Millisecond,
		RetryMaxDelay:          350 * time.Millisecond,
		CacheTTL:               24 * time.Hour,
		NegativeCacheTTL:       5 * time.Minute,
		FuzzyMatchThreshold:    0.7,
		CircuitBreakerMaxFails: 5,
		CircuitBreakerTimeout:  60 * time.Second,
		TextMirrorURL:          "https://r.jina.ai/",
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	return cfg, nil
}

// Validate checks if the configuration values are valid
func (c *Config) Validate() error {
	if c.TelegramToken == "" {
		return fmt.Errorf("telegram token cannot be empty")
	}

	if c.MaxConcurrentFetches < 1 {
		return fmt.Errorf("max concurrent fetches must be at least 1, got %d", c.MaxConcurrentFetches)
	}

	if c.MaxConcurrentMessages < 1 {
		return fmt.Errorf("max concurrent messages must be at least 1, got %d", c.MaxConcurrentMessages)
	}

	if c.RequestTimeout <= 0 {
		return fmt.Errorf("request timeout must be positive, got %v", c.RequestTimeout)
	}

	if c.RetryAttempts < 0 {
		return fmt.Errorf("retry attempts cannot be negative, got %d", c.RetryAttempts)
	}

	if c.FuzzyMatchThreshold < 0 || c.FuzzyMatchThreshold > 1 {
		return fmt.Errorf("fuzzy match threshold must be between 0 and 1, got %f", c.FuzzyMatchThreshold)
	}

	if c.CircuitBreakerMaxFails < 1 {
		return fmt.Errorf("circuit breaker max fails must be at least 1, got %d", c.CircuitBreakerMaxFails)
	}

	if c.CircuitBreakerTimeout <= 0 {
		return fmt.Errorf("circuit breaker timeout must be positive, got %v", c.CircuitBreakerTimeout)
	}

	return nil
}

// envBool parses a boolean from environment variable with a default value
func envBool(key string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch v {
	case "1", "true", "yes", "y":
		return true
	case "0", "false", "no", "n":
		return false
	default:
		return def
	}
}

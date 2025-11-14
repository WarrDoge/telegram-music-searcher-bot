// Package http provides HTTP client utilities with retry and circuit breaker support
package http

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"time"

	"github.com/sony/gobreaker"
	"go.uber.org/zap"
)

const (
	userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
)

// Config holds HTTP client configuration
type Config struct {
	Timeout       time.Duration
	RetryAttempts int
	RetryMinDelay time.Duration
	RetryMaxDelay time.Duration
	Debug         bool
}

// Client provides HTTP operations with retry logic and circuit breakers
type Client struct {
	httpClient *http.Client
	config     *Config
	logger     *zap.Logger
	rand       *rand.Rand
}

// New creates a new HTTP client with the given configuration
func New(config *Config, logger *zap.Logger) *Client {
	httpClient := &http.Client{
		Timeout: config.Timeout,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   10,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			ForceAttemptHTTP2:     true,
		},
	}

	return &Client{
		httpClient: httpClient,
		config:     config,
		logger:     logger,
		rand:       rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// Fetch fetches a URL with retry logic
func (c *Client) Fetch(ctx context.Context, targetURL string) (*http.Response, error) {
	var lastErr error

	for attempt := 0; attempt < c.config.RetryAttempts; attempt++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
		if err != nil {
			return nil, fmt.Errorf("new request: %w", err)
		}

		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("Accept-Language", "en-US,en;q=0.9,uk;q=0.8,ru;q=0.7")
		req.Header.Set("Accept", "text/html,application/json;q=0.9,*/*;q=0.8")

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = err
		} else if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, nil
		} else if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			lastErr = fmt.Errorf("status %d for %s", resp.StatusCode, targetURL)
		} else {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			return nil, fmt.Errorf("status %d for %s", resp.StatusCode, targetURL)
		}

		if attempt < c.config.RetryAttempts-1 {
			delay := c.config.RetryMinDelay + time.Duration(c.rand.Intn(int(c.config.RetryMaxDelay-c.config.RetryMinDelay)))
			if c.config.Debug {
				c.logger.Debug("Retrying HTTP request",
					zap.Int("attempt", attempt+1),
					zap.Int("max_attempts", c.config.RetryAttempts),
					zap.String("url", targetURL),
					zap.Duration("delay", delay))
			}

			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}

	return nil, fmt.Errorf("failed to fetch %s after %d attempts: %w", targetURL, c.config.RetryAttempts, lastErr)
}

// CircuitBreaker wraps a gobreaker circuit breaker with platform-specific settings
type CircuitBreaker struct {
	breaker *gobreaker.CircuitBreaker
	logger  *zap.Logger
}

// CircuitBreakerConfig holds circuit breaker configuration
type CircuitBreakerConfig struct {
	Name        string
	MaxFails    int
	Timeout     time.Duration
	OnStateChange func(name string, from gobreaker.State, to gobreaker.State)
}

// NewCircuitBreaker creates a new circuit breaker with the given configuration
func NewCircuitBreaker(config *CircuitBreakerConfig, logger *zap.Logger) *CircuitBreaker {
	settings := gobreaker.Settings{
		Name:        config.Name,
		MaxRequests: uint32(config.MaxFails),
		Interval:    time.Minute,
		Timeout:     config.Timeout,
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			failureRatio := float64(counts.TotalFailures) / float64(counts.Requests)
			return counts.Requests >= 3 && failureRatio >= 0.6
		},
		OnStateChange: func(name string, from gobreaker.State, to gobreaker.State) {
			if logger != nil {
				logger.Info("Circuit breaker state changed",
					zap.String("service", name),
					zap.String("from", from.String()),
					zap.String("to", to.String()))
			}
			if config.OnStateChange != nil {
				config.OnStateChange(name, from, to)
			}
		},
	}

	return &CircuitBreaker{
		breaker: gobreaker.NewCircuitBreaker(settings),
		logger:  logger,
	}
}

// Execute runs the function through the circuit breaker
func (cb *CircuitBreaker) Execute(fn func() (interface{}, error)) (interface{}, error) {
	return cb.breaker.Execute(fn)
}

// State returns the current state of the circuit breaker
func (cb *CircuitBreaker) State() gobreaker.State {
	return cb.breaker.State()
}

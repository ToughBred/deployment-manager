// Package retry provides a generic retry strategy with exponential backoff
// and jitter. It is used by the orchestrator for health checks, image pulls,
// and migration runs where transient failures are expected.
package retry

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"time"
)

// Config controls the retry behaviour.
type Config struct {
	// MaxAttempts is the total number of attempts (including the first).
	MaxAttempts int
	// BaseDelay is the initial wait between attempts.
	BaseDelay time.Duration
	// MaxDelay caps the exponential backoff.
	MaxDelay time.Duration
	// Multiplier is the exponential base. Typically 2.0.
	Multiplier float64
	// Jitter adds randomness to prevent thundering herd.
	// 0.0 = no jitter, 1.0 = up to 100% jitter.
	Jitter float64
}

// DefaultConfig is a sensible default for transient network operations.
var DefaultConfig = Config{
	MaxAttempts: 5,
	BaseDelay:   2 * time.Second,
	MaxDelay:    30 * time.Second,
	Multiplier:  2.0,
	Jitter:      0.2,
}

// HealthCheckConfig is calibrated for post-deployment health checks where
// the application needs time to start.
var HealthCheckConfig = Config{
	MaxAttempts: 12,
	BaseDelay:   10 * time.Second,
	MaxDelay:    10 * time.Second, // fixed interval for health checks
	Multiplier:  1.0,
	Jitter:      0.0,
}

// Do executes fn up to cfg.MaxAttempts times, respecting ctx cancellation.
// It returns the first nil error or the last non-nil error if all attempts fail.
func Do(ctx context.Context, cfg Config, fn func(attempt int) error) error {
	var lastErr error
	for attempt := 1; attempt <= cfg.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("context cancelled before attempt %d: %w", attempt, err)
		}

		lastErr = fn(attempt)
		if lastErr == nil {
			return nil
		}

		if attempt == cfg.MaxAttempts {
			break
		}

		delay := backoffDelay(cfg, attempt)
		select {
		case <-ctx.Done():
			return fmt.Errorf("context cancelled during backoff: %w", ctx.Err())
		case <-time.After(delay):
		}
	}
	return fmt.Errorf("all %d attempts failed: %w", cfg.MaxAttempts, lastErr)
}

// backoffDelay calculates the wait before the next attempt.
func backoffDelay(cfg Config, attempt int) time.Duration {
	base := float64(cfg.BaseDelay) * math.Pow(cfg.Multiplier, float64(attempt-1))
	if max := float64(cfg.MaxDelay); base > max {
		base = max
	}
	if cfg.Jitter > 0 {
		jitter := base * cfg.Jitter * rand.Float64() //nolint:gosec — not crypto
		base += jitter
	}
	return time.Duration(base)
}

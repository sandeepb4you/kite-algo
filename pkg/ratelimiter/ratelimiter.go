// Package ratelimiter implements a simple, goroutine-safe token-bucket rate
// limiter. Kite enforces per-endpoint rate limits (e.g. 3 order placements per
// second); exceeding them returns HTTP 429 / "Too many requests" and can get the
// API key temporarily blocked. Every Kite call should pass through a limiter.
package ratelimiter

import (
	"context"
	"sync"
	"time"
)

// Limiter allows up to `rate` operations per second, with a burst capacity.
// It blocks (respecting context cancellation) when the bucket is empty.
type Limiter struct {
	mu        sync.Mutex
	tokens    float64   // current tokens in the bucket
	maxTokens float64   // bucket capacity (== burst)
	rate      float64   // tokens added per second
	last      time.Time // last refill time
}

// New returns a Limiter allowing `rate` ops/sec with a burst equal to rate
// (rounded up, min 1). For Kite orders use New(3).
func New(rate float64) *Limiter {
	max := rate
	if max < 1 {
		max = 1
	}
	return &Limiter{
		tokens:    max, // start full so the first burst isn't throttled
		maxTokens: max,
		rate:      rate,
		last:      time.Now(),
	}
}

// Wait blocks until a token is available or ctx is canceled.
func (l *Limiter) Wait(ctx context.Context) error {
	for {
		l.mu.Lock()
		l.refill()
		if l.tokens >= 1 {
			l.tokens -= 1
			l.mu.Unlock()
			return nil
		}
		// Compute how long until one token is available.
		needed := 1 - l.tokens
		wait := time.Duration(needed / l.rate * float64(time.Second))
		l.mu.Unlock()

		select {
		case <-time.After(wait):
			// loop and try again
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// refill adds tokens for elapsed time since the last call. Caller holds mu.
func (l *Limiter) refill() {
	now := time.Now()
	elapsed := now.Sub(l.last).Seconds()
	l.last = now
	l.tokens += elapsed * l.rate
	if l.tokens > l.maxTokens {
		l.tokens = l.maxTokens
	}
}

// TryAcquire attempts to take a token without blocking. Returns true on success.
// Useful for non-critical calls that can be dropped under load.
func (l *Limiter) TryAcquire() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refill()
	if l.tokens >= 1 {
		l.tokens -= 1
		return true
	}
	return false
}

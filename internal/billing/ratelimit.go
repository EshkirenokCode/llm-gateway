package billing

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// RateLimiter is a per-key token bucket. State lives in memory, so each
// gateway instance enforces its own limit: with N instances behind a load
// balancer the effective limit is N times the configured one. See
// docs/adr/0005-in-memory-rate-limiting.md.
type RateLimiter struct {
	burstRatio float64
	now        func() time.Time

	mu      sync.Mutex
	buckets map[uuid.UUID]*bucket
}

type bucket struct {
	tokens   float64
	capacity float64
	// refill is tokens added per second.
	refill float64
	last   time.Time
}

// NewRateLimiter returns a limiter. burstRatio decides how much of a minute's
// allowance may be spent at once: 0.25 lets a client burst a quarter of its
// per-minute budget and then settle into a steady rate.
func NewRateLimiter(burstRatio float64) *RateLimiter {
	if burstRatio <= 0 {
		burstRatio = 0.25
	}
	return &RateLimiter{
		burstRatio: burstRatio,
		now:        time.Now,
		buckets:    make(map[uuid.UUID]*bucket),
	}
}

// Allow consumes one token for the key. A rpm of zero or less means unlimited.
// The second return value is how long to wait before retrying.
func (l *RateLimiter) Allow(keyID uuid.UUID, rpm int) (bool, time.Duration) {
	if rpm <= 0 {
		return true, 0
	}

	now := l.now()
	capacity := float64(rpm) * l.burstRatio
	if capacity < 1 {
		capacity = 1
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[keyID]
	if !ok {
		b = &bucket{tokens: capacity, capacity: capacity, refill: float64(rpm) / 60, last: now}
		l.buckets[keyID] = b
	}

	// The limit can change while a bucket is alive, for example after an
	// operator raises it.
	if b.capacity != capacity {
		b.capacity = capacity
		b.refill = float64(rpm) / 60
		if b.tokens > capacity {
			b.tokens = capacity
		}
	}

	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * b.refill
		if b.tokens > b.capacity {
			b.tokens = b.capacity
		}
		b.last = now
	}

	if b.tokens < 1 {
		missing := 1 - b.tokens
		return false, time.Duration(missing / b.refill * float64(time.Second))
	}

	b.tokens--

	return true, 0
}

// Forget drops the bucket for a key, used when a key is revoked.
func (l *RateLimiter) Forget(keyID uuid.UUID) {
	l.mu.Lock()
	delete(l.buckets, keyID)
	l.mu.Unlock()
}

// Sweep removes buckets untouched for the given age. Without it the map grows
// with every key that ever made a request.
func (l *RateLimiter) Sweep(maxAge time.Duration) int {
	cutoff := l.now().Add(-maxAge)

	l.mu.Lock()
	defer l.mu.Unlock()

	removed := 0
	for id, b := range l.buckets {
		if b.last.Before(cutoff) {
			delete(l.buckets, id)
			removed++
		}
	}

	return removed
}

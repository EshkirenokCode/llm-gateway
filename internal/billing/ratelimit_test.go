package billing_test

import (
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/EshkirenokCode/llm-gateway/internal/billing"
)

func TestRateLimiterAllowsBurstThenBlocks(t *testing.T) {
	t.Parallel()

	l := billing.NewRateLimiter(0.25)
	key := uuid.New()

	// 60 rpm with a 0.25 burst ratio means 15 requests before throttling.
	for i := 0; i < 15; i++ {
		allowed, _ := l.Allow(key, 60)
		require.True(t, allowed, "request %d should pass", i+1)
	}

	allowed, retryAfter := l.Allow(key, 60)
	require.False(t, allowed)
	require.Positive(t, retryAfter)
	require.Less(t, retryAfter, 2*time.Second, "at 60 rpm a token returns within a second")
}

func TestRateLimiterRefills(t *testing.T) {
	t.Parallel()

	l := billing.NewRateLimiter(0.25)
	key := uuid.New()

	for {
		if allowed, _ := l.Allow(key, 600); !allowed {
			break
		}
	}

	// 600 rpm is ten tokens per second, so 200ms is worth two of them.
	time.Sleep(250 * time.Millisecond)

	allowed, _ := l.Allow(key, 600)
	require.True(t, allowed)
}

func TestRateLimiterIsPerKey(t *testing.T) {
	t.Parallel()

	l := billing.NewRateLimiter(0.25)
	noisy, quiet := uuid.New(), uuid.New()

	for i := 0; i < 20; i++ {
		l.Allow(noisy, 60)
	}

	allowed, _ := l.Allow(quiet, 60)
	require.True(t, allowed, "one key must not throttle another")
}

func TestRateLimiterUnlimited(t *testing.T) {
	t.Parallel()

	l := billing.NewRateLimiter(0.25)
	key := uuid.New()

	for i := 0; i < 1000; i++ {
		allowed, _ := l.Allow(key, 0)
		require.True(t, allowed)
	}
}

func TestRateLimiterSweepAndForget(t *testing.T) {
	t.Parallel()

	l := billing.NewRateLimiter(0.25)
	key := uuid.New()

	l.Allow(key, 60)
	require.Zero(t, l.Sweep(time.Hour), "a fresh bucket must survive")

	time.Sleep(20 * time.Millisecond)
	require.Equal(t, 1, l.Sweep(10*time.Millisecond))

	l.Allow(key, 60)
	l.Forget(key)
	require.Zero(t, l.Sweep(time.Hour))
}

func TestRateLimiterConcurrent(t *testing.T) {
	t.Parallel()

	l := billing.NewRateLimiter(1)
	key := uuid.New()

	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0

	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := l.Allow(key, 100); ok {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	require.LessOrEqual(t, allowed, 101, "the bucket must not hand out more than its capacity")
	require.Positive(t, allowed)
}

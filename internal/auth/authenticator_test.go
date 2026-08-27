package auth_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/EshkirenokCode/llm-gateway/internal/auth"
)

type fakeLookup struct {
	mu       sync.Mutex
	identity auth.Identity
	err      error

	calls atomic.Int64
	used  atomic.Int64
}

func (f *fakeLookup) IdentityByKeyHash(context.Context, []byte) (auth.Identity, error) {
	f.calls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.identity, f.err
}

func (f *fakeLookup) MarkKeyUsed(context.Context, uuid.UUID) { f.used.Add(1) }

func (f *fakeLookup) set(identity auth.Identity, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.identity, f.err = identity, err
}

func TestAuthenticateResolvesIdentity(t *testing.T) {
	t.Parallel()

	want := auth.Identity{TenantID: uuid.New(), TenantPrefix: "acme", KeyID: uuid.New()}
	lookup := &fakeLookup{identity: want}
	a := auth.NewAuthenticator(lookup, 0)

	got, err := a.Authenticate(context.Background(), "sk_acme_secret")

	require.NoError(t, err)
	require.Equal(t, want, got)
	require.Equal(t, int64(1), lookup.used.Load(), "last_used_at should be refreshed")
}

func TestAuthenticateRejectsEmptyKey(t *testing.T) {
	t.Parallel()

	a := auth.NewAuthenticator(&fakeLookup{}, 0)

	_, err := a.Authenticate(context.Background(), "")

	require.ErrorIs(t, err, auth.ErrNoCredentials)
}

func TestCacheAvoidsRepeatedLookups(t *testing.T) {
	t.Parallel()

	lookup := &fakeLookup{identity: auth.Identity{KeyID: uuid.New()}}
	a := auth.NewAuthenticator(lookup, time.Minute)

	for i := 0; i < 5; i++ {
		_, err := a.Authenticate(context.Background(), "sk_acme_secret")
		require.NoError(t, err)
	}

	require.Equal(t, int64(1), lookup.calls.Load())
}

func TestCacheRemembersRejections(t *testing.T) {
	t.Parallel()

	lookup := &fakeLookup{err: auth.ErrUnknownKey}
	a := auth.NewAuthenticator(lookup, time.Minute)

	for i := 0; i < 3; i++ {
		_, err := a.Authenticate(context.Background(), "sk_acme_bogus")
		require.ErrorIs(t, err, auth.ErrUnknownKey)
	}

	require.Equal(t, int64(1), lookup.calls.Load(), "a known-bad key should not hit the database repeatedly")
}

func TestTransientErrorsAreNotCached(t *testing.T) {
	t.Parallel()

	boom := errors.New("connection refused")
	lookup := &fakeLookup{err: boom}
	a := auth.NewAuthenticator(lookup, time.Minute)

	_, err := a.Authenticate(context.Background(), "sk_acme_secret")
	require.ErrorIs(t, err, boom)

	lookup.set(auth.Identity{KeyID: uuid.New()}, nil)

	_, err = a.Authenticate(context.Background(), "sk_acme_secret")
	require.NoError(t, err, "the database recovered, so the key must be retried")
	require.Equal(t, int64(2), lookup.calls.Load())
}

func TestInvalidateDropsCachedVerdict(t *testing.T) {
	t.Parallel()

	lookup := &fakeLookup{identity: auth.Identity{KeyID: uuid.New()}}
	a := auth.NewAuthenticator(lookup, time.Minute)

	_, err := a.Authenticate(context.Background(), "sk_acme_secret")
	require.NoError(t, err)

	a.Invalidate("sk_acme_secret")
	lookup.set(auth.Identity{}, auth.ErrRevoked)

	_, err = a.Authenticate(context.Background(), "sk_acme_secret")
	require.ErrorIs(t, err, auth.ErrRevoked)
}

func TestConcurrentAuthenticate(t *testing.T) {
	t.Parallel()

	lookup := &fakeLookup{identity: auth.Identity{KeyID: uuid.New()}}
	a := auth.NewAuthenticator(lookup, time.Minute)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := a.Authenticate(context.Background(), "sk_acme_secret")
			require.NoError(t, err)
		}()
	}
	wg.Wait()
}

func TestSoftLimit(t *testing.T) {
	t.Parallel()

	withBudget := auth.Identity{HasBudget: true, BudgetMicros: 1_000_000, SoftLimitRatio: 0.8}
	require.Equal(t, int64(800_000), withBudget.SoftLimitMicros())

	require.Zero(t, auth.Identity{}.SoftLimitMicros())
}

package auth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

var (
	// ErrNoCredentials means the request carried no usable Authorization header.
	ErrNoCredentials = errors.New("missing api key")
	// ErrUnknownKey means the key is not in the database.
	ErrUnknownKey = errors.New("unknown api key")
	// ErrRevoked means the key exists but was revoked.
	ErrRevoked = errors.New("api key revoked")
	// ErrTenantDisabled means the tenant itself is switched off.
	ErrTenantDisabled = errors.New("tenant disabled")
)

// Identity is the caller behind a request.
type Identity struct {
	TenantID       uuid.UUID
	TenantName     string
	TenantPrefix   string
	KeyID          uuid.UUID
	RateLimitRPM   int
	BudgetMicros   int64
	HasBudget      bool
	SoftLimitRatio float64
}

// Lookup resolves a key hash to an identity. It is implemented by the storage
// layer; the interface keeps auth testable without a database.
type Lookup interface {
	IdentityByKeyHash(ctx context.Context, hash []byte) (Identity, error)
	MarkKeyUsed(ctx context.Context, keyID uuid.UUID)
}

// Authenticator verifies keys, with a short lived cache in front of the
// database. The cache trades immediate revocation for one database round trip
// per key per TTL; see docs/adr/0006-api-key-cache.md.
type Authenticator struct {
	lookup Lookup
	ttl    time.Duration
	now    func() time.Time

	mu      sync.RWMutex
	entries map[string]cacheEntry
}

type cacheEntry struct {
	identity Identity
	err      error
	expires  time.Time
}

// NewAuthenticator returns an Authenticator. A ttl of zero disables caching.
func NewAuthenticator(lookup Lookup, ttl time.Duration) *Authenticator {
	return &Authenticator{
		lookup:  lookup,
		ttl:     ttl,
		now:     time.Now,
		entries: make(map[string]cacheEntry),
	}
}

// Authenticate resolves a plaintext key to an identity.
func (a *Authenticator) Authenticate(ctx context.Context, plaintext string) (Identity, error) {
	if plaintext == "" {
		return Identity{}, ErrNoCredentials
	}

	hash := HashKey(plaintext)
	cacheKey := string(hash)

	if identity, err, ok := a.fromCache(cacheKey); ok {
		if err != nil {
			return Identity{}, err
		}
		a.lookup.MarkKeyUsed(ctx, identity.KeyID)
		return identity, nil
	}

	identity, err := a.lookup.IdentityByKeyHash(ctx, hash)
	if err != nil {
		// Only cache verdicts that are cheap to be wrong about. A transient
		// database failure must not be remembered as "key is invalid".
		if isPermanent(err) {
			a.store(cacheKey, Identity{}, err)
		}
		return Identity{}, err
	}

	a.store(cacheKey, identity, nil)
	a.lookup.MarkKeyUsed(ctx, identity.KeyID)

	return identity, nil
}

// Invalidate drops a cached verdict, so revoking a key can take effect at once.
func (a *Authenticator) Invalidate(plaintext string) {
	a.mu.Lock()
	delete(a.entries, string(HashKey(plaintext)))
	a.mu.Unlock()
}

func (a *Authenticator) fromCache(key string) (Identity, error, bool) {
	if a.ttl <= 0 {
		return Identity{}, nil, false
	}

	a.mu.RLock()
	entry, ok := a.entries[key]
	a.mu.RUnlock()

	if !ok || a.now().After(entry.expires) {
		return Identity{}, nil, false
	}

	return entry.identity, entry.err, true
}

func (a *Authenticator) store(key string, identity Identity, err error) {
	if a.ttl <= 0 {
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	// The map only grows if keys keep changing, which in practice means an
	// attacker guessing keys. Drop everything once it gets unreasonable.
	if len(a.entries) > 10000 {
		a.entries = make(map[string]cacheEntry)
	}

	a.entries[key] = cacheEntry{identity: identity, err: err, expires: a.now().Add(a.ttl)}
}

func isPermanent(err error) bool {
	return errors.Is(err, ErrUnknownKey) ||
		errors.Is(err, ErrRevoked) ||
		errors.Is(err, ErrTenantDisabled)
}

// SoftLimitMicros is the spend at which a warning is returned, or zero when
// the tenant has no budget.
func (i Identity) SoftLimitMicros() int64 {
	if !i.HasBudget {
		return 0
	}
	return int64(float64(i.BudgetMicros) * i.SoftLimitRatio)
}

// String renders an identity for logs without leaking anything sensitive.
func (i Identity) String() string {
	return fmt.Sprintf("tenant=%s key=%s", i.TenantPrefix, i.KeyID)
}

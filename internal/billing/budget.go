package billing

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ErrBudgetExceeded is returned when a tenant has spent its monthly budget.
var ErrBudgetExceeded = errors.New("monthly budget exceeded")

// SpendReader reports how much a tenant has spent since a point in time.
type SpendReader interface {
	SpentSince(ctx context.Context, tenantID uuid.UUID, since time.Time) (Micros, error)
}

// Budget decides whether a tenant may make another request.
type Budget struct {
	spend SpendReader
	ttl   time.Duration
	now   func() time.Time

	mu     sync.Mutex
	cached map[uuid.UUID]spendEntry
}

type spendEntry struct {
	spent   Micros
	expires time.Time
	// pending accumulates cost recorded since the last read, so a burst of
	// requests inside one cache window still moves towards the limit.
	pending Micros
}

// Status is the outcome of a budget check.
type Status struct {
	Spent     Micros
	Limit     Micros
	HasLimit  bool
	SoftLimit Micros
	// OverSoftLimit means the caller should be warned but not blocked.
	OverSoftLimit bool
}

// Remaining is how much is left, or zero when there is no limit.
func (s Status) Remaining() Micros {
	if !s.HasLimit {
		return 0
	}
	if s.Spent >= s.Limit {
		return 0
	}
	return s.Limit - s.Spent
}

// NewBudget returns a Budget. Spend is cached for ttl because summing the
// requests table on every call would put the hot path behind an aggregate.
func NewBudget(spend SpendReader, ttl time.Duration) *Budget {
	return &Budget{
		spend:  spend,
		ttl:    ttl,
		now:    time.Now,
		cached: make(map[uuid.UUID]spendEntry),
	}
}

// Check reports the tenant's budget status, or ErrBudgetExceeded when the hard
// cap is reached.
func (b *Budget) Check(ctx context.Context, tenantID uuid.UUID, limit Micros, hasLimit bool, softRatio float64) (Status, error) {
	spent, err := b.spent(ctx, tenantID)
	if err != nil {
		return Status{}, err
	}

	status := Status{Spent: spent, Limit: limit, HasLimit: hasLimit}
	if !hasLimit {
		return status, nil
	}

	status.SoftLimit = Micros(float64(limit) * softRatio)
	status.OverSoftLimit = spent >= status.SoftLimit

	if spent >= limit {
		return status, fmt.Errorf("%w: spent %s of %s", ErrBudgetExceeded, spent, limit)
	}

	return status, nil
}

// Record adds the cost of a finished request to the cached total. The
// authoritative number still comes from the database on the next refresh.
func (b *Budget) Record(tenantID uuid.UUID, cost Micros) {
	if cost <= 0 {
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	entry, ok := b.cached[tenantID]
	if !ok {
		return
	}
	entry.pending += cost
	b.cached[tenantID] = entry
}

func (b *Budget) spent(ctx context.Context, tenantID uuid.UUID) (Micros, error) {
	b.mu.Lock()
	entry, ok := b.cached[tenantID]
	fresh := ok && b.now().Before(entry.expires)
	b.mu.Unlock()

	if fresh {
		return entry.spent + entry.pending, nil
	}

	total, err := b.spend.SpentSince(ctx, tenantID, MonthStart(b.now()))
	if err != nil {
		// Falling back to a stale total is better than failing the request:
		// the alternative is an outage caused by the billing subsystem.
		if ok {
			return entry.spent + entry.pending, nil
		}
		return 0, fmt.Errorf("read tenant spend: %w", err)
	}

	b.mu.Lock()
	b.cached[tenantID] = spendEntry{spent: total, expires: b.now().Add(b.ttl)}
	b.mu.Unlock()

	return total, nil
}

// MonthStart is the beginning of the billing period containing t, in UTC.
func MonthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

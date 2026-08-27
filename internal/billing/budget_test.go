package billing_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/EshkirenokCode/llm-gateway/internal/billing"
)

type fakeSpend struct {
	total billing.Micros
	err   error
	calls atomic.Int64
}

func (f *fakeSpend) SpentSince(context.Context, uuid.UUID, time.Time) (billing.Micros, error) {
	f.calls.Add(1)
	return f.total, f.err
}

func TestBudgetAllowsUnderLimit(t *testing.T) {
	t.Parallel()

	b := billing.NewBudget(&fakeSpend{total: 100}, time.Minute)

	status, err := b.Check(context.Background(), uuid.New(), 1000, true, 0.8)

	require.NoError(t, err)
	require.False(t, status.OverSoftLimit)
	require.Equal(t, billing.Micros(900), status.Remaining())
}

func TestBudgetWarnsAtSoftLimit(t *testing.T) {
	t.Parallel()

	b := billing.NewBudget(&fakeSpend{total: 850}, time.Minute)

	status, err := b.Check(context.Background(), uuid.New(), 1000, true, 0.8)

	require.NoError(t, err, "the soft limit warns, it does not block")
	require.True(t, status.OverSoftLimit)
	require.Equal(t, billing.Micros(800), status.SoftLimit)
}

func TestBudgetBlocksAtHardLimit(t *testing.T) {
	t.Parallel()

	b := billing.NewBudget(&fakeSpend{total: 1000}, time.Minute)

	_, err := b.Check(context.Background(), uuid.New(), 1000, true, 0.8)

	require.ErrorIs(t, err, billing.ErrBudgetExceeded)
}

func TestBudgetIgnoresLimitWhenTenantHasNone(t *testing.T) {
	t.Parallel()

	b := billing.NewBudget(&fakeSpend{total: 999_999_999}, time.Minute)

	status, err := b.Check(context.Background(), uuid.New(), 0, false, 0.8)

	require.NoError(t, err)
	require.False(t, status.HasLimit)
	require.Zero(t, status.Remaining())
}

func TestRecordedSpendCountsBeforeRefresh(t *testing.T) {
	t.Parallel()

	spend := &fakeSpend{total: 900}
	b := billing.NewBudget(spend, time.Minute)
	tenant := uuid.New()

	_, err := b.Check(context.Background(), tenant, 1000, true, 0.8)
	require.NoError(t, err)

	b.Record(tenant, 150)

	_, err = b.Check(context.Background(), tenant, 1000, true, 0.8)
	require.ErrorIs(t, err, billing.ErrBudgetExceeded, "spend recorded inside the cache window must still count")
	require.Equal(t, int64(1), spend.calls.Load())
}

func TestStaleTotalIsUsedWhenTheDatabaseFails(t *testing.T) {
	t.Parallel()

	spend := &fakeSpend{total: 100}
	b := billing.NewBudget(spend, time.Nanosecond)
	tenant := uuid.New()

	_, err := b.Check(context.Background(), tenant, 1000, true, 0.8)
	require.NoError(t, err)

	spend.err = errors.New("connection refused")

	status, err := b.Check(context.Background(), tenant, 1000, true, 0.8)
	require.NoError(t, err, "billing must not take the gateway down with it")
	require.Equal(t, billing.Micros(100), status.Spent)
}

func TestFirstCheckFailsWhenTheDatabaseIsDown(t *testing.T) {
	t.Parallel()

	b := billing.NewBudget(&fakeSpend{err: errors.New("connection refused")}, time.Minute)

	_, err := b.Check(context.Background(), uuid.New(), 1000, true, 0.8)

	require.ErrorContains(t, err, "read tenant spend")
}

func TestMonthStart(t *testing.T) {
	t.Parallel()

	got := billing.MonthStart(time.Date(2026, 3, 17, 12, 30, 0, 0, time.UTC))

	require.Equal(t, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), got)
}

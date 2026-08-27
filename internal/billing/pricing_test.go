package billing_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/EshkirenokCode/llm-gateway/internal/billing"
)

func TestCostUsesModelPrice(t *testing.T) {
	t.Parallel()

	p := billing.DefaultPricing()

	// gpt-4o-mini: 150000 micros per million prompt tokens, 600000 completion.
	got := p.Cost("gpt-4o-mini", 1_000_000, 1_000_000)

	require.Equal(t, billing.Micros(750_000), got)
}

func TestCostRoundsUp(t *testing.T) {
	t.Parallel()

	p := billing.DefaultPricing()

	// A single token costs a fraction of a micro dollar; it must not be free.
	require.Equal(t, billing.Micros(1), p.Cost("gpt-4o-mini", 1, 0))
	require.Zero(t, p.Cost("gpt-4o-mini", 0, 0))
}

func TestUnknownModelFallsBack(t *testing.T) {
	t.Parallel()

	p := billing.DefaultPricing()

	_, listed := p.Price("some-new-model")
	require.False(t, listed)

	require.Positive(t, p.Cost("some-new-model", 1000, 1000), "an unlisted model must still be charged")
}

func TestModelLookupIsCaseInsensitive(t *testing.T) {
	t.Parallel()

	p := billing.DefaultPricing()

	_, listed := p.Price("GPT-4o-MINI")
	require.True(t, listed)
}

func TestLoadPricing(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "pricing.json")
	require.NoError(t, os.WriteFile(path, []byte(`{
	  "fallback": {"prompt_per_mtok": 2000000, "completion_per_mtok": 4000000},
	  "models": {"tiny": {"prompt_per_mtok": 100000, "completion_per_mtok": 200000}}
	}`), 0o600))

	p, err := billing.LoadPricing(path)
	require.NoError(t, err)

	require.Equal(t, billing.Micros(300_000), p.Cost("tiny", 1_000_000, 1_000_000))
	require.Equal(t, billing.Micros(6_000_000), p.Cost("other", 1_000_000, 1_000_000))
}

func TestLoadPricingRejectsBadFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	empty := filepath.Join(dir, "empty.json")
	require.NoError(t, os.WriteFile(empty, []byte(`{"models":{}}`), 0o600))
	_, err := billing.LoadPricing(empty)
	require.ErrorContains(t, err, "lists no models")

	negative := filepath.Join(dir, "negative.json")
	require.NoError(t, os.WriteFile(negative, []byte(`{"models":{"a":{"prompt_per_mtok":-1}}}`), 0o600))
	_, err = billing.LoadPricing(negative)
	require.ErrorContains(t, err, "negative price")

	_, err = billing.LoadPricing(filepath.Join(dir, "missing.json"))
	require.Error(t, err)
}

func TestMicrosFormatting(t *testing.T) {
	t.Parallel()

	require.Equal(t, "$0.001500", billing.Micros(1500).String())
	require.InDelta(t, 1.5, billing.Micros(1_500_000).Dollars(), 0.000001)
}

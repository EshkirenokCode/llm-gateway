// Package billing turns token counts into money and enforces tenant budgets.
package billing

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Micros is an amount in millionths of a US dollar. Money is kept in integers
// so that summing millions of rows stays exact.
type Micros int64

// Dollars renders the amount for humans.
func (m Micros) Dollars() float64 { return float64(m) / 1_000_000 }

func (m Micros) String() string { return fmt.Sprintf("$%.6f", m.Dollars()) }

// ModelPrice is the price of a model, per million tokens.
type ModelPrice struct {
	PromptPerMTok     Micros `json:"prompt_per_mtok"`
	CompletionPerMTok Micros `json:"completion_per_mtok"`
}

// Pricing maps model names to prices. Lookups are case insensitive and fall
// back to a default entry, so an unknown model is still charged rather than
// silently free.
type Pricing struct {
	models   map[string]ModelPrice
	fallback ModelPrice
}

// DefaultPricing is used when no pricing file is configured. The numbers are
// list prices in micro dollars per million tokens and go stale quickly, which
// is why they can be overridden without a rebuild.
func DefaultPricing() *Pricing {
	return &Pricing{
		models: map[string]ModelPrice{
			"gpt-4o":           {PromptPerMTok: 2_500_000, CompletionPerMTok: 10_000_000},
			"gpt-4o-mini":      {PromptPerMTok: 150_000, CompletionPerMTok: 600_000},
			"claude-sonnet-4":  {PromptPerMTok: 3_000_000, CompletionPerMTok: 15_000_000},
			"claude-haiku-4-5": {PromptPerMTok: 1_000_000, CompletionPerMTok: 5_000_000},
			"mock":             {},
		},
		fallback: ModelPrice{PromptPerMTok: 1_000_000, CompletionPerMTok: 3_000_000},
	}
}

type pricingFile struct {
	Fallback ModelPrice            `json:"fallback"`
	Models   map[string]ModelPrice `json:"models"`
}

// LoadPricing reads a pricing table from a JSON file.
func LoadPricing(path string) (*Pricing, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read pricing file: %w", err)
	}

	var parsed pricingFile
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&parsed); err != nil {
		return nil, fmt.Errorf("parse pricing file %s: %w", path, err)
	}

	if len(parsed.Models) == 0 {
		return nil, fmt.Errorf("pricing file %s lists no models", path)
	}

	models := make(map[string]ModelPrice, len(parsed.Models))
	for name, price := range parsed.Models {
		if price.PromptPerMTok < 0 || price.CompletionPerMTok < 0 {
			return nil, fmt.Errorf("model %q has a negative price", name)
		}
		models[strings.ToLower(name)] = price
	}

	return &Pricing{models: models, fallback: parsed.Fallback}, nil
}

// Price returns the price of a model and whether it was listed explicitly.
func (p *Pricing) Price(model string) (ModelPrice, bool) {
	price, ok := p.models[strings.ToLower(model)]
	if !ok {
		return p.fallback, false
	}
	return price, true
}

// Cost computes what a request costs. Rounding is upward on the total so that
// a busy tenant is never charged less than the provider charges us.
func (p *Pricing) Cost(model string, promptTokens, completionTokens int) Micros {
	price, _ := p.Price(model)

	prompt := divCeil(int64(promptTokens)*int64(price.PromptPerMTok), 1_000_000)
	completion := divCeil(int64(completionTokens)*int64(price.CompletionPerMTok), 1_000_000)

	return Micros(prompt + completion)
}

func divCeil(value, div int64) int64 {
	if value <= 0 {
		return 0
	}
	return (value + div - 1) / div
}

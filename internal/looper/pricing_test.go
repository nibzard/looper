package looper

import (
	"encoding/json"
	"testing"
	"time"
)

var pricingTestDate = time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)

func TestPricingEveryCatalogModelResolves(t *testing.T) {
	var providers []priceProvider
	if err := json.Unmarshal(priceData, &providers); err != nil {
		t.Fatal(err)
	}
	models, free := 0, 0
	for _, p := range providers {
		for _, m := range p.Models {
			t.Run(p.ID+"/"+m.ID, func(t *testing.T) {
				selected, provider := resolvePrice(m.ID, p.ID)
				if selected == nil || selected.ID != m.ID || provider != p.ID {
					t.Fatalf("provider model did not resolve: %v, %q", selected, provider)
				}
				rates := applicablePrices(selected.Prices, pricingTestDate)
				if rates == nil {
					t.Fatal("model has neither applicable prices nor a free marker")
				}
				models++
				if len(rates) == 0 {
					free++
				}
				// Exercise estimation where the catalog supplies scalar text
				// rates or an explicit free marker. Other units and context
				// tiers still resolve, but need usage details we do not have.
				var input, output float64
				if len(rates) == 0 || (json.Unmarshal(rates["input_mtok"], &input) == nil &&
					json.Unmarshal(rates["output_mtok"], &output) == nil) {
					value, gotProvider := estimateUsage(UsageRecord{
						Model: m.ID, Provider: p.ID, Input: 1_000_000, Output: 1_000_000,
					}, pricingTestDate)
					assertUSD(t, value, input+output)
					if gotProvider != p.ID {
						t.Fatalf("provider = %q, want %q", gotProvider, p.ID)
					}
				}
				// A bare catalog ID must also resolve. Shared IDs use the
				// documented defaults, so their provider can differ.
				bare, _ := resolvePrice(m.ID, "")
				if bare == nil || applicablePrices(bare.Prices, pricingTestDate) == nil {
					t.Fatal("bare catalog ID has neither prices nor a free marker")
				}
			})
		}
	}
	if models == 0 || free == 0 {
		t.Fatalf("catalog coverage is empty: %d models, %d free", models, free)
	}
	t.Logf("resolved %d catalog models, including %d explicit free markers", models, free)
}

func TestPricingProviderDefaultsAndFallbacks(t *testing.T) {
	for _, tt := range []struct {
		model, provider string
		want            float64
	}{
		{"codex-mini", "openai", 7.5},
		{"codex-mini-latest", "openai", 7.5},
		{"GLM-4-Air", "zhipuai", .138},
		{"glm-4-air", "zhipuai", .138},
		{"glm-5p1", "fireworks", 5.8},
		{"glm-4-32b", "openrouter", .48},
		{"glm-4-32b:free", "openrouter", 0},
		{"glm-5.3[1m]", "zai", 5.8},
		{"claude-sonnet-4-6", "anthropic", 18},
		{"gpt-5.3-codex", "openai", 15.75},
	} {
		t.Run(tt.model, func(t *testing.T) {
			value, provider := estimateUsage(UsageRecord{
				Model: tt.model, Input: 1_000_000, Output: 1_000_000,
			}, pricingTestDate)
			assertUSD(t, value, tt.want)
			if provider != tt.provider {
				t.Fatalf("provider = %q, want %q", provider, tt.provider)
			}
		})
	}
}

func TestPricingExplicitProviderAndUnknownModels(t *testing.T) {
	value, provider := estimateUsage(UsageRecord{
		Model: "GLM-5.3", Provider: "zhipuai", Input: 1_000_000, Output: 1_000_000,
	}, pricingTestDate)
	assertUSD(t, value, 4.965)
	if provider != "zhipuai" {
		t.Fatalf("explicit provider was replaced: %q", provider)
	}
	for _, u := range []UsageRecord{
		{Model: "unknown-model", Input: 100},
		{Model: "glm-unknown-model", Input: 100},
		{Model: "gpt-unknown-model", Input: 100},
		{Model: "glm-4-air", Provider: "zai", Input: 100},
		{Model: "codex-mini", Provider: "unknown-provider", Input: 100},
		{Model: "glm-4-32b:free", Input: 100, CacheRead: 200},
	} {
		if value, _ := estimateUsage(u, pricingTestDate); value != nil {
			t.Errorf("unknown or invalid usage received a price: %+v", u)
		}
	}
}

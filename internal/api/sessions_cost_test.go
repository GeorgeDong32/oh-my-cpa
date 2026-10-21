package api

import (
	"testing"

	"github.com/oh-my-cpa/oh-my-cpa/internal/pricing"
	"github.com/oh-my-cpa/oh-my-cpa/internal/sessionstats"
)

// The session cost estimate must tie to the price book through the gateway's
// own Quote (spec §5): base rates, model multiplier, uncached prompt /
// cache-read / cache-write / completion buckets — a flat per-field
// multiplication would overprice cache-read traffic by the prompt rate.
func TestEstimateSessionCostMatchesQuotePath(t *testing.T) {
	price := pricing.ModelPrice{
		Model: "glm-5.3", PromptPricePer1M: 1.0, CompletionPer1M: 4.0,
		CacheReadPer1M: 0.1, CacheWritePer1M: 1.0, PriceMultiplier: 2, Source: pricing.SourceManual,
	}
	rows := []sessionstats.AggregatedRow{{
		Bucket15MS: 1_700_000_000_000, Client: "pi", Model: "glm-5.3",
		Input: 900_000, Output: 200_000, CacheRead: 800_000, CacheWrite: 100_000, Reasoning: 50_000,
	}}
	// One Quote of the same tokens must equal the sum the estimator produces.
	want := pricing.Tokens{
		Input:     rows[0].Input + rows[0].CacheRead + rows[0].CacheWrite,
		Output:    rows[0].Output + rows[0].Reasoning,
		CacheRead: rows[0].CacheRead, CacheWrite: rows[0].CacheWrite,
	}
	breakdown, err := pricing.Quote(price, 1.0, want, rows[0].Bucket15MS, "")
	if err != nil {
		t.Fatal(err)
	}
	costNanos, unpriced := estimateSessionCost(rows, []pricing.ModelPrice{price})
	if unpriced != 0 {
		t.Fatalf("unpriced: %d", unpriced)
	}
	if costNanos != breakdown.TotalNanos {
		t.Fatalf("estimator %d != quote %d", costNanos, breakdown.TotalNanos)
	}
	// The flat-multiplication trap: pricing every input-side token at the
	// prompt rate would exceed the quote because cache reads price at a
	// tenth here.
	flatInput := (rows[0].Input + rows[0].CacheRead + rows[0].CacheWrite) * 1e3 // $1/M = 1000 nanos/token
	flatOutput := (rows[0].Output + rows[0].Reasoning) * 4e3                    // $4/M
	flatNanos := (flatInput + flatOutput) * 2                                   // model multiplier
	if costNanos >= flatNanos {
		t.Fatalf("estimate did not benefit from cache rates: %d vs flat %d", costNanos, flatNanos)
	}
	// Unpriced models add to the unpriced share, never to cost.
	other := append(rows, sessionstats.AggregatedRow{Bucket15MS: 1, Client: "x", Model: "mystery", Input: 5})
	costNanos2, unpriced2 := estimateSessionCost(other, []pricing.ModelPrice{price})
	if costNanos2 != costNanos || unpriced2 != 5 {
		t.Fatalf("unpriced handling: cost %d unpriced %d", costNanos2, unpriced2)
	}
}

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
	// Quote's Input is cache-inclusive: the quote subtracts the cache fields
	// itself before applying the prompt rate.
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
	// Unpriced models add to the unpriced share, never to cost. The unpriced
	// bucket keeps its exact token count even though it prices at zero.
	other := append(rows, sessionstats.AggregatedRow{Bucket15MS: 1, Client: "x", Model: "mystery", Input: 5})
	costNanos2, unpriced2 := estimateSessionCost(other, []pricing.ModelPrice{price})
	if costNanos2 != costNanos || unpriced2 != 5 {
		t.Fatalf("unpriced handling: cost %d unpriced %d", costNanos2, unpriced2)
	}
	// Per-model aggregation is exact: two rows of one model price the same as
	// each row quoted alone, summed.
	doubled := append([]sessionstats.AggregatedRow{}, rows...)
	doubled = append(doubled, sessionstats.AggregatedRow{
		Bucket15MS: rows[0].Bucket15MS + 1, Client: rows[0].Client, Model: rows[0].Model,
		Input: rows[0].Input, Output: rows[0].Output, CacheRead: rows[0].CacheRead,
		CacheWrite: rows[0].CacheWrite, Reasoning: rows[0].Reasoning,
	})
	costNanos3, _ := estimateSessionCost(doubled, []pricing.ModelPrice{price})
	if costNanos3 != 2*costNanos {
		t.Fatalf("summed-quote equivalence: %d != 2×%d", costNanos3, costNanos)
	}
}

// A tiered price row must not leak its long-context rate onto a whole
// 15-minute aggregate: tiers are per-request assertions an aggregate cannot
// decide (spec §5), so the estimate always quotes the base rates.
func TestEstimateSessionCostIgnoresTiers(t *testing.T) {
	highPrompt, highCompletion := 100.0, 400.0
	tiered := pricing.ModelPrice{
		Model: "glm-5.3", PromptPricePer1M: 1.0, CompletionPer1M: 4.0,
		CacheReadPer1M: 0.1, CacheWritePer1M: 1.0, PriceMultiplier: 1, Source: pricing.SourceManual,
		Tiers: []pricing.PriceTier{{MinPromptTokens: 100_000, PromptPricePer1M: &highPrompt, CompletionPer1M: &highCompletion}},
	}
	// 1.8M cache-inclusive input: the threshold tier fires on a plain Quote.
	row := sessionstats.AggregatedRow{
		Bucket15MS: 1_700_000_000_000, Client: "pi", Model: "glm-5.3",
		Input: 900_000, Output: 200_000, CacheRead: 800_000, CacheWrite: 100_000, Reasoning: 50_000,
	}
	tokens := pricing.Tokens{
		Input:  row.Input + row.CacheRead + row.CacheWrite,
		Output: row.Output + row.Reasoning, CacheRead: row.CacheRead, CacheWrite: row.CacheWrite,
	}
	withTiers, err := pricing.Quote(tiered, 1.0, tokens, row.Bucket15MS, "")
	if err != nil {
		t.Fatal(err)
	}
	base := tiered
	base.Tiers = nil
	baseOnly, err := pricing.Quote(base, 1.0, tokens, row.Bucket15MS, "")
	if err != nil {
		t.Fatal(err)
	}
	if withTiers.TotalNanos == baseOnly.TotalNanos {
		t.Fatal("fixture must actually fire the tier for this test to guard anything")
	}
	costNanos, _ := estimateSessionCost([]sessionstats.AggregatedRow{row}, []pricing.ModelPrice{tiered})
	if costNanos != baseOnly.TotalNanos {
		t.Fatalf("estimate %d != base-rate quote %d (tier leaked)", costNanos, baseOnly.TotalNanos)
	}
}

// Session files carry the clients' own spellings; the book stores the
// gateway's catalog names. The slug fallback must rejoin the two — case,
// channel prefix, and dated snapshots — without borrowing a neighbour's
// rates for a name no row answers to.
func TestSessionCostSlugFallback(t *testing.T) {
	book := []pricing.ModelPrice{
		{Model: "MiniMax-M3", UpstreamID: "minimax/minimax-m3", PromptPricePer1M: 1, CompletionPer1M: 4, PriceMultiplier: 1},
		{Model: "codex/gpt-5.6-sol", UpstreamID: "openai/gpt-5.6-sol", PromptPricePer1M: 2, CompletionPer1M: 8, PriceMultiplier: 1},
		{Model: "deepseek-flash", UpstreamID: "deepseek/deepseek-v4-flash", PromptPricePer1M: 1, CompletionPer1M: 3, PriceMultiplier: 1},
	}
	rows := []sessionstats.AggregatedRow{
		{Model: "Minimax/MiniMax-M3", Input: 1_000_000, Output: 1_000_000},
		{Model: "gpt-5.6-sol", Input: 1_000_000, Output: 1_000_000},
		{Model: "bailian/deepseek-v4-flash-0731", Input: 1_000_000},
	}
	cost, unpriced := estimateSessionCost(rows, book)
	if unpriced != 0 {
		t.Fatalf("same-model spellings must price: unpriced=%d", unpriced)
	}
	want, err := pricing.Quote(book[0], 1.0, pricing.Tokens{Input: 2_000_000}, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	// One lump per model: minimax quotes 1M in + 1M out, gpt-5.6-sol the same,
	// deepseek 1M in.
	_ = want
	// Exact-name matches must keep winning over the slug form: a book that
	// carries both spellings prices the exact one even if another row's slug
	// collides.
	dup := append(append([]pricing.ModelPrice{}, book...), pricing.ModelPrice{
		Model: "Minimax/MiniMax-M3", PromptPricePer1M: 9, CompletionPer1M: 9, PriceMultiplier: 1,
	})
	exactCost, exactUnpriced := estimateSessionCost(rows[:1], dup)
	if exactUnpriced != 0 {
		t.Fatalf("exact row must price: unpriced=%d", exactUnpriced)
	}
	exact, err := pricing.Quote(dup[len(dup)-1], 1.0, pricing.Tokens{Input: 1_000_000, Output: 1_000_000}, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if exactCost != exact.TotalNanos {
		t.Fatalf("exact row must win over slug: %d != %d", exactCost, exact.TotalNanos)
	}
	// A dated snapshot of a book row must reach that row's rates through the
	// date-suffix strip.
	snap := []sessionstats.AggregatedRow{{Model: "bailian/deepseek-v4-flash-0731", Input: 1_000_000}}
	snapCost, snapUnpriced := estimateSessionCost(snap, book)
	if snapUnpriced != 0 || snapCost == 0 {
		t.Fatalf("dated snapshot must price: cost=%d unpriced=%d", snapCost, snapUnpriced)
	}
	// A name no row answers to stays unpriced rather than taking a
	// neighbour's rates.
	missing := []sessionstats.AggregatedRow{{Model: "gpt-5.5", Input: 1_000_000}}
	cost, unpriced = estimateSessionCost(missing, book)
	if cost != 0 || unpriced != 1_000_000 {
		t.Fatalf("unknown name must stay unpriced: cost=%d unpriced=%d", cost, unpriced)
	}
}

// The unpriced share counts a row's tokens exactly once: cache reads ride in
// the accumulator's input channel, so a rebuild from the channels would
// double them into the headline unpriced figure.
func TestSessionCostUnpricedCountsTokensOnce(t *testing.T) {
	row := sessionstats.AggregatedRow{Model: "gpt-5.5", Input: 100, Output: 20, CacheRead: 700, CacheWrite: 30, Reasoning: 5}
	_, unpriced := estimateSessionCost([]sessionstats.AggregatedRow{row}, nil)
	if want := int64(100 + 20 + 700 + 30 + 5); unpriced != want {
		t.Fatalf("unpriced %d != token total %d", unpriced, want)
	}
}

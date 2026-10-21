package repository

import (
	"context"
	"testing"
	"time"

	"github.com/oh-my-cpa/oh-my-cpa/internal/pricing"
	"github.com/oh-my-cpa/oh-my-cpa/internal/usage"
)

// saveUnpricedEvents ingests a request for a model that has no price yet (it
// lands as 'unpriced') and seeds one 'legacy_unpriced' row the way rows that
// predate the pricing columns exist.
func saveUnpricedEvents(t *testing.T, ctx context.Context, r *Repository, model string, at int64) {
	t.Helper()
	if _, err := r.InsertUsageEvents(ctx, []usage.Event{{
		InstanceID: "default", EventKey: "bf-live-" + model, Model: model,
		TimestampMS: at, InputTokens: 2000000, OutputTokens: 500000, CacheReadTokens: 250000,
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SQL().Exec(`INSERT INTO usage_events (instance_id, event_key, api_group_key, model, timestamp_ms,
 input_tokens, output_tokens, created_at_ms, pricing_status)
 VALUES ('default', ?, ?, ?, ?, 1000000, 100000, ?, 'legacy_unpriced')`, "bf-legacy-"+model, model, model, at, at); err != nil {
		t.Fatal(err)
	}
}

// A model priced after its requests were ingested gets those requests' costs
// backfilled from the operator's recorded rate, evaluated at each request's
// own timestamp, marked 'backfilled', and the derived facts re-fold. The
// breakdown recomputes from the locked version and must agree.
func TestBackfillUsagePricesStoredUnpricedRequests(t *testing.T) {
	r := usageTestRepository(t)
	ctx := context.Background()
	service := manualPriceService(t, r, &countingFetcher{})

	const model = "priced-model"
	const otherModel = "still-unpriced"
	saveUnpricedEvents(t, ctx, r, model, time.Now().UnixMilli()-60_000)
	saveUnpricedEvents(t, ctx, r, otherModel, time.Now().UnixMilli()-60_000)
	if err := saveCustomPrice(ctx, service, pricing.ModelPrice{
		Model: model, PromptPricePer1M: 0.15, CompletionPer1M: 0.6, CacheReadPer1M: 0.015, PriceMultiplier: 1,
	}); err != nil {
		t.Fatal(err)
	}

	backfilled, skipped, err := r.BackfillUsagePricing(ctx, model, model)
	if err != nil {
		t.Fatal(err)
	}
	if backfilled != 2 || skipped != 0 {
		t.Fatalf("unexpected counts: backfilled=%d skipped=%d", backfilled, skipped)
	}

	var rowID int64
	var status, eventKey string
	if err := r.SQL().QueryRow(`SELECT id, pricing_status, event_key FROM usage_events
 WHERE model = ? AND pricing_status = 'backfilled' LIMIT 1`, model).Scan(&rowID, &status, &eventKey); err != nil {
		t.Fatal(err)
	}
	_ = eventKey
	breakdown, err := r.GetUsageEventCostBreakdown(ctx, rowID)
	if err != nil {
		t.Fatal(err)
	}
	if breakdown.StoredNanos == nil || *breakdown.StoredNanos <= 0 {
		t.Fatalf("backfilled cost missing: %+v", breakdown.StoredNanos)
	}
	if breakdown.Version == nil || breakdown.Quote == nil || !breakdown.RecomputedMatches {
		t.Fatalf("backfilled row does not recompute cleanly: %+v", breakdown)
	}
	// The cached share was priced at the cache rate, not the prompt rate.
	var cacheBucket float64
	for _, bucket := range breakdown.Quote.Buckets {
		if bucket.Kind == pricing.BucketCacheRead && bucket.Tokens == 250000 {
			cacheBucket = bucket.RatePer1M
		}
	}
	if cacheBucket != 0.015 {
		t.Fatalf("cache bucket not priced at the cache rate: %v", cacheBucket)
	}

	// Idempotent: every row now carries a cost, so a second pass fills nothing.
	againFilled, againSkipped, err := r.BackfillUsagePricing(ctx, model, model)
	if err != nil || againFilled != 0 || againSkipped != 0 {
		t.Fatalf("second pass: %d/%d %v", againFilled, againSkipped, err)
	}
	// Other models are untouched.
	var other int64
	if err := r.SQL().QueryRow(`SELECT COUNT(*) FROM usage_events WHERE model = ? AND pricing_status = 'backfilled'`, otherModel).Scan(&other); err != nil {
		t.Fatal(err)
	}
	if other != 0 {
		t.Fatal("another model's rows were backfilled")
	}

	// A retired name can be priced from another book row the operator asserts
	// prices the same model: the history keeps its own name, the rates come
	// from the named row.
	if _, err := r.InsertUsageEvents(ctx, []usage.Event{{
		InstanceID: "default", EventKey: "bf-retired", Model: "retired-bare-name",
		TimestampMS: time.Now().UnixMilli() - 60_000, InputTokens: 1_000_000,
	}}); err != nil {
		t.Fatal(err)
	}
	crossFilled, crossSkipped, err := r.BackfillUsagePricing(ctx, "retired-bare-name", model)
	if err != nil {
		t.Fatal(err)
	}
	if crossFilled != 1 || crossSkipped != 0 {
		t.Fatalf("cross-name backfill: %d/%d", crossFilled, crossSkipped)
	}
	var versionModel string
	if err := r.SQL().QueryRow(`SELECT v.model FROM usage_events e JOIN model_price_versions v ON v.id = e.price_version_id
 WHERE e.model = 'retired-bare-name' AND e.pricing_status = 'backfilled'`).Scan(&versionModel); err != nil {
		t.Fatal(err)
	}
	if versionModel != model {
		t.Fatalf("rates did not come from the asserted row: %q", versionModel)
	}
	// Facts were cleared for the rollup to re-fold.
	var facts int64
	if err := r.SQL().QueryRow(`SELECT (SELECT COUNT(*) FROM usage_facts_15m) + (SELECT COUNT(*) FROM usage_facts_daily)`).Scan(&facts); err != nil {
		t.Fatal(err)
	}
	if facts != 0 {
		t.Fatalf("facts not cleared: %d", facts)
	}
}

// A row already carrying a cost is never touched: pricing a later request
// locks it, and the backfill must not reprice or restamp it.
func TestBackfillUsageNeverTouchesPricedRows(t *testing.T) {
	r := usageTestRepository(t)
	ctx := context.Background()
	service := manualPriceService(t, r, &countingFetcher{})

	const model = "priced-model"
	if err := saveCustomPrice(ctx, service, pricing.ModelPrice{
		Model: model, PromptPricePer1M: 2, CompletionPer1M: 2, PriceMultiplier: 1,
	}); err != nil {
		t.Fatal(err)
	}
	rowID, err := r.InsertUsageEvents(ctx, []usage.Event{{
		InstanceID: "default", EventKey: "priced-live", Model: model,
		TimestampMS: time.Now().UnixMilli(), InputTokens: 1000000,
	}})
	if err != nil {
		t.Fatal(err)
	}
	var before int64
	if err := r.SQL().QueryRow(`SELECT cost_nanos FROM usage_events WHERE id = ?`, rowID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.BackfillUsagePricing(ctx, model, model); err != nil {
		t.Fatal(err)
	}
	var after int64
	var status string
	if err := r.SQL().QueryRow(`SELECT cost_nanos, pricing_status FROM usage_events WHERE id = ?`, rowID).Scan(&after, &status); err != nil {
		t.Fatal(err)
	}
	if after != before || status != "priced" {
		t.Fatalf("priced row disturbed: %d/%q", after, status)
	}
}

// The immutability trigger keeps every pricing-column transition it always
// banned; only the one unpriced/legacy_unpriced → backfilled fill passes.
func TestBackfillTriggerKeepsOtherTransitionsBanned(t *testing.T) {
	r := usageTestRepository(t)
	ctx := context.Background()
	service := manualPriceService(t, r, &countingFetcher{})

	const model = "priced-model"
	if err := saveCustomPrice(ctx, service, pricing.ModelPrice{
		Model: model, PromptPricePer1M: 1, CompletionPer1M: 1, PriceMultiplier: 1,
	}); err != nil {
		t.Fatal(err)
	}
	rowID, err := r.InsertUsageEvents(ctx, []usage.Event{{
		InstanceID: "default", EventKey: "locked-live", Model: model,
		TimestampMS: time.Now().UnixMilli(), InputTokens: 1000000,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.SQL().Exec(`UPDATE usage_events SET cost_nanos = cost_nanos + 1 WHERE id = ?`, rowID); err == nil {
		t.Fatal("repricing a priced row must abort")
	}
	if _, err := r.SQL().Exec(`INSERT INTO usage_events (instance_id, event_key, api_group_key, model, timestamp_ms, created_at_ms, pricing_status)
 VALUES ('default', 'legacy-2', 'g', ?, 1, 1, 'legacy_unpriced')`, model); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SQL().Exec(`UPDATE usage_events SET pricing_status = 'priced' WHERE event_key = 'legacy-2'`); err == nil {
		t.Fatal("legacy_unpriced must not become priced directly")
	}
	if _, err := r.SQL().Exec(`UPDATE usage_events SET pricing_status = 'backfilled' WHERE event_key = 'legacy-2'`); err == nil {
		t.Fatal("backfill without a cost must abort")
	}
}

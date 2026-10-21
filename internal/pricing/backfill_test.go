package pricing

import (
	"context"
	"errors"
	"testing"
)

func (f *fakeStore) BackfillUsagePricing(ctx context.Context, model string) (backfilled int64, skipped int64, err error) {
	f.backfillModel = model
	return f.backfilled, f.backfillSkipped, f.backfillErr
}

// BackfillUsage refuses a model the price book does not price — filling
// history needs a rate to fill from — and delegates priced models to the
// store under the price-write lock.
func TestBackfillUsageRequiresPricedModel(t *testing.T) {
	store := newFakeStore("minimax-m3.1-flash")
	store.prices = []ModelPrice{{Model: "minimax-m3.1-flash", PromptPricePer1M: 0.15, PriceMultiplier: 1, Source: SourceModelsDev}}
	store.backfilled, store.backfillSkipped = 7, 2
	service := NewService(store, &fakeFetcher{catalog: loadFixtureCatalog(t)}, nil)
	service.SetFallbackFetcher(&fakeFetcher{catalog: fallbackTestCatalog(t)})

	if _, _, err := service.BackfillUsage(context.Background(), ""); !errors.Is(err, ErrModelUnpriced) {
		t.Fatalf("empty model: %v", err)
	}
	if _, _, err := service.BackfillUsage(context.Background(), "unpriced-model"); !errors.Is(err, ErrModelUnpriced) {
		t.Fatalf("unpriced model must be refused: %v", err)
	}
	backfilled, skipped, err := service.BackfillUsage(context.Background(), "  minimax-m3.1-flash  ")
	if err != nil {
		t.Fatal(err)
	}
	if backfilled != 7 || skipped != 2 {
		t.Fatalf("unexpected counts: %d/%d", backfilled, skipped)
	}
	if store.backfillModel != "minimax-m3.1-flash" {
		t.Fatalf("model not trimmed: %q", store.backfillModel)
	}
}

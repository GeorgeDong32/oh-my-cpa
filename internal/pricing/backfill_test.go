package pricing

import (
	"context"
	"errors"
	"testing"
)

func (f *fakeStore) BackfillUsagePricing(ctx context.Context, model, priceModel string) (backfilled int64, skipped int64, err error) {
	f.backfillModel = model
	f.backfillPriceModel = priceModel
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

	if _, _, err := service.BackfillUsage(context.Background(), "", ""); !errors.Is(err, ErrModelUnpriced) {
		t.Fatalf("empty model: %v", err)
	}
	if _, _, err := service.BackfillUsage(context.Background(), "unpriced-model", ""); !errors.Is(err, ErrModelUnpriced) {
		t.Fatalf("unpriced model must be refused: %v", err)
	}
	backfilled, skipped, err := service.BackfillUsage(context.Background(), "  minimax-m3.1-flash  ", "")
	if err != nil {
		t.Fatal(err)
	}
	if backfilled != 7 || skipped != 2 {
		t.Fatalf("unexpected counts: %d/%d", backfilled, skipped)
	}
	if store.backfillModel != "minimax-m3.1-flash" || store.backfillPriceModel != "minimax-m3.1-flash" {
		t.Fatalf("names not resolved: %q / %q", store.backfillModel, store.backfillPriceModel)
	}
	// A retired name can be priced from another book row the operator asserts
	// prices the same model.
	if _, _, err := service.BackfillUsage(context.Background(), "old-bare-name", "minimax-m3.1-flash"); err != nil {
		t.Fatal(err)
	}
	if store.backfillModel != "old-bare-name" || store.backfillPriceModel != "minimax-m3.1-flash" {
		t.Fatalf("cross-name backfill lost the names: %q / %q", store.backfillModel, store.backfillPriceModel)
	}
}

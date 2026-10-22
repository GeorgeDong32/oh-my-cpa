package pricing

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func fallbackTestCatalog(t *testing.T) Catalog {
	t.Helper()
	return NewCatalog(decodeModelsDev(t), time.Unix(0, 0))
}

// primaryWithMinimax is an OpenRouter snapshot that now lists the model the
// fallback priced before: the moment the primary source catches up.
func primaryWithMinimax(t *testing.T) Catalog {
	t.Helper()
	base := loadFixtureCatalog(t).Models
	models := make([]UpstreamModel, 0, len(base)+1)
	models = append(models, base...)
	models = append(models, UpstreamModel{ID: "minimax/minimax-m3.1-flash", CanonicalSlug: "minimax/minimax-m3.1-flash",
		Name: "MiniMax M3.1 Flash", Author: "minimax", PromptPricePer1M: 0.2, CompletionPer1M: 0.8,
		CacheReadPer1M: 0.02, CacheWritePer1M: 0.2})
	return NewCatalog(models, time.Unix(0, 0))
}

// A model OpenRouter lists nothing for is priced from models.dev's first-party
// entry, and the row says so.
func TestSyncFallsBackToModelsDevWhenOpenRouterLacksModel(t *testing.T) {
	store := newFakeStore("claude-opus-5.5", "minimax-m3.1-flash")
	service := NewService(store, &fakeFetcher{catalog: loadFixtureCatalog(t)}, nil)
	service.SetFallbackFetcher(&fakeFetcher{catalog: fallbackTestCatalog(t)})
	result, err := service.SyncOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.FallbackMatched != 1 {
		t.Fatalf("fallback match not counted: %+v", result)
	}
	row, ok := store.price("minimax-m3.1-flash")
	if !ok {
		t.Fatal("fallback row not written")
	}
	if row.Source != SourceModelsDev || row.UpstreamID != "minimax/MiniMax-M3.1-Flash" || row.PromptPricePer1M != 0.15 {
		t.Fatalf("fallback row wrong: %+v", row)
	}
	if row.MatchKind != MatchExact {
		t.Fatalf("match kind not recorded: %+v", row)
	}
	if err := row.ValidateWrite(); err != nil {
		t.Fatalf("written row must validate: %v", err)
	}
	// The primary-matched model is untouched by the fallback.
	opus, _ := store.price("claude-opus-5.5")
	if opus.Source != SourceOpenRouter {
		t.Fatalf("primary row disturbed: %+v", opus)
	}
}

// When OpenRouter later lists the model, the next sync replaces the fallback
// row with the primary source's rate.
func TestSyncUpgradesFallbackRowWhenOpenRouterCatchesUp(t *testing.T) {
	store := newFakeStore("minimax-m3.1-flash")
	store.prices = []ModelPrice{{Model: "minimax-m3.1-flash", PromptPricePer1M: 0.15, CompletionPer1M: 0.6,
		CacheReadPer1M: 0.015, CacheWritePer1M: 0.15, PriceMultiplier: 1, Source: SourceModelsDev,
		UpstreamID: "minimax/MiniMax-M3.1-Flash", MatchKind: MatchNormalized}}
	service := NewService(store, &fakeFetcher{catalog: primaryWithMinimax(t)}, nil)
	service.SetFallbackFetcher(&fakeFetcher{catalog: fallbackTestCatalog(t)})
	result, err := service.SyncOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.FallbackMatched != 0 {
		t.Fatalf("fallback must not fire once OpenRouter lists the model: %+v", result)
	}
	row, _ := store.price("minimax-m3.1-flash")
	if row.Source != SourceOpenRouter || row.UpstreamID != "minimax/minimax-m3.1-flash" || row.PromptPricePer1M != 0.2 {
		t.Fatalf("fallback row not upgraded to the primary source: %+v", row)
	}
}

// An OpenRouter rate a sync can no longer find survives untouched: the
// fallback fills gaps, it never replaces a primary rate.
func TestSyncFallbackDoesNotReplaceOpenRouterRate(t *testing.T) {
	store := newFakeStore("vanished-model")
	store.prices = []ModelPrice{{Model: "vanished-model", PromptPricePer1M: 0.42, PriceMultiplier: 1,
		Source: SourceOpenRouter, UpstreamID: "maker/vanished-model", MatchKind: MatchExact}}
	service := NewService(store, &fakeFetcher{catalog: loadFixtureCatalog(t)}, nil)
	service.SetFallbackFetcher(&fakeFetcher{catalog: fallbackTestCatalog(t)})
	result, err := service.SyncOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.FallbackMatched != 0 {
		t.Fatalf("fallback replaced a primary rate: %+v", result)
	}
	row, _ := store.price("vanished-model")
	if row.Source != SourceOpenRouter || row.PromptPricePer1M != 0.42 {
		t.Fatalf("known rate disturbed: %+v", row)
	}
}

// An operator's pin names an OpenRouter model; a failed lookup is never
// reinterpreted as a fallback match.
func TestSyncFallbackSkipsLinkedModels(t *testing.T) {
	store := newFakeStore("pinned-model")
	store.links["pinned-model"] = "maker/pinned-model"
	service := NewService(store, &fakeFetcher{catalog: loadFixtureCatalog(t)}, nil)
	service.SetFallbackFetcher(&fakeFetcher{catalog: fallbackTestCatalog(t)})
	result, err := service.SyncOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.FallbackMatched != 0 {
		t.Fatalf("pin reinterpreted as a fallback match: %+v", result)
	}
	if _, ok := store.price("pinned-model"); ok {
		t.Fatal("row written for a broken pin")
	}
}

// The fallback is opportunistic: when models.dev is unreachable the sync still
// succeeds and simply prices nothing new.
func TestSyncFallbackFailureKeepsSyncSucceeding(t *testing.T) {
	store := newFakeStore("minimax-m3.1-flash")
	service := NewService(store, &fakeFetcher{catalog: loadFixtureCatalog(t)}, nil)
	service.SetFallbackFetcher(&fakeFetcher{err: errors.New("models.dev down")})
	result, err := service.SyncOnce(context.Background())
	if err != nil {
		t.Fatalf("fallback failure must not fail the sync: %v", err)
	}
	if result.Unmatched != 1 || result.FallbackMatched != 0 {
		t.Fatalf("unexpected result: %+v", result)
	}
	if _, ok := store.price("minimax-m3.1-flash"); ok {
		t.Fatal("row written from a failed fallback fetch")
	}
	if strings.Contains(store.state.LastError, "models.dev") {
		t.Fatalf("fallback failure misrecorded as a primary failure: %+v", store.state)
	}
}

// A catalog-only reconciliation prices a new model from the cached fallback
// without paying a fetch.
func TestCatalogOnlyReconcileUsesCachedFallback(t *testing.T) {
	store := newFakeStore("minimax-m3.1-flash")
	fallback := &fakeFetcher{catalog: fallbackTestCatalog(t)}
	service := NewService(store, &fakeFetcher{catalog: loadFixtureCatalog(t)}, nil)
	service.SetFallbackFetcher(fallback)
	if _, err := service.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	fetches := fallback.calls
	store.models["minimax-m2"] = "minimax-m2"
	if _, err := service.syncOnce(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if fallback.calls != fetches {
		t.Fatalf("catalog-only reconcile fetched the fallback: %d -> %d", fetches, fallback.calls)
	}
	row, ok := store.price("minimax-m2")
	if !ok || row.Source != SourceModelsDev {
		t.Fatalf("new model not priced from the cached fallback: %+v", row)
	}
}

// Session statistics carry the clients' own model spellings, names the
// gateway catalog never carried. Sync must price their gaps through the same
// fallback chain, and leave a name nobody carries honestly unpriced.
func TestSyncPricesSessionModelGaps(t *testing.T) {
	store := newFakeStore()
	service := NewService(store, &fakeFetcher{catalog: loadFixtureCatalog(t)}, nil)
	service.SetFallbackFetcher(&fakeFetcher{catalog: fallbackTestCatalog(t)})
	service.SetSessionModelNames(func(ctx context.Context) ([]string, error) {
		return []string{"Minimax/MiniMax-M3.1-Flash", "totally-unknown-model"}, nil
	})
	result, err := service.SyncOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	row, ok := store.price("Minimax/MiniMax-M3.1-Flash")
	if !ok {
		t.Fatal("session-side model not priced")
	}
	if row.Source != SourceModelsDev {
		t.Fatalf("session gap must ride the fallback: %+v", row)
	}
	if result.Unmatched < 1 {
		t.Fatalf("unknown session name must stay unmatched: %+v", result)
	}
}

// A failing session read must not fail the sync — it only means the pass
// prices nothing new beyond the catalog.
func TestSyncSurvivesSessionNameFailure(t *testing.T) {
	store := newFakeStore("claude-opus-5.5")
	service := NewService(store, &fakeFetcher{catalog: loadFixtureCatalog(t)}, nil)
	service.SetSessionModelNames(func(ctx context.Context) ([]string, error) {
		return nil, errors.New("session tables unavailable")
	})
	if _, err := service.SyncOnce(context.Background()); err != nil {
		t.Fatalf("sync must survive a session-name failure: %v", err)
	}
}

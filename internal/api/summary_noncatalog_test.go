package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/oh-my-cpa/oh-my-cpa/internal/config"
	"github.com/oh-my-cpa/oh-my-cpa/internal/crypto"
	"github.com/oh-my-cpa/oh-my-cpa/internal/pricing"
	"github.com/oh-my-cpa/oh-my-cpa/internal/repository"
)

// A session model whose price row exists but which the gateway catalog never
// carried must still price: the sessions estimate reads the whole book, not
// the catalog-filtered view.
func TestSummaryPricesNonCatalogModels(t *testing.T) {
	cipher, err := crypto.New("01234567890123456789012345678901")
	if err != nil {
		t.Fatal(err)
	}
	db, err := repository.Open(context.Background(), dashboardMemoryDSN("summary-noncatalog"), repository.WithCipher(cipher))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo := repository.New(db)
	pricingService := pricing.NewService(repo, nil, nil)

	seedSessionRows(t, repo, "codex", "sess-g55", "gpt-5.5", 1)
	buckets, err := repo.DirtyBuckets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.FoldBucketCache(context.Background(), buckets); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModelPrices(context.Background(), []pricing.ModelPrice{{
		Model: "gpt-5.5", PromptPricePer1M: 5, CompletionPer1M: 30,
		CacheReadPer1M: 0.5, PriceMultiplier: 1, Source: pricing.SourceOpenRouter,
		SyncedAtMS: time.Now().UnixMilli(), UpdatedAtMS: time.Now().UnixMilli(),
	}}); err != nil {
		t.Fatal(err)
	}

	handler := NewHandler(config.Config{BasePath: "/omc", Version: "test"}, repo, cipher, nil, nil)
	handler.SetPricing(pricingService)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/sessions/summary?days=today&tz=UTC", nil)
	handler.getSessionsSummary(rec, req)
	if rec.Code != 200 {
		t.Fatalf("summary status %d: %s", rec.Code, rec.Body.String()[:min(300, rec.Body.Len())])
	}
	var body struct {
		Models []struct {
			Model  string `json:"model"`
			Priced bool   `json:"priced"`
		} `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, row := range body.Models {
		if row.Model == "gpt-5.5" {
			if !row.Priced {
				t.Fatalf("gpt-5.5 must price from a plain book row")
			}
			return
		}
	}
	t.Fatalf("gpt-5.5 row missing from summary models")
}

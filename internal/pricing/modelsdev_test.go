package pricing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const modelsDevPayload = `{
  "minimax": {"models": {
    "MiniMax-M3.1-Flash": {"family": "minimax", "name": "MiniMax M3.1 Flash", "limit": {"context": 1000000},
      "cost": {"input": 0.15, "output": 0.6, "cache_read": 0.015}},
    "MiniMax-M3": {"family": "minimax", "name": "MiniMax M3", "limit": {"context": 280000},
      "cost": {"input": 0.3, "output": 1.2, "cache_read": 0.06,
        "tiers": [
          {"input": 0.6, "output": 2.4, "cache_read": 0.12, "tier": {"type": "context", "size": 512000}},
          {"input": 0.1, "output": 0.4, "tier": {"type": "batch"}}
        ],
        "context_over_200k": {"input": 0.9, "output": 3.6}}},
    "MiniMax-M2": {"family": "minimax", "name": "MiniMax M2", "limit": {"context": 204800},
      "cost": {"input": 0.3, "output": 1.2}},
    "MiniMax-Preview": {"family": "minimax", "name": "unpriced preview"}
  }},
  "minimax-cn": {"models": {
    "MiniMax-M3.1-Flash": {"family": "minimax", "name": "regional arm", "cost": {"input": 0.3137, "output": 1.2549}}
  }},
  "edenai": {"models": {
    "minimax/MiniMax-M3.1-Flash": {"family": "minimax", "name": "reseller", "cost": {"input": 0.15, "output": 0.6}}
  }},
  "alibaba": {"models": {
    "deepseek-v4-flash": {"family": "deepseek", "name": "cross-family hosting", "cost": {"input": 0.5, "output": 2}}
  }},
  "moonshotai": {"models": {
    "kimi-k9": {"name": "Kimi K9", "cost": {"input": 1, "output": 4}},
    "kimi-k3": {"family": "kimi-k3", "name": "Kimi K3", "cost": {"input": 3, "output": 15}}
  }},
  "zai": {"models": {"glm-5.3": {"family": "glm", "name": "GLM", "cost": {"input": 0.6, "output": 2.2}}}},
  "zhipuai": {"models": {"glm-5.3": {"family": "glm", "name": "GLM", "cost": {"input": 0.44, "output": 2.09}}}}
}`

func decodeModelsDev(t *testing.T) []UpstreamModel {
	t.Helper()
	models, err := DecodeModelsDev([]byte(modelsDevPayload))
	if err != nil {
		t.Fatal(err)
	}
	return models
}

func findModel(models []UpstreamModel, id string) (UpstreamModel, bool) {
	for _, model := range models {
		if model.ID == id {
			return model, true
		}
	}
	return UpstreamModel{}, false
}

func TestDecodeModelsDevKeepsFirstPartyEntries(t *testing.T) {
	models := decodeModelsDev(t)
	flash, ok := findModel(models, "minimax/MiniMax-M3.1-Flash")
	if !ok {
		t.Fatalf("first-party entry missing: %+v", ids(models))
	}
	if flash.PromptPricePer1M != 0.15 || flash.CompletionPer1M != 0.6 || flash.CacheReadPer1M != 0.015 {
		t.Fatalf("rates not carried over unscaled: %+v", flash)
	}
	if flash.CacheWritePer1M != 0.15 {
		t.Fatalf("missing cache write must resolve to the prompt rate: %+v", flash)
	}
	if flash.ContextLength != 1000000 || flash.Author != "minimax" {
		t.Fatalf("metadata lost: %+v", flash)
	}
	// The regional arm and every reseller namespace are skipped, and so is a
	// first-party namespace hosting another maker's model.
	for _, id := range []string{"minimax-cn/MiniMax-M3.1-Flash", "edenai/minimax/MiniMax-M3.1-Flash", "alibaba/deepseek-v4-flash", "minimax/MiniMax-Preview"} {
		if _, ok := findModel(models, id); ok {
			t.Fatalf("%s must not be priced: %+v", id, ids(models))
		}
	}
	// A source-declared family absent from the entry falls back to the name prefix.
	if _, ok := findModel(models, "moonshotai/kimi-k9"); !ok {
		t.Fatalf("prefix-derived family entry missing: %+v", ids(models))
	}
	if k3, ok := findModel(models, "moonshotai/kimi-k3"); !ok || k3.PromptPricePer1M != 3 {
		t.Fatalf("per-model declared family must not exclude the entry: %+v", k3)
	}
}

func ids(models []UpstreamModel) []string {
	out := make([]string, 0, len(models))
	for _, model := range models {
		out = append(out, model.ID)
	}
	return out
}

func TestDecodeModelsDevTiers(t *testing.T) {
	models := decodeModelsDev(t)
	m3, ok := findModel(models, "minimax/MiniMax-M3")
	if !ok {
		t.Fatal("MiniMax-M3 missing")
	}
	if len(m3.Tiers) != 1 {
		t.Fatalf("only the context tier is carried, legacy alias ignored: %+v", m3.Tiers)
	}
	tier := m3.Tiers[0]
	if tier.MinPromptTokens != 512000 || tier.PromptPricePer1M == nil || *tier.PromptPricePer1M != 0.6 {
		t.Fatalf("context tier not mapped: %+v", tier)
	}
	if tier.CacheReadPer1M == nil || *tier.CacheReadPer1M != 0.12 {
		t.Fatalf("tier cache read not mapped: %+v", tier)
	}
	if tier.CacheWritePer1M == nil || *tier.CacheWritePer1M != 0.6 {
		t.Fatalf("tier cache write must inherit the tier prompt rate when unpublished: %+v", tier)
	}
	m2, ok := findModel(models, "minimax/MiniMax-M2")
	if !ok {
		t.Fatal("MiniMax-M2 missing")
	}
	if m2.CacheReadPer1M != 0.3 || m2.CacheWritePer1M != 0.3 {
		t.Fatalf("omitted cache rates must resolve to the prompt rate: %+v", m2)
	}
}

func TestDecodeModelsDevLegacyContextOver200k(t *testing.T) {
	payload := `{"minimax": {"models": {"MiniMax-M3": {"family": "minimax", "cost": {"input": 0.3, "output": 1.2,
	  "context_over_200k": {"input": 0.6, "output": 2.4}}}}}}`
	models, err := DecodeModelsDev([]byte(payload))
	if err != nil {
		t.Fatal(err)
	}
	m3, ok := findModel(models, "minimax/MiniMax-M3")
	if !ok || len(m3.Tiers) != 1 {
		t.Fatalf("legacy long-context step not mapped: %+v", m3)
	}
	if m3.Tiers[0].MinPromptTokens != 200000 || *m3.Tiers[0].PromptPricePer1M != 0.6 {
		t.Fatalf("legacy tier wrong: %+v", m3.Tiers[0])
	}
}

func TestDecodeModelsDevRejectsBrokenInput(t *testing.T) {
	if _, err := DecodeModelsDev([]byte("not json")); err == nil {
		t.Fatal("expected decode error")
	}
	empty := `{"minimax": {"models": {}}}`
	models, err := DecodeModelsDev([]byte(empty))
	if err != nil || len(models) != 0 {
		t.Fatalf("empty catalogue is valid but prices nothing: %v %+v", err, models)
	}
}

func TestModelsDevFetch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(modelsDevPayload))
	}))
	t.Cleanup(server.Close)
	catalog, err := NewModelsDevClientWithTransport(server.Client().Transport, server.URL).Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Models) == 0 {
		t.Fatal("expected decoded models")
	}
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(failing.Close)
	if _, err := NewModelsDevClientWithTransport(failing.Client().Transport, failing.URL).Fetch(context.Background()); err == nil {
		t.Fatal("expected HTTP failure")
	}
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"minimax": {"models": {}}}`))
	}))
	t.Cleanup(empty.Close)
	if _, err := NewModelsDevClientWithTransport(empty.Client().Transport, empty.URL).Fetch(context.Background()); err == nil {
		t.Fatal("an empty first-party result is a broken source, not an empty price book")
	}
}

func TestMatchFallbackPrefersNamespaceOrder(t *testing.T) {
	catalog := NewCatalog(decodeModelsDev(t), time.Unix(0, 0))
	match, ok := matchFallback(catalog, "glm-5.3")
	if !ok {
		t.Fatal("expected a fallback match")
	}
	// zai ranks before zhipuai, mirroring z-ai before thudm on OpenRouter.
	if match.Model.ID != "zai/glm-5.3" || match.Model.PromptPricePer1M != 0.6 {
		t.Fatalf("namespace order not honoured: %+v", match.Model)
	}
	if _, ok := matchFallback(catalog, ""); ok {
		t.Fatal("empty target must not match")
	}
	if _, ok := matchFallback(Catalog{}, "glm-5.3"); ok {
		t.Fatal("empty catalog must not match")
	}
}

func TestValidateWriteAcceptsFallbackSource(t *testing.T) {
	row := ModelPrice{Model: "minimax-m3.1-flash", PromptPricePer1M: 0.15, CompletionPer1M: 0.6,
		PriceMultiplier: 1, Source: SourceModelsDev, UpstreamID: "minimax/MiniMax-M3.1-Flash", MatchKind: MatchNormalized}
	if err := row.ValidateWrite(); err != nil {
		t.Fatalf("fallback row rejected: %v", err)
	}
}

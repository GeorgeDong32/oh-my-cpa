package pricing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// models.dev is the fallback price source. OpenRouter stays primary: it is
// matched first and its rows win on every later sync. Only when OpenRouter
// lists nothing for a model does the sync consult models.dev — and there only
// entries under the maker's own namespace (see firstPartyModelsDevProviders),
// because the catalogue also lists every model once per relay and aggregator
// that resells it, and a reseller's rate must never become the list price.

// ModelsDevCatalogURL is the fixed fallback source. It is public, needs no
// key, and the client never accepts an operator-supplied URL.
const ModelsDevCatalogURL = "https://models.dev/api.json"

// firstPartyModelsDevProviders maps a model family to the models.dev
// namespaces that publish it first-hand, most authoritative first. The order
// mirrors firstPartyAuthors where both sources have the vendor: z.ai before
// the Zhipu platform, as z-ai ranks before thudm on OpenRouter. Families left
// out have no trustworthy first-hand namespace: models.dev's "llama" mixes
// hosting-variant ids (cerebras-, groq- prefixed), and the "-cn" and plan
// namespaces are regional arms of the same vendors.
var firstPartyModelsDevProviders = map[string][]string{
	"openai":    {"openai"},
	"anthropic": {"anthropic"},
	"google":    {"google"},
	"xai":       {"xai"},
	"glm":       {"zai", "zhipuai"},
	"qwen":      {"alibaba"},
	"moonshot":  {"moonshotai"},
	"deepseek":  {"deepseek"},
	"minimax":   {"minimax"},
	"xiaomi":    {"xiaomi"},
	"mistral":   {"mistral"},
	"cohere":    {"cohere"},
	"doubao":    {"volcengine"},
}

// matchFallback resolves a CPA target in the models.dev first-party catalog,
// ranking same-slug candidates by models.dev's own namespace order.
func matchFallback(catalog Catalog, target string) (Match, bool) {
	if len(catalog.Models) == 0 || target == "" {
		return Match{}, false
	}
	return catalog.matchModel(target, firstPartyRank(target, modelsDevAuthorities))
}

func modelsDevAuthorities(family string) []string { return firstPartyModelsDevProviders[family] }

// ModelsDevClient fetches and decodes models.dev's catalogue down to its
// first-party priced entries. Transport and URL are injectable for tests only;
// production uses the fixed HTTPS default.
type ModelsDevClient struct {
	client  *http.Client
	baseURL string
}

func NewModelsDevClient() *ModelsDevClient {
	return &ModelsDevClient{client: &http.Client{Timeout: upstreamTimeout}, baseURL: ModelsDevCatalogURL}
}

func NewModelsDevClientWithTransport(transport http.RoundTripper, baseURL string) *ModelsDevClient {
	return &ModelsDevClient{client: &http.Client{Timeout: upstreamTimeout, Transport: transport}, baseURL: baseURL}
}

// Fetch downloads the catalogue and returns its first-party priced entries as
// a lookup catalog. Any HTTP error, truncation or an empty first-party result
// is an error, and the caller keeps its last good prices.
func (c *ModelsDevClient) Fetch(ctx context.Context) (Catalog, error) {
	parsed, err := url.Parse(c.baseURL)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return Catalog{}, fmt.Errorf("pricing source URL must be http(s)")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL, nil)
	if err != nil {
		return Catalog{}, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "oh-my-cpa-pricing/3")
	response, err := c.client.Do(request)
	if err != nil {
		return Catalog{}, fmt.Errorf("fetch models.dev prices: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return Catalog{}, fmt.Errorf("fetch models.dev prices: status %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, upstreamMaxBytes+1))
	if err != nil {
		return Catalog{}, fmt.Errorf("read models.dev prices: %w", err)
	}
	if int64(len(body)) > upstreamMaxBytes {
		return Catalog{}, errors.New("models.dev price list exceeds size limit")
	}
	models, err := DecodeModelsDev(body)
	if err != nil {
		return Catalog{}, err
	}
	if len(models) == 0 {
		return Catalog{}, errors.New("models.dev returned no first-party priced models")
	}
	return NewCatalog(models, time.Now()), nil
}

type modelsDevProvider struct {
	Models map[string]modelsDevModel `json:"models"`
}

type modelsDevModel struct {
	Family string `json:"family"`
	Name   string `json:"name"`
	Limit  struct {
		Context json.RawMessage `json:"context"`
	} `json:"limit"`
	Cost *modelsDevCost `json:"cost"`
}

type modelsDevCost struct {
	Input      json.RawMessage `json:"input"`
	Output     json.RawMessage `json:"output"`
	CacheRead  json.RawMessage `json:"cache_read"`
	CacheWrite json.RawMessage `json:"cache_write"`
	Tiers      []struct {
		Input      json.RawMessage `json:"input"`
		Output     json.RawMessage `json:"output"`
		CacheRead  json.RawMessage `json:"cache_read"`
		CacheWrite json.RawMessage `json:"cache_write"`
		Tier       struct {
			Type string          `json:"type"`
			Size json.RawMessage `json:"size"`
		} `json:"tier"`
	} `json:"tiers"`
	// ContextOver200k is the pre-tiers spelling of the one long-context step
	// models.dev used to publish.
	ContextOver200k *struct {
		Input     json.RawMessage `json:"input"`
		Output    json.RawMessage `json:"output"`
		CacheRead json.RawMessage `json:"cache_read"`
	} `json:"context_over_200k"`
}

// DecodeModelsDev reduces models.dev's provider catalogue to first-party priced
// entries. Rates are already USD per 1M tokens, so they carry over unscaled.
//
// Kept: an entry whose source-declared family (or, failing that, name prefix)
// maps to the publishing provider's own namespace. Skipped: every relay,
// aggregator and regional arm; entries without a cost; and a cost without both
// a prompt and a completion rate.
//
// A cache rate models.dev omits resolves to the prompt rate, never to zero —
// the same rule OpenRouter decoding applies.
func DecodeModelsDev(body []byte) ([]UpstreamModel, error) {
	var envelope map[string]modelsDevProvider
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.UseNumber()
	if err := decoder.Decode(&envelope); err != nil {
		return nil, fmt.Errorf("decode models.dev prices: %w", err)
	}
	models := make([]UpstreamModel, 0, 256)
	for provider, payload := range envelope {
		if provider == "" || len(provider) > 64 {
			continue
		}
		for id, entry := range payload.Models {
			if id == "" || len(id) > 192 {
				continue
			}
			family := entry.Family
			if family == "" {
				family = modelFamilyOf(id)
			}
			officials := firstPartyModelsDevProviders[family]
			if !slices.Contains(officials, provider) {
				continue
			}
			if entry.Cost == nil {
				continue
			}
			prompt, promptOK := directRate(entry.Cost.Input)
			completion, completionOK := directRate(entry.Cost.Output)
			if !promptOK || !completionOK {
				continue
			}
			cacheRead, hasCacheRead := directRate(entry.Cost.CacheRead)
			if !hasCacheRead {
				cacheRead = prompt
			}
			cacheWrite, hasCacheWrite := directRate(entry.Cost.CacheWrite)
			if !hasCacheWrite {
				cacheWrite = prompt
			}
			model := UpstreamModel{
				ID: provider + "/" + id, CanonicalSlug: id, Name: strings.TrimSpace(entry.Name),
				Author: provider, ContextLength: max(nonNegativeNumber(entry.Limit.Context), 0),
				PromptPricePer1M: prompt, CompletionPer1M: completion, CacheReadPer1M: cacheRead, CacheWritePer1M: cacheWrite,
			}
			model.Tiers = CanonicalTiers(modelsDevTiers(entry.Cost, hasCacheRead, hasCacheWrite))
			if ValidateTiers(model.Tiers) != nil {
				model.Tiers = nil
			}
			models = append(models, model)
		}
	}
	return models, nil
}

// modelsDevTiers maps models.dev's conditional rates onto price tiers. Only
// context thresholds are carried: the request lock evaluates prompt thresholds
// and time-of-day windows, and models.dev publishes only the former. A tier
// cache rate that is absent inherits the tier's own prompt rate, so a doubled
// long-context prompt rate is not paired with the base-tier cache rate.
func modelsDevTiers(cost *modelsDevCost, hasCacheRead, hasCacheWrite bool) []PriceTier {
	var tiers []PriceTier
	for _, tier := range cost.Tiers {
		if tier.Tier.Type != "context" {
			continue
		}
		size, ok := integerField(tier.Tier.Size)
		if !ok || size <= 0 {
			continue
		}
		var mapped PriceTier
		mapped.MinPromptTokens = size
		mapped.PromptPricePer1M = optionalDirectRate(tier.Input)
		mapped.CompletionPer1M = optionalDirectRate(tier.Output)
		mapped.CacheReadPer1M = optionalDirectRate(tier.CacheRead)
		mapped.CacheWritePer1M = optionalDirectRate(tier.CacheWrite)
		if mapped.PromptPricePer1M != nil {
			if !hasCacheRead && mapped.CacheReadPer1M == nil {
				mapped.CacheReadPer1M = mapped.PromptPricePer1M
			}
			if !hasCacheWrite && mapped.CacheWritePer1M == nil {
				mapped.CacheWritePer1M = mapped.PromptPricePer1M
			}
		}
		tiers = append(tiers, mapped)
	}
	if len(tiers) > 0 || cost.ContextOver200k == nil {
		return tiers
	}
	var mapped PriceTier
	mapped.MinPromptTokens = 200_000
	mapped.PromptPricePer1M = optionalDirectRate(cost.ContextOver200k.Input)
	mapped.CompletionPer1M = optionalDirectRate(cost.ContextOver200k.Output)
	mapped.CacheReadPer1M = optionalDirectRate(cost.ContextOver200k.CacheRead)
	if mapped.PromptPricePer1M != nil && !hasCacheRead && mapped.CacheReadPer1M == nil {
		mapped.CacheReadPer1M = mapped.PromptPricePer1M
	}
	tiers = append(tiers, mapped)
	return tiers
}

// directRate reads a per-1M USD rate models.dev publishes as a JSON number.
// Negative, empty and non-numeric values are "no price".
func directRate(raw json.RawMessage) (float64, bool) {
	text := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if text == "" || text == "null" {
		return 0, false
	}
	value, ok := new(big.Rat).SetString(text)
	if !ok || value.Sign() < 0 {
		return 0, false
	}
	scaled, _ := value.Float64()
	return scaled, true
}

func optionalDirectRate(raw json.RawMessage) *float64 {
	value, ok := directRate(raw)
	if !ok {
		return nil
	}
	return &value
}

// nonNegativeNumber reads a JSON number, mapping anything unreadable to zero.
func nonNegativeNumber(raw json.RawMessage) int64 {
	value, ok := integerField(raw)
	if !ok || value < 0 {
		return 0
	}
	return value
}

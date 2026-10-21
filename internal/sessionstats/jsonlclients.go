package sessionstats

import (
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
)

// muxParser reads ~/.mux/sessions/<workspaceId>/session-usage.json — one
// cumulative usage snapshot per workspace session, with per-model buckets.
// Each model entry emits one row keyed on (session, model) so an incremental
// re-parse collapses the same entry instead of double-counting it.
type muxParser struct{}

type muxBucket struct {
	Tokens  *int64   `json:"tokens"`
	CostUSD *float64 `json:"cost_usd"`
}

type muxModelUsage struct {
	Input       *muxBucket `json:"input"`
	Cached      *muxBucket `json:"cached"`
	CacheCreate *muxBucket `json:"cacheCreate"`
	Output      *muxBucket `json:"output"`
	Reasoning   *muxBucket `json:"reasoning"`
}

type muxUsageFile struct {
	ByModel    map[string]muxModelUsage `json:"byModel"`
	LastUpdate struct {
		Timestamp *int64 `json:"timestamp"`
	} `json:"lastRequest"`
}

func muxTokens(bucket *muxBucket) int64 {
	if bucket == nil || bucket.Tokens == nil {
		return 0
	}
	return clampNonNeg(*bucket.Tokens)
}

func (muxParser) Client() string { return "mux" }

func (muxParser) Roots(home string) []string { return []string{home + "/.mux/sessions"} }

func (muxParser) Discover(root string) ([]string, error) {
	return WalkFilesWithSuffix(root, "session-usage.json")
}

func (muxParser) ParseFile(file string, mtimeMS int64, emit func(UnifiedEntry)) error {
	body, ok := readFileLimited(file, maxFileBytes)
	if !ok {
		return nil
	}
	var usage muxUsageFile
	if json.Unmarshal(body, &usage) != nil {
		return nil
	}
	timestamp := mtimeMS
	if usage.LastUpdate.Timestamp != nil && *usage.LastUpdate.Timestamp > 0 {
		timestamp = *usage.LastUpdate.Timestamp
	}
	session := dirBase(fileDir(file))
	// Deterministic order: the map iteration upstream used to mint positional
	// dedup keys was unstable across re-parses (its own comment says so); a
	// sorted walk plus a stable (session, model) key needs no positional luck.
	for _, modelKey := range sortedKeys(usage.ByModel) {
		modelUsage := usage.ByModel[modelKey]
		tokens := TokenBreakdown{
			Input: muxTokens(modelUsage.Input), Output: muxTokens(modelUsage.Output),
			CacheRead: muxTokens(modelUsage.Cached), CacheWrite: muxTokens(modelUsage.CacheCreate),
			Reasoning: muxTokens(modelUsage.Reasoning),
		}
		if tokens.Total() == 0 {
			continue
		}
		provider, model := "", modelKey
		if idx := strings.Index(modelKey, ":"); idx > 0 {
			provider, model = canonicalProvider(modelKey[:idx]), modelKey[idx+1:]
		}
		if provider == "" {
			provider = providerForModelOr(model, "mux")
		}
		emit(UnifiedEntry{
			Client: "mux", ModelID: model, ProviderID: provider, SessionID: session,
			TimestampMS: timestamp, Tokens: tokens,
			DedupKey: "mux:" + session + ":" + modelKey,
		})
	}
	return nil
}

func sortedKeys[V any](mapping map[string]V) []string {
	keys := make([]string, 0, len(mapping))
	for key := range mapping {
		keys = append(keys, key)
	}
	sortStrings(keys)
	return keys
}

// reasonixParser reads ~/.reasonix/stats/YYYY-MM-DD.jsonl — one JSON object
// per provider request, append-only and authoritative. Transcript JSONL is
// deliberately not scanned (it overlaps the stats).
type reasonixParser struct{}

type reasonixStat struct {
	TS         json.RawMessage `json:"ts"`
	Model      string          `json:"model"`
	Prompt     int64           `json:"prompt"`
	Completion int64           `json:"completion"`
	Reasoning  int64           `json:"reasoning"`
	CacheHit   int64           `json:"cache_hit"`
	CacheMiss  *int64          `json:"cache_miss"`
	Total      int64           `json:"total"`
	Requests   int64           `json:"requests"`
	Turn       bool            `json:"turn"`
}

func (reasonixParser) Client() string { return "reasonix" }

func (reasonixParser) Roots(home string) []string { return []string{home + "/.reasonix/stats"} }

func (reasonixParser) Discover(root string) ([]string, error) { return WalkJSONLFiles(root) }

func (reasonixParser) ParseFile(file string, mtimeMS int64, emit func(UnifiedEntry)) error {
	session := "reasonix-stats:" + file
	forEachJSONLineIndexed(file, mtimeMS, func(raw []byte, index int) {
		var record reasonixStat
		if json.Unmarshal(raw, &record) != nil {
			return
		}
		if record.Turn || strings.TrimSpace(record.Model) == "" ||
			(record.Total <= 0 && record.Requests <= 0) {
			return
		}
		timestamp, ok := parseTimestampRaw(record.TS)
		if !ok {
			return
		}
		provider, model := "reasonix", strings.TrimSpace(record.Model)
		if idx := strings.Index(model, "/"); idx > 0 {
			provider, model = canonicalProvider(model[:idx]), model[idx+1:]
		} else if inferred := providerForModel(model); inferred != "" {
			provider = inferred
		}
		cacheRead := clampNonNeg(record.CacheHit)
		// A nonzero cache miss is the authoritative ordinary-input bucket;
		// older records omit it and derive it from prompt minus cache hits.
		input := clampNonNeg(record.Prompt - cacheRead)
		if record.CacheMiss != nil && *record.CacheMiss != 0 {
			input = clampNonNeg(*record.CacheMiss)
		}
		reasoning := min64(clampNonNeg(record.Reasoning), clampNonNeg(record.Completion))
		tokens := TokenBreakdown{
			Input: input, Output: clampNonNeg(record.Completion) - reasoning,
			CacheRead: cacheRead, Reasoning: reasoning,
		}
		messages := clampNonNeg(record.Requests)
		if messages < 1 {
			messages = 1
		}
		emit(UnifiedEntry{
			Client: "reasonix", ModelID: model, ProviderID: provider, SessionID: session,
			TimestampMS: timestamp, Tokens: tokens, MessageCount: int32(messages),
			DedupKey: "reasonix:" + file + ":" + strconv.Itoa(index) + ":" + strconv.FormatInt(record.Requests, 10) + ":" + strconv.FormatInt(record.Total, 10),
		})
	})
	return nil
}

// mcodeParser reads Tokens-captured MiniMax Code headless streams
// (mcode exec --output-format stream-json): assistant usage buffers per turn
// and only a model-bearing exec.result flushes it — a partial stream without
// the result is ignored rather than priced against a guessed model.
type mcodeParser struct{}

type mcodePending struct {
	timestamp int64
	tokens    TokenBreakdown
}

func (mcodeParser) Client() string { return "mcode" }

func (mcodeParser) Roots(home string) []string {
	// The Tokens headless capture roots (upstream headless_roots).
	return []string{
		home + "/.config/tokens/headless/mcode",
		home + "/Library/Application Support/tokens/headless/mcode",
	}
}

func (mcodeParser) Discover(root string) ([]string, error) { return WalkJSONLFiles(root) }

func (mcodeParser) ParseFile(file string, mtimeMS int64, emit func(UnifiedEntry)) error {
	pending := map[string][]mcodePending{}
	forEachJSONLine(file, mtimeMS, func(raw []byte) {
		var value struct {
			Type    string `json:"type"`
			TurnID  string `json:"turnId"`
			Message *struct {
				Role      string `json:"role"`
				TurnID    string `json:"turnId"`
				Timestamp int64  `json:"timestamp"`
				Usage     *struct {
					InputTokens     *int64 `json:"inputTokens"`
					OutputTokens    *int64 `json:"outputTokens"`
					CacheReadTokens *int64 `json:"cacheReadTokens"`
					CacheWriteToken *int64 `json:"cacheWriteTokens"`
				} `json:"usage"`
			} `json:"message"`
			SessionID string `json:"sessionId"`
			Model     *struct {
				ProviderID string `json:"providerId"`
				ModelID    string `json:"modelId"`
			} `json:"model"`
		}
		if json.Unmarshal(raw, &value) != nil {
			return
		}
		switch value.Type {
		case "message":
			message := value.Message
			if message == nil || message.Role != "assistant" || message.Usage == nil {
				return
			}
			// The message event carries turnId inside message; exec.result
			// repeats it at the top level.
			turnID := strings.TrimSpace(message.TurnID)
			if turnID == "" {
				turnID = strings.TrimSpace(value.TurnID)
			}
			if turnID == "" {
				return
			}
			usage := message.Usage
			tokens := TokenBreakdown{
				Input: nonNeg(usage.InputTokens), Output: nonNeg(usage.OutputTokens),
				CacheRead: nonNeg(usage.CacheReadTokens), CacheWrite: nonNeg(usage.CacheWriteToken),
			}
			if tokens.Total() == 0 {
				return
			}
			timestamp := message.Timestamp
			if timestamp <= 0 {
				timestamp = mtimeMS
			}
			pending[turnID] = append(pending[turnID], mcodePending{timestamp: timestamp, tokens: tokens})
		case "exec.result":
			turnID := strings.TrimSpace(value.TurnID)
			sessionID := strings.TrimSpace(value.SessionID)
			if turnID == "" || sessionID == "" || value.Model == nil {
				return
			}
			provider := strings.TrimSpace(value.Model.ProviderID)
			model := strings.TrimSpace(value.Model.ModelID)
			if provider == "" || model == "" {
				return
			}
			usage := pending[turnID]
			delete(pending, turnID)
			for index, item := range usage {
				emit(UnifiedEntry{
					Client: "mcode", ModelID: model, ProviderID: provider, SessionID: sessionID,
					TimestampMS: item.timestamp, Tokens: item.tokens,
					DedupKey: "mcode:" + sessionID + ":" + turnID + ":" + itoa(index) + ":" +
						strconv.FormatInt(item.tokens.Input, 10) + ":" + strconv.FormatInt(item.tokens.Output, 10) + ":" +
						strconv.FormatInt(item.tokens.CacheRead, 10) + ":" + strconv.FormatInt(item.tokens.CacheWrite, 10),
				})
			}
		}
	})
	return nil
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// parseTimestampRaw accepts either an RFC3339 string or a raw epoch
// milliseconds number.
func parseTimestampRaw(raw json.RawMessage) (int64, bool) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return 0, false
	}
	if trimmed[0] == '"' {
		var text string
		if json.Unmarshal(raw, &text) != nil {
			return 0, false
		}
		if ts, ok := ParseRFC3339MS(text); ok {
			return ts, true
		}
		return 0, false
	}
	var number int64
	if json.Unmarshal(raw, &number) != nil || number <= 0 {
		return 0, false
	}
	return number, true
}

// gjcParser reads ~/.gjc/agent/sessions/<project-slug>/*.jsonl — a pi-like
// header plus assistant messages with Anthropic-style usage.
type gjcParser struct{}

type gjcUsage struct {
	InputTokens  *int64 `json:"input_tokens"`
	OutputTokens *int64 `json:"output_tokens"`
	CacheRead    *int64 `json:"cache_read_input_tokens"`
	CacheWrite   *int64 `json:"cache_creation_input_tokens"`
}

func (u *gjcUsage) breakdown() (TokenBreakdown, bool) {
	if u == nil {
		return TokenBreakdown{}, false
	}
	tokens := TokenBreakdown{
		Input: nonNeg(u.InputTokens), Output: nonNeg(u.OutputTokens),
		CacheRead: nonNeg(u.CacheRead), CacheWrite: nonNeg(u.CacheWrite),
	}
	return tokens, tokens.Total() > 0
}

func (gjcParser) Client() string { return "gjc" }

func (gjcParser) Roots(home string) []string { return []string{home + "/.gjc/agent/sessions"} }

func (gjcParser) Discover(root string) ([]string, error) { return WalkJSONLFiles(root) }

func (gjcParser) ParseFile(file string, mtimeMS int64, emit func(UnifiedEntry)) error {
	var session, wsKey, wsLabel string
	forEachJSONLine(file, mtimeMS, func(raw []byte) {
		var entry struct {
			Type      string  `json:"type"`
			ID        *string `json:"id"`
			Timestamp string  `json:"timestamp"`
			CWD       string  `json:"cwd"`
			Message   *struct {
				Role      string    `json:"role"`
				Model     string    `json:"model"`
				Provider  string    `json:"provider"`
				Timestamp *int64    `json:"timestamp"`
				Usage     *gjcUsage `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal(raw, &entry) != nil {
			return
		}
		if entry.Type == "session" {
			if entry.ID != nil && strings.TrimSpace(*entry.ID) != "" {
				session = strings.TrimSpace(*entry.ID)
			}
			if key := NormalizeWorkspaceKey(entry.CWD); key != "" {
				wsKey, wsLabel = key, WorkspaceLabelFromKey(key)
			}
			return
		}
		if entry.Type != "message" || entry.Message == nil || entry.Message.Role != "assistant" {
			return
		}
		tokens, ok := entry.Message.Usage.breakdown()
		if !ok {
			return
		}
		model := strings.TrimSpace(entry.Message.Model)
		if model == "" {
			return
		}
		provider := strings.TrimSpace(entry.Message.Provider)
		if provider == "" {
			provider = providerForModel(model)
		}
		if provider == "" {
			provider = "gjc"
		}
		ts := mtimeMS
		if entry.Message.Timestamp != nil && *entry.Message.Timestamp > 0 {
			ts = *entry.Message.Timestamp
		} else if parsed, valid := ParseRFC3339MS(entry.Timestamp); valid {
			ts = parsed
		}
		if session == "" {
			session = strings.TrimSuffix(filepath.Base(file), ".jsonl")
		}
		key := "gjc:" + file + ":" + session
		if entry.ID != nil && strings.TrimSpace(*entry.ID) != "" {
			key = session + ":" + strings.TrimSpace(*entry.ID)
		}
		emit(UnifiedEntry{
			Client: "gjc", ModelID: model, ProviderID: provider, SessionID: session,
			WorkspaceKey: wsKey, WorkspaceLabel: wsLabel, TimestampMS: ts, Tokens: tokens,
			DedupKey: key,
		})
	})
	return nil
}

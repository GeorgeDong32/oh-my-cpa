package sessionstats

// Gemini CLI: ~/.gemini/tmp/<id>/chats/*.json session recordings and
// headless *.jsonl transcripts (sessions/gemini.rs). Token fields arrive
// under several aliases; prompt counts are cache-inclusive upstream and are
// split here into the console's mutually exclusive buckets.

import (
	"encoding/json"
	"path/filepath"
	"strings"
)

type geminiParser struct{}

func (geminiParser) Client() string { return "gemini" }

func (p geminiParser) Roots(home string) []string {
	return []string{filepath.Join(home, ".gemini", "tmp")}
}

func (p geminiParser) Discover(root string) ([]string, error) {
	// Both chats/*.json recordings and *.jsonl headless transcripts.
	jsonFiles, err := WalkFilesWithSuffix(root, ".json")
	if err != nil {
		return nil, err
	}
	jsonlFiles, err := WalkJSONLFiles(root)
	if err != nil {
		return nil, err
	}
	return append(jsonFiles, jsonlFiles...), nil
}

type geminiSession struct {
	SessionID string          `json:"sessionId"`
	Messages  []geminiMessage `json:"messages"`
}

type geminiMessage struct {
	ID        string          `json:"id"`
	Timestamp string          `json:"timestamp"`
	Tokens    json.RawMessage `json:"tokens"`
	Model     string          `json:"model"`
}

type geminiTokens struct {
	Input    *int64 `json:"input"`
	Output   *int64 `json:"output"`
	Cached   *int64 `json:"cached"`
	Thoughts *int64 `json:"thoughts"`
	Tool     *int64 `json:"tool"`
	Total    *int64 `json:"total"`
}

func firstI64(value map[string]any, keys ...string) *int64 {
	for _, key := range keys {
		switch raw := value[key].(type) {
		case float64:
			v := int64(raw)
			return &v
		}
	}
	return nil
}

func geminiTokensFrom(value json.RawMessage) *geminiTokens {
	var generic map[string]any
	if json.Unmarshal(value, &generic) != nil {
		return nil
	}
	return &geminiTokens{
		Input:    firstI64(generic, "input", "prompt", "input_tokens", "prompt_tokens", "promptTokenCount"),
		Output:   firstI64(generic, "output", "candidates", "output_tokens", "completion_tokens", "candidatesTokenCount"),
		Cached:   firstI64(generic, "cached", "cached_tokens", "cachedContentTokenCount"),
		Thoughts: firstI64(generic, "thoughts", "reasoning", "thoughts_tokens"),
		Tool:     firstI64(generic, "tool", "tool_tokens"),
		Total:    firstI64(generic, "total", "totalTokenCount", "total_tokens"),
	}
}

// subtractCachedOverlap splits a cache-inclusive input count into the
// console's exclusive buckets: uncached input and the cached share.
func subtractCachedOverlap(input, cached int64) (int64, int64) {
	if input < 0 {
		input = 0
	}
	if cached < 0 {
		cached = 0
	}
	cachedPortion := cached
	if cachedPortion > input {
		cachedPortion = input
	}
	return input - cachedPortion, cached
}

func (p geminiParser) ParseFile(file string, mtimeMS int64, emit func(UnifiedEntry)) error {
	body, ok := readFileLimited(file, maxFileBytes)
	if !ok {
		return nil
	}
	fallbackSession := strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))
	if strings.HasSuffix(file, ".jsonl") {
		parseGeminiHeadless(body, fallbackSession, mtimeMS, emit)
		return nil
	}
	// Session recording: a single JSON document.
	var session geminiSession
	if json.Unmarshal(body, &session) == nil && session.SessionID != "" && len(session.Messages) > 0 {
		for index, message := range session.Messages {
			tokens := geminiTokensFrom(message.Tokens)
			if tokens == nil || strings.TrimSpace(message.Model) == "" {
				continue
			}
			ts := mtimeMS
			if parsed, valid := ParseRFC3339MS(message.Timestamp); valid {
				ts = parsed
			}
			input, cached := subtractCachedOverlap(nonNeg(tokens.Input), nonNeg(tokens.Cached))
			breakdown := TokenBreakdown{
				Input: input, Output: nonNeg(tokens.Output), CacheRead: cached,
				Reasoning: nonNeg(tokens.Thoughts) + nonNeg(tokens.Tool),
			}
			if breakdown.Total() == 0 {
				continue
			}
			emit(UnifiedEntry{
				Client: p.Client(), ModelID: message.Model, ProviderID: "google",
				SessionID: session.SessionID, TimestampMS: ts, Tokens: breakdown,
				DedupKey: "gemini:" + session.SessionID + ":" + message.ID,
			})
			_ = index
		}
		return nil
	}
	// A single headless JSON document also occurs.
	parseGeminiHeadless(body, fallbackSession, mtimeMS, emit)
	return nil
}

// parseGeminiHeadless walks JSONL (or a single JSON document's lines) with the
// upstream's state: init line carries model/session, later lines with tokens
// emit, id-keyed lines replace in place.
func parseGeminiHeadless(body []byte, fallbackSession string, mtimeMS int64, emit func(UnifiedEntry)) {
	sessionID := fallbackSession
	currentModel := ""
	index := 0
	dec := json.NewDecoder(strings.NewReader(string(body)))
	for dec.More() {
		var value map[string]any
		if dec.Decode(&value) != nil {
			continue
		}
		eventType, _ := value["type"].(string)
		if id, ok := stringValue(value, "session_id"); ok {
			sessionID = id
		} else if id, ok := stringValue(value, "sessionId"); ok {
			sessionID = id
		}
		if eventType == "init" {
			if model, ok := stringValue(value, "model"); ok {
				currentModel = model
			}
			continue
		}
		rawTokens, hasTokens := value["tokens"]
		if eventType != "gemini" && !hasTokens {
			continue
		}
		if model, ok := stringValue(value, "model"); ok {
			currentModel = model
		}
		tokensRaw, _ := json.Marshal(rawTokens)
		tokens := geminiTokensFrom(tokensRaw)
		if tokens == nil {
			continue
		}
		model := currentModel
		if m, ok := stringValue(value, "model"); ok {
			model = m
		}
		input, cached := subtractCachedOverlap(nonNeg(tokens.Input), nonNeg(tokens.Cached))
		breakdown := TokenBreakdown{
			Input: input, Output: nonNeg(tokens.Output), CacheRead: cached,
			Reasoning: nonNeg(tokens.Thoughts) + nonNeg(tokens.Tool),
		}
		if model == "" || breakdown.Total() == 0 {
			continue
		}
		ts := mtimeMS
		if raw, ok := value["timestamp"].(string); ok {
			if parsed, valid := ParseRFC3339MS(raw); valid {
				ts = parsed
			}
		}
		emit(UnifiedEntry{
			Client: "gemini", ModelID: model, ProviderID: "google",
			SessionID: sessionID, TimestampMS: ts, Tokens: breakdown,
			DedupKey: "gemini:" + sessionID + ":" + itoa(index),
		})
		index++
	}
}

func stringValue(value map[string]any, key string) (string, bool) {
	raw, ok := value[key].(string)
	return raw, ok && strings.TrimSpace(raw) != ""
}

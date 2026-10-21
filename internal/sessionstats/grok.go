package sessionstats

import (
	"encoding/json"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
)

// grokParser reads Grok Build's two usage stores:
//
//   - session update streams ~/.grok/sessions/<ws>/<id>/updates.jsonl, where
//     authoritative per-call usage rows (params.update.usage) win and the
//     cumulative token counters become per-turn deltas as a fallback;
//   - the append-only unified log ~/.grok/logs/unified.jsonl, whose
//     shell.turn.inference_done records report a cache-inclusive prompt total
//     and a reasoning-inclusive completion total.
type grokParser struct{}

func (grokParser) Client() string { return "grok" }

// Roots registers both stores: the sessions tree and the sibling logs dir
// (upstream derives the logs root from the Grok home; both stay registered).
func (grokParser) Roots(home string) []string {
	return []string{home + "/.grok/sessions", home + "/.grok/logs"}
}

func (grokParser) Discover(root string) ([]string, error) {
	if filepath.Base(filepath.Clean(root)) == "logs" {
		return WalkFilesWithSuffix(root, "unified.jsonl")
	}
	return WalkFilesWithSuffix(root, "updates.jsonl")
}

// grokMetadata carries what the session directory itself says: the session id
// is the parent dir name and the workspace the (percent-decoded) grandparent.
type grokMetadata struct {
	sessionID    string
	modelID      string
	timestamp    int64
	workspaceKey string
	workspaceLbl string
}

func readGrokMetadata(file string, mtimeMS int64) grokMetadata {
	sessionDir := fileDir(file)
	metadata := grokMetadata{
		sessionID:    dirBase(sessionDir),
		timestamp:    mtimeMS,
		workspaceKey: NormalizeWorkspaceKey(percentDecode(dirBase(fileDir(sessionDir)))),
	}
	if metadata.workspaceKey != "" {
		metadata.workspaceLbl = WorkspaceLabelFromKey(metadata.workspaceKey)
	}
	if summary, ok := readFileLimited(sessionDir+"/summary.json", maxFileBytes); ok {
		var value struct {
			Model          string `json:"model"`
			CurrentModelID string `json:"current_model_id"`
			ModelID        string `json:"model_id"`
		}
		if json.Unmarshal(summary, &value) == nil {
			for _, candidate := range []string{value.CurrentModelID, value.ModelID, value.Model} {
				if strings.TrimSpace(candidate) != "" {
					metadata.modelID = strings.TrimSpace(candidate)
					break
				}
			}
		}
	}
	return metadata
}

func percentDecode(value string) string {
	if decoded, err := url.QueryUnescape(value); err == nil {
		return decoded
	}
	return value
}

func grokPathAny(value map[string]any, path ...string) (any, bool) {
	current := any(value)
	for _, segment := range path {
		mapping, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = mapping[segment]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func grokPathI64(value map[string]any, path ...string) (int64, bool) {
	raw, ok := grokPathAny(value, path...)
	if !ok {
		return 0, false
	}
	switch number := raw.(type) {
	case float64:
		return int64(number), true
	case json.Number:
		parsed, err := number.Int64()
		return parsed, err == nil
	}
	return 0, false
}

func grokPathString(value map[string]any, path ...string) (string, bool) {
	raw, ok := grokPathAny(value, path...)
	if !ok {
		return "", false
	}
	text, ok := raw.(string)
	return text, ok
}

// extractGrokModelID walks the model-id paths in upstream priority order.
func extractGrokModelID(value map[string]any) string {
	for _, path := range [][]string{
		{"params", "update", "_meta", "modelId"},
		{"params", "_meta", "modelId"},
		{"params", "modelId"},
		{"model_id"},
		{"modelId"},
		{"model"},
	} {
		if model, ok := grokPathString(value, path...); ok && strings.TrimSpace(model) != "" {
			return strings.TrimSpace(model)
		}
	}
	return ""
}

// extractGrokTotalTokens walks the cumulative-counter paths in upstream order.
func extractGrokTotalTokens(value map[string]any) (int64, bool) {
	for _, path := range [][]string{
		{"params", "_meta", "totalTokens"},
		{"params", "update", "_meta", "totalTokens"},
		{"params", "update", "totalTokens"},
		{"params", "totalTokens"},
		{"usage", "totalTokens"},
		{"totalTokens"},
	} {
		if total, ok := grokPathI64(value, path...); ok {
			return total, true
		}
	}
	return 0, false
}

func extractGrokTimestampMS(value map[string]any) int64 {
	for _, path := range [][]string{
		{"params", "_meta", "agentTimestampMs"},
		{"params", "update", "_meta", "agentTimestampMs"},
		{"params", "_meta", "timestampMs"},
		{"params", "update", "_meta", "timestampMs"},
		{"params", "_meta", "timestamp"},
		{"timestamp"},
	} {
		if ts, ok := grokPathI64(value, path...); ok && ts > 0 {
			return ts
		}
	}
	return 0
}

// grokUsageFromUpdate extracts params.update.usage, splitting cache out of a
// cache-inclusive input and reasoning out of an inclusive output when the
// reported total proves inclusivity.
func grokUsageFromUpdate(value map[string]any) (TokenBreakdown, bool) {
	usage, ok := grokPathAny(value, "params", "update", "usage")
	if !ok {
		return TokenBreakdown{}, false
	}
	mapping, ok := usage.(map[string]any)
	if !ok {
		return TokenBreakdown{}, false
	}
	number := func(keys ...string) int64 {
		for _, key := range keys {
			if raw, exists := mapping[key]; exists {
				if floated, ok := raw.(float64); ok && floated >= 0 {
					return int64(floated)
				}
			}
		}
		return 0
	}
	input := number("inputTokens", "input_tokens", "promptTokens")
	output := number("outputTokens", "output_tokens", "completionTokens")
	cacheRead := number("cachedReadTokens", "cacheReadTokens", "cache_read_input_tokens")
	cacheWrite := number("cachedWriteTokens", "cacheWriteTokens", "cacheCreationTokens", "cache_creation_input_tokens")
	reasoning := number("reasoningTokens", "thoughtTokens", "thinkingTokens")
	if input == 0 && output == 0 && cacheRead == 0 && cacheWrite == 0 && reasoning == 0 {
		return TokenBreakdown{}, false
	}
	total := number("totalTokens", "total_tokens")
	inclusiveTotal := input + output
	inclusive := total == 0 || total == inclusiveTotal
	if inclusive {
		input = clampNonNeg(input - cacheRead)
		output = clampNonNeg(output - reasoning)
	}
	tokens := TokenBreakdown{Input: input, Output: output, CacheRead: cacheRead, CacheWrite: cacheWrite, Reasoning: reasoning}
	return tokens, tokens.Total() > 0
}

// grokActiveTurn accumulates one user-turn window of cumulative-counter deltas.
type grokActiveTurn struct {
	baseline int64
	maxTotal int64
	ts       int64
	model    string
	index    int
}

func (t *grokActiveTurn) intoEntry(metadata grokMetadata, dedupNamespace string) (UnifiedEntry, bool) {
	delta := t.maxTotal - t.baseline
	if delta <= 0 {
		return UnifiedEntry{}, false
	}
	model := t.model
	if model == "" || model == "grok-unknown" {
		model = metadata.modelID
	}
	if model == "" {
		model = "grok-unknown"
	}
	return UnifiedEntry{
		Client: "grok", ModelID: model, ProviderID: "xai",
		SessionID: metadata.sessionID, WorkspaceKey: metadata.workspaceKey,
		WorkspaceLabel: metadata.workspaceLbl, TimestampMS: t.ts,
		Tokens: TokenBreakdown{Input: delta}, IsTurnStart: true,
		DedupKey: "grok:" + metadata.sessionID + ":" + dedupNamespace + ":" + strconv.Itoa(t.index),
	}, true
}

func (grokParser) ParseFile(file string, mtimeMS int64, emit func(UnifiedEntry)) error {
	if filepath.Base(file) == "unified.jsonl" {
		parseGrokUnifiedLog(file, mtimeMS, emit)
		return nil
	}
	if filepath.Base(file) != "updates.jsonl" {
		return nil
	}
	parseGrokUpdates(file, mtimeMS, emit)
	return nil
}

// parseGrokUpdates: authoritative usage rows first; cumulative counters
// become per-turn (user-message bounded) deltas, kept only when newer than
// the latest authoritative record.
func parseGrokUpdates(file string, mtimeMS int64, emit func(UnifiedEntry)) {
	metadata := readGrokMetadata(file, mtimeMS)
	var usageRows []UnifiedEntry
	var fallback []UnifiedEntry
	currentModel := ""
	if metadata.modelID != "" {
		currentModel = metadata.modelID
	}
	var lastTotal *int64
	var active *grokActiveTurn
	turnIndex := 0
	usageIndex := 0

	forEachJSONLine(file, mtimeMS, func(raw []byte) {
		var value map[string]any
		if json.Unmarshal(raw, &value) != nil {
			return
		}
		if model := extractGrokModelID(value); model != "" {
			currentModel = model
			if active != nil && (active.model == "" || active.model == "grok-unknown") {
				active.model = currentModel
			}
		}
		ts := extractGrokTimestampMS(value)
		if ts == 0 {
			ts = metadata.timestamp
		}
		// A user-message chunk opens a new turn: flush the previous one.
		if sessionUpdate, _ := grokPathString(value, "params", "update", "sessionUpdate"); sessionUpdate == "user_message_chunk" {
			if active != nil {
				if entry, ok := active.intoEntry(metadata, "counter"); ok {
					fallback = append(fallback, entry)
				}
				active = nil
			}
			baseline := int64(0)
			if lastTotal != nil {
				baseline = *lastTotal
			}
			active = &grokActiveTurn{baseline: baseline, ts: ts, model: currentModel, index: turnIndex}
			turnIndex++
		}
		if usage, ok := grokUsageFromUpdate(value); ok {
			model := currentModel
			if model == "" || model == "grok-unknown" {
				if models, ok := grokPathAny(value, "params", "update", "usage", "modelUsage"); ok {
					if mapping, ok := models.(map[string]any); ok && len(mapping) == 1 {
						for key := range mapping {
							model = key
						}
					}
				}
			}
			if model == "" {
				model = "grok-unknown"
			}
			eventID := ""
			if id, ok := grokPathString(value, "params", "_meta", "eventId"); ok {
				eventID = strings.TrimSpace(id)
			}
			// eventId is not unique per record — the index keeps same-id
			// records distinct, and the session scopes the key for the
			// cross-file merge.
			usageRows = append(usageRows, UnifiedEntry{
				Client: "grok", ModelID: model, ProviderID: "xai",
				SessionID: metadata.sessionID, WorkspaceKey: metadata.workspaceKey,
				WorkspaceLabel: metadata.workspaceLbl, TimestampMS: ts, Tokens: usage,
				DedupKey: "grok:" + metadata.sessionID + ":usage:" + strconv.Itoa(usageIndex) + ":" + eventID,
			})
			usageIndex++
			// Fall through: the same update can also advance the cumulative
			// counter (upstream handles both in one pass).
		}
		total, hasTotal := extractGrokTotalTokens(value)
		if !hasTotal || total < 0 {
			return
		}
		if lastTotal != nil && total < *lastTotal {
			return
		}
		if lastTotal == nil && active == nil {
			// First observation only records the baseline (upstream's None
			// arm): a session that started mid-stream must not bill its
			// pre-existing cumulative total as this session's delta.
			lastTotal = &total
			return
		}
		if active == nil {
			baseline := int64(0)
			if lastTotal != nil {
				baseline = *lastTotal
			}
			active = &grokActiveTurn{baseline: baseline, ts: ts, model: currentModel, index: turnIndex}
			turnIndex++
		}
		if total > active.maxTotal {
			active.maxTotal = total
			active.ts = ts
		}
		lastTotal = &total
	})
	if active != nil {
		if entry, ok := active.intoEntry(metadata, "counter"); ok {
			fallback = append(fallback, entry)
		}
	}
	if len(usageRows) == 0 {
		for _, row := range fallback {
			emit(row)
		}
		return
	}
	latest := int64(0)
	for _, row := range usageRows {
		if row.TimestampMS > latest {
			latest = row.TimestampMS
		}
	}
	for _, row := range usageRows {
		emit(row)
	}
	for _, row := range fallback {
		if row.TimestampMS > latest {
			emit(row)
		}
	}
}

// parseGrokUnifiedLog reads `{msg:"shell.turn.inference_done", sid, ctx:{...},
// pid, ts}` records; prompt totals include cached tokens and completion totals
// include reasoning, so both split out for additive buckets.
func parseGrokUnifiedLog(file string, mtimeMS int64, emit func(UnifiedEntry)) {
	modelsBySession := map[string]string{}
	var records []map[string]any
	forEachJSONLine(file, mtimeMS, func(raw []byte) {
		var value map[string]any
		if json.Unmarshal(raw, &value) != nil {
			return
		}
		records = append(records, value)
		msg, _ := value["msg"].(string)
		// Model bookkeeping records carry the model per session; a later
		// inference row attributes to the most recent one.
		if msg == "model changed" || msg == "subagent model resolved" {
			if sid, _ := value["sid"].(string); strings.TrimSpace(sid) != "" {
				if ctx, ok := value["ctx"].(map[string]any); ok {
					for _, key := range []string{"model", "model_id", "current_model_id"} {
						if model, _ := ctx[key].(string); strings.TrimSpace(model) != "" && strings.TrimSpace(model) != "grok-unknown" {
							modelsBySession[strings.TrimSpace(sid)] = strings.TrimSpace(model)
							break
						}
					}
				}
			}
		}
	})
	seen := map[string]struct{}{}
	for _, value := range records {
		msg, _ := value["msg"].(string)
		if msg != "shell.turn.inference_done" {
			continue
		}
		session, _ := value["sid"].(string)
		session = strings.TrimSpace(session)
		if session == "" {
			continue
		}
		context, ok := value["ctx"].(map[string]any)
		if !ok {
			continue
		}
		prompt := contextI64(context, "prompt_tokens")
		completion := contextI64(context, "completion_tokens")
		cached := min64(contextI64(context, "cached_prompt_tokens"), prompt)
		reasoning := min64(contextI64(context, "reasoning_tokens"), completion)
		input := clampNonNeg(prompt - cached)
		output := clampNonNeg(completion - reasoning)
		tokens := TokenBreakdown{Input: input, Output: output, CacheRead: cached, Reasoning: reasoning}
		if tokens.Total() == 0 {
			continue
		}
		ts := mtimeMS
		if stamp, _ := value["ts"].(string); stamp != "" {
			if parsed, ok := ParseRFC3339MS(stamp); ok {
				ts = parsed
			}
		}
		// loop_index 1 is the turn's first inference (upstream default 1).
		turnStart := true
		if raw, exists := context["loop_index"]; exists {
			if floated, ok := raw.(float64); ok && int64(floated) > 1 {
				turnStart = false
			}
		}
		model := modelsBySession[session]
		if model == "" {
			model = "grok-unknown"
		}
		eventID := ""
		for _, key := range []string{"event_id", "eventId", "id", "uuid"} {
			if id, _ := value[key].(string); strings.TrimSpace(id) != "" {
				eventID = "id:" + strings.TrimSpace(id)
				break
			}
		}
		if eventID == "" {
			if ctx, ok := value["ctx"].(map[string]any); ok {
				for _, key := range []string{"event_id", "eventId", "id", "uuid"} {
					if id, _ := ctx[key].(string); strings.TrimSpace(id) != "" {
						eventID = "id:" + strings.TrimSpace(id)
						break
					}
				}
			}
		}
		if eventID == "" {
			serialized, _ := json.Marshal(value)
			eventID = "row:" + string(serialized)
		}
		key := "grok-unified:" + session + ":" + eventID
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		emit(UnifiedEntry{
			Client: "grok", ModelID: model, ProviderID: "xai",
			SessionID: session, TimestampMS: ts, Tokens: tokens, DedupKey: key,
			IsTurnStart: turnStart,
		})
	}
}

func contextI64(context map[string]any, key string) int64 {
	if raw, exists := context[key]; exists {
		if floated, ok := raw.(float64); ok && floated >= 0 {
			return int64(floated)
		}
	}
	return 0
}

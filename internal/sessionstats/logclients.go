package sessionstats

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// lmstudioParser scrapes the OpenAI-compatible server log
// (~/.lmstudio/server-logs/YYYY-MM/*.log): pretty-printed final responses
// whose `"usage": {` objects carry per-call counts. The upstream streams the
// file through a bounded window with refill-boundary arithmetic; this port
// reads the capped whole file and scans markers in one pass (documented
// deviation §12 — logs above the read cap are skipped, not windowed).
type lmstudioParser struct{}

const lmstudioUsageMarker = `"usage": {`

func (lmstudioParser) Client() string { return "lmstudio" }

func (lmstudioParser) Roots(home string) []string { return []string{home + "/.lmstudio/server-logs"} }

func (lmstudioParser) Discover(root string) ([]string, error) {
	return WalkFilesWithSuffix(root, ".log")
}

type lmstudioUsage struct {
	PromptTokens     *int64 `json:"prompt_tokens"`
	PromptTokensAlt  *int64 `json:"promptTokens"`
	InputTokens      *int64 `json:"input_tokens"`
	InputTokensAlt   *int64 `json:"inputTokens"`
	CompletionTokens *int64 `json:"completion_tokens"`
	CompletionAlt    *int64 `json:"completionTokens"`
	OutputTokens     *int64 `json:"output_tokens"`
	OutputAlt        *int64 `json:"outputTokens"`
	PromptDetails    *struct {
		CachedTokens *int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	PromptDetailsAlt *struct {
		CachedTokens *int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	CachedTokens  *int64 `json:"cached_tokens"`
	OutputDetails *struct {
		ReasoningTokens *int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

func (lmstudioParser) ParseFile(file string, mtimeMS int64, emit func(UnifiedEntry)) error {
	body, ok := readFileLimited(file, maxFileBytes)
	if !ok {
		return nil
	}
	session := "lmstudio:" + file
	fallbackTS := mtimeMS
	cursor := 0
	for {
		marker := bytes.Index(body[cursor:], []byte(lmstudioUsageMarker))
		if marker < 0 {
			break
		}
		markerAt := cursor + marker
		objectStart := markerAt + len(lmstudioUsageMarker)
		// Balanced-object end (a usage object contains no braces in strings we
		// care about; escaped quotes are skipped conservatively).
		end := balancedJSONObjectEnd(body, objectStart)
		if end < 0 {
			break
		}
		// Metadata slice: bytes between the previous record's end and this
		// marker carry the model id and the timestamp for this record.
		metadata := body[cursor:markerAt]
		var usage lmstudioUsage
		// objectStart sits just after the marker's opening brace; the slice
		// must include it to be valid JSON.
		if json.Unmarshal(body[objectStart-1:end], &usage) == nil {
			model := lastJSONStringField(metadata, []byte(`"id"`))
			if known := lastJSONStringField(metadata, []byte(`"model"`)); known != "" {
				model = known
			}
			if model == "" {
				model = "lms-unknown"
			}
			prompt := firstPositive(usage.PromptTokens, usage.PromptTokensAlt, usage.InputTokens, usage.InputTokensAlt)
			completion := firstPositive(usage.CompletionTokens, usage.CompletionAlt, usage.OutputTokens, usage.OutputAlt)
			cached := int64(0)
			if usage.PromptDetails != nil {
				cached = nonNeg(usage.PromptDetails.CachedTokens)
			} else if usage.PromptDetailsAlt != nil {
				cached = nonNeg(usage.PromptDetailsAlt.CachedTokens)
			} else {
				cached = nonNeg(usage.CachedTokens)
			}
			reasoning := int64(0)
			if usage.OutputDetails != nil {
				reasoning = nonNeg(usage.OutputDetails.ReasoningTokens)
			}
			input, cacheRead := subtractCachedOverlap(prompt, cached)
			tokens := TokenBreakdown{Input: input, Output: clampNonNeg(completion - reasoning), CacheRead: cacheRead, Reasoning: reasoning}
			if tokens.Total() > 0 {
				ts := fallbackTS
				if stamp := lastLogTimestamp(metadata); stamp > 0 {
					ts = stamp
				}
				emit(UnifiedEntry{
					Client: "lmstudio", ModelID: model,
					ProviderID: providerForModelOr(model, "lmstudio"),
					SessionID:  session, TimestampMS: ts, Tokens: tokens,
					DedupKey: "lmstudio:" + file + ":" + strconv.Itoa(markerAt) + ":" + model,
				})
			}
		}
		cursor = end
	}
	return nil
}

func balancedJSONObjectEnd(body []byte, start int) int {
	// The caller already consumed the opening brace (the usage marker includes
	// it), so the scan starts one level deep.
	depth := 1
	inString := false
	escaped := false
	for index := start; index < len(body); index++ {
		ch := body[index]
		if inString {
			if escaped {
				escaped = false
			} else if ch == '\\' {
				escaped = true
			} else if ch == '"' {
				inString = false
			}
			continue
		}
		switch ch {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return index + 1
			}
		}
	}
	return -1
}

// lastJSONStringField finds the last `"field": "value"` occurrence in a byte
// slice — LM Studio pretty-prints, so the pattern is stable.
func lastJSONStringField(body []byte, field []byte) string {
	pattern := append(field, ':')
	last := bytes.LastIndex(body, pattern)
	if last < 0 {
		return ""
	}
	rest := body[last+len(pattern):]
	open := bytes.IndexByte(rest, '"')
	if open < 0 {
		return ""
	}
	rest = rest[open+1:]
	close := bytes.IndexByte(rest, '"')
	if close < 0 {
		return ""
	}
	return string(rest[:close])
}

// lastLogTimestamp picks the last timestamp header in the metadata slice.
// LM Studio writes either an RFC3339 stamp or a naive local
// "[YYYY-MM-DD HH:MM:SS.mmm] [level]" header; the naive form is parsed in
// the machine's local zone (the server logs local wall-clock time).
func lastLogTimestamp(body []byte) int64 {
	text := string(body)
	lastRFC, lastNaive := "", ""
	for _, candidate := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(candidate)
		if idx := strings.Index(trimmed, "["); idx > 0 && idx <= 45 {
			stamp := strings.TrimSpace(trimmed[:idx])
			if len(stamp) >= 20 && (stamp[len(stamp)-1] == 'Z' || strings.Contains(stamp[10:], "+")) {
				lastRFC = stamp
				continue
			}
		}
		if start := strings.Index(trimmed, "["); start >= 0 {
			if end := strings.Index(trimmed[start+1:], "]"); end > 0 {
				header := trimmed[start+1 : start+1+end]
				// Both spellings: [YYYY-MM-DD HH:MM:SS] (19) and .mmm (23).
				if (len(header) == 19 || len(header) == 23) && header[4] == '-' && header[7] == '-' && header[10] == ' ' {
					lastNaive = header
				}
			}
		}
	}
	if lastRFC != "" {
		if ts, ok := ParseRFC3339MS(lastRFC); ok {
			return ts
		}
	}
	if lastNaive != "" {
		if parsed, err := time.ParseInLocation("2006-01-02 15:04:05.000", lastNaive[:23], time.Local); err == nil {
			return parsed.UnixMilli()
		}
		if parsed, err := time.ParseInLocation("2006-01-02 15:04:05", lastNaive[:19], time.Local); err == nil {
			return parsed.UnixMilli()
		}
	}
	return 0
}

// commandcodeParser reads ~/.commandcode/projects/<slug>/<session>.jsonl —
// Claude-mirror entries with an Anthropic-shaped usage object; assistant
// rows without usage estimate from the turn's accumulated user input.
type commandcodeParser struct{}

type commandcodeUsage struct {
	InputTokens     *int64   `json:"inputTokens"`
	OutputTokens    *int64   `json:"outputTokens"`
	CacheReadTokens *int64   `json:"cacheReadTokens"`
	CacheWriteToken *int64   `json:"cacheWriteTokens"`
	CostUSD         *float64 `json:"costUsd"`
}

// commandcodeTokenCeiling clamps untrusted transcript values so one tampered
// line cannot poison aggregates (upstream: 1e12).
const commandcodeTokenCeiling = 1_000_000_000_000

// reportedCost keeps an explicit $0 (free-promo models bill nothing) but
// rejects negative or implausibly large costs.
func (u *commandcodeUsage) reportedCost() (float64, bool) {
	if u == nil || u.CostUSD == nil {
		return 0, false
	}
	cost := *u.CostUSD
	if cost < 0 || cost > 1_000_000 {
		return 0, false
	}
	return cost, true
}

func (u *commandcodeUsage) breakdown() (TokenBreakdown, bool) {
	if u == nil || (u.InputTokens == nil && u.OutputTokens == nil && u.CacheReadTokens == nil && u.CacheWriteToken == nil) {
		return TokenBreakdown{}, false
	}
	clamp := func(value *int64) int64 {
		if value == nil || *value < 0 {
			return 0
		}
		if *value > commandcodeTokenCeiling {
			return commandcodeTokenCeiling
		}
		return *value
	}
	cacheRead := clamp(u.CacheReadTokens)
	cacheWrite := clamp(u.CacheWriteToken)
	// inputTokens carries the full input side: split the cache buckets out
	// so the five buckets stay mutually exclusive.
	input := clampNonNeg(clamp(u.InputTokens) - cacheRead - cacheWrite)
	tokens := TokenBreakdown{
		Input: input, Output: clamp(u.OutputTokens),
		CacheRead: cacheRead, CacheWrite: cacheWrite,
	}
	return tokens, tokens.Total() > 0
}

func (commandcodeParser) Client() string { return "commandcode" }

func (commandcodeParser) Roots(home string) []string {
	return []string{home + "/.commandcode/projects"}
}

func (commandcodeParser) Discover(root string) ([]string, error) {
	files, err := WalkJSONLFiles(root)
	if err != nil {
		return nil, err
	}
	kept := files[:0]
	for _, file := range files {
		if !strings.HasSuffix(file, ".checkpoints.jsonl") {
			kept = append(kept, file)
		}
	}
	return kept, nil
}

func (commandcodeParser) ParseFile(file string, mtimeMS int64, emit func(UnifiedEntry)) error {
	session := commandCodeSessionFromPath(file)
	wsKey := commandCodeWorkspaceFromPath(file)
	wsLabel := ""
	if wsKey != "" {
		wsLabel = WorkspaceLabelFromKey(wsKey)
	}
	configModel := commandCodeConfigModel(file)
	var sessionID string
	var modelID string
	turnInputChars := int64(0)
	pendingTurnStart := false
	assistantIndex := 0
	forEachJSONLine(file, mtimeMS, func(raw []byte) {
		var entry struct {
			Type      string            `json:"type"`
			ID        string            `json:"id"`
			Timestamp string            `json:"timestamp"`
			Model     string            `json:"model"`
			Usage     *commandcodeUsage `json:"usage"`
			SessionID string            `json:"sessionId"`
			Message   *struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(raw, &entry) != nil {
			return
		}
		if entry.Type == "session" && strings.TrimSpace(entry.ID) != "" {
			sessionID = strings.TrimSpace(entry.ID)
		} else if sessionID == "" && strings.TrimSpace(entry.SessionID) != "" {
			sessionID = strings.TrimSpace(entry.SessionID)
		}
		if strings.TrimSpace(entry.Model) != "" {
			modelID = strings.TrimSpace(entry.Model)
		}
		if entry.Type == "model_change" {
			return
		}
		if entry.Message == nil {
			return
		}
		role := entry.Message.Role
		chars := contentCharCount(entry.Message.Content)
		if role == "assistant" {
			tokens, hasUsage := entry.Usage.breakdown()
			cost, costReported := entry.Usage.reportedCost()
			if !hasUsage {
				tokens = TokenBreakdown{
					Input:  estimateTokensFromChars(turnInputChars),
					Output: estimateTokensFromChars(chars),
				}
			}
			turnInputChars = 0
			// All-zero buckets with no POSITIVE cost are bookkeeping rows the
			// upstream drops; an explicit $0 stays only when a real bucket
			// carries usage (free-promo models).
			if tokens.Total() == 0 && !(costReported && cost > 0) {
				return
			}
			resolvedSession := sessionID
			if resolvedSession == "" {
				resolvedSession = session
			}
			model := modelID
			if model == "" {
				model = configModel
			}
			if model == "" {
				model = "unknown"
			}
			ts := mtimeMS
			explicit := int64(-1)
			if parsed, ok := ParseRFC3339MS(entry.Timestamp); ok {
				ts, explicit = parsed, parsed
			}
			key := "commandcode:" + wsKeyOrDefault(wsKey) + ":" + resolvedSession + ":" + itoa(assistantIndex)
			if strings.TrimSpace(entry.ID) != "" {
				key = strings.TrimSpace(entry.ID) + ":" + strconv.FormatInt(explicit, 10)
			}
			emit(UnifiedEntry{
				Client: "commandcode", ModelID: model,
				ProviderID: providerForModelOr(model, "command-code"),
				SessionID:  resolvedSession, WorkspaceKey: wsKey, WorkspaceLabel: wsLabel,
				TimestampMS: ts, Tokens: tokens, IsTurnStart: pendingTurnStart, DedupKey: key,
			})
			assistantIndex++
			pendingTurnStart = false
		} else if role == "user" {
			if !isToolResultContent(entry.Message.Content) {
				pendingTurnStart = true
			}
			turnInputChars += chars
		}
	})
	return nil
}

func wsKeyOrDefault(key string) string {
	if key == "" {
		return "-"
	}
	return key
}

func commandCodeSessionFromPath(file string) string {
	return strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))
}

func commandCodeWorkspaceFromPath(file string) string {
	// projects/<slug>/<session>.jsonl — the slug is the workspace.
	return dirBase(fileDir(file))
}

func commandCodeConfigModel(file string) string {
	// projects/<slug>/.config.json carries the configured default model.
	candidate := fileDir(file) + "/.config.json"
	body, ok := readFileLimited(candidate, maxFileBytes)
	if !ok {
		return ""
	}
	var config struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &config) != nil {
		return ""
	}
	return strings.TrimSpace(config.Model)
}

func contentCharCount(content json.RawMessage) int64 {
	if len(content) == 0 {
		return 0
	}
	var text string
	if json.Unmarshal(content, &text) == nil {
		return int64(len([]rune(text)))
	}
	var blocks []struct {
		Text string `json:"text"`
		Type string `json:"type"`
	}
	if json.Unmarshal(content, &blocks) == nil {
		total := int64(0)
		for _, block := range blocks {
			total += int64(len([]rune(block.Text)))
		}
		return total
	}
	return 0
}

func isToolResultContent(content json.RawMessage) bool {
	if len(content) == 0 {
		return false
	}
	var blocks []struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return false
	}
	for _, block := range blocks {
		if block.Type == "tool_result" {
			return true
		}
	}
	return false
}

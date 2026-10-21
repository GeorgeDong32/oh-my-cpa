package sessionstats

// Codex CLI: ~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl
// (sessions/codex.rs). token_count events carry cumulative
// total_token_usage (a mutable snapshot upstream — compaction can rewrite
// it) and per-increment last_token_usage. The port keeps the upstream state
// machine's six arms verbatim: last is the increment source, totals drive
// dedup and monotonicity only. Scope not ported: the headless JSONL variant,
// OpenClaw re-attribution and the fork-child inherited-baseline skip — the
// fork-scoped dedup key already collapses sibling replays.

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
)

type codexParser struct{}

func (codexParser) Client() string { return "codex" }

func (p codexParser) Roots(home string) []string {
	return []string{filepath.Join(home, ".codex", "sessions")}
}

func (p codexParser) Discover(root string) ([]string, error) {
	files, err := walkNamedPrefixFiles(root, "rollout-")
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}

func walkNamedPrefixFiles(root, prefix string) ([]string, error) {
	if root == "" {
		return nil, nil
	}
	var files []string
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return nil
		}
		if strings.HasPrefix(entry.Name(), prefix) && strings.HasSuffix(entry.Name(), ".jsonl") {
			files = append(files, path)
		}
		return nil
	})
	return files, nil
}

type codexTokenUsage struct {
	Input     *int64 `json:"input_tokens"`
	Cached    *int64 `json:"cached_input_tokens"`
	Output    *int64 `json:"output_tokens"`
	Reasoning *int64 `json:"reasoning_output_tokens"`
	Total     *int64 `json:"total_tokens"`
}

// codexTotals is the monotonicity view of a usage snapshot.
type codexTotals struct {
	input, output, cached, reasoning int64
}

func codexTotalsFrom(usage *codexTokenUsage) codexTotals {
	return codexTotals{
		input: nonNeg(usage.Input), output: nonNeg(usage.Output),
		cached: nonNeg(usage.Cached), reasoning: nonNeg(usage.Reasoning),
	}
}

func (t codexTotals) key(provider, model string) string {
	return fmt.Sprintf("codex:token_count-total:%s:%s:%s:%d:%d:%d:%d",
		"%s", provider, model, t.input, t.output, t.cached, t.reasoning)
}

// intoTokens maps the upstream snapshot to the console's buckets: cached is
// cache read; upstream total_tokens counts input+output (reasoning inside
// output), so no further arithmetic is needed.
func (u *codexTokenUsage) intoTokens() TokenBreakdown {
	return TokenBreakdown{
		Input: nonNeg(u.Input), Output: nonNeg(u.Output),
		CacheRead: nonNeg(u.Cached), Reasoning: nonNeg(u.Reasoning),
	}
}

func (t codexTotals) sameAs(other codexTotals) bool {
	return t == other
}

// deltaFrom is Some when every bucket grew (a clean forward step).
func (t codexTotals) deltaFrom(previous codexTotals) (codexTotals, bool) {
	delta := codexTotals{
		input: t.input - previous.input, output: t.output - previous.output,
		cached: t.cached - previous.cached, reasoning: t.reasoning - previous.reasoning,
	}
	if delta.input < 0 || delta.output < 0 || delta.cached < 0 || delta.reasoning < 0 {
		return codexTotals{}, false
	}
	return delta, true
}

func (t codexTotals) add(other codexTotals) codexTotals {
	return codexTotals{
		input: t.input + other.input, output: t.output + other.output,
		cached: t.cached + other.cached, reasoning: t.reasoning + other.reasoning,
	}
}

func (t codexTotals) intoTokens() TokenBreakdown {
	return TokenBreakdown{Input: t.input, Output: t.output, CacheRead: t.cached, Reasoning: t.reasoning}
}

// pendingMessage is a token-bearing record that arrived before any model was
// known; it waits for the next model-bearing event or defaults to "unknown".
type pendingMessage struct{ entry UnifiedEntry }

type codexEntry struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type codexSessionMetaPayload struct {
	ID           *string `json:"id"`
	CWD          *string `json:"cwd"`
	ForkedFromID string  `json:"forked_from_id"`
	Provider     *string `json:"model_provider"`
}

type codexModelInfo struct {
	Slug string `json:"slug"`
}

type codexTurnContextPayload struct {
	ModelInfo *codexModelInfo `json:"model_info"`
	Model     string          `json:"model"`
}

type codexTokenCountPayload struct {
	Model     string          `json:"model"`
	ModelName string          `json:"model_name"`
	Info      *codexTokenInfo `json:"info"`
}

type codexTokenInfo struct {
	TotalTokenUsage *codexTokenUsage `json:"total_token_usage"`
	LastTokenUsage  *codexTokenUsage `json:"last_token_usage"`
}

// effectiveCodexType flattens the two nesting generations: session_meta is a
// top-level type, while turn_context/user_message/token_count ride inside
// event_msg payloads.
func effectiveCodexType(entryType, payloadType string) string {
	if entryType == "event_msg" && payloadType != "" {
		return payloadType
	}
	return entryType
}

func (p codexParser) ParseFile(file string, mtimeMS int64, emit func(UnifiedEntry)) error {
	body, ok := readFileLimited(file, maxFileBytes)
	if !ok {
		return nil
	}
	sessionID := strings.TrimSuffix(filepath.Base(file), ".jsonl")
	sessionID = strings.TrimPrefix(sessionID, "rollout-")
	var provider, wsKey, wsLabel, currentModel, sessionFromMeta, forkedFrom string
	var previousTotals *codexTotals
	var lastAcceptedTS *int64
	pendingTurnStart := false
	// Messages that arrived before any model was known wait for one.
	var pending []pendingMessage
	dec := json.NewDecoder(strings.NewReader(string(body)))
	for dec.More() {
		var entry codexEntry
		if dec.Decode(&entry) != nil {
			continue
		}
		entryType := entry.Type
		payloadType := ""
		if entryType == "event_msg" {
			var probe struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(entry.Payload, &probe) == nil {
				payloadType = probe.Type
			}
		}
		_ = payloadType
		switch effectiveCodexType(entry.Type, payloadType) {
		case "session_meta":
			var payload codexSessionMetaPayload
			if json.Unmarshal(entry.Payload, &payload) == nil {
				if payload.ID != nil && *payload.ID != "" {
					sessionFromMeta = *payload.ID
				}
				if payload.ForkedFromID != "" {
					forkedFrom = payload.ForkedFromID
				}
				if payload.Provider != nil && strings.TrimSpace(*payload.Provider) != "" {
					provider = strings.TrimSpace(*payload.Provider)
				}
				if payload.CWD != nil {
					wsKey = NormalizeWorkspaceKey(*payload.CWD)
					wsLabel = WorkspaceLabelFromKey(wsKey)
				}
			}
		case "turn_context":
			var payload codexTurnContextPayload
			if json.Unmarshal(entry.Payload, &payload) == nil {
				if model := codexModelFrom(payload.ModelInfo, payload.Model, ""); model != "" {
					currentModel = model
					flushPending(&pending, currentModel, emit)
				}
			}
		case "user_message":
			pendingTurnStart = true
		case "token_count":
			var payload codexTokenCountPayload
			if json.Unmarshal(entry.Payload, &payload) != nil || payload.Info == nil {
				continue
			}
			model := codexModelFrom(nil, payload.Model, payload.ModelName)
			if model == "" {
				model = currentModel
			}
			if model != "" && currentModel == "" {
				currentModel = model
				flushPending(&pending, model, emit)
			}
			var totalUsage, lastUsage *codexTokenUsage
			if payload.Info.TotalTokenUsage != nil {
				totalUsage = payload.Info.TotalTokenUsage
			}
			if payload.Info.LastTokenUsage != nil {
				lastUsage = payload.Info.LastTokenUsage
			}
			var tokens TokenBreakdown
			var nextTotals *codexTotals
			switch {
			case totalUsage != nil && lastUsage != nil && previousTotals != nil:
				total := codexTotalsFrom(totalUsage)
				if total.sameAs(*previousTotals) {
					continue
				}
				if _, clean := total.deltaFrom(*previousTotals); !clean && staleRegression(total, *previousTotals, codexTotalsFrom(lastUsage)) {
					continue
				}
				tokens = lastUsage.intoTokens()
				next := total
				nextTotals = &next
			case totalUsage != nil && lastUsage != nil:
				// First event of a session: last only — a resumed session must
				// not import its history.
				tokens = lastUsage.intoTokens()
				next := codexTotalsFrom(totalUsage)
				nextTotals = &next
			case totalUsage != nil && previousTotals != nil:
				total := codexTotalsFrom(totalUsage)
				if total.sameAs(*previousTotals) {
					continue
				}
				if delta, clean := total.deltaFrom(*previousTotals); clean {
					tokens = delta.intoTokens()
					next := total
					nextTotals = &next
				} else {
					previousTotals = &total
					continue
				}
			case totalUsage != nil:
				tokens = totalUsage.intoTokens()
				next := codexTotalsFrom(totalUsage)
				nextTotals = &next
			case lastUsage != nil && previousTotals != nil:
				tokens = lastUsage.intoTokens()
				next := previousTotals.add(codexTotalsFrom(lastUsage))
				nextTotals = &next
			case lastUsage != nil:
				tokens = lastUsage.intoTokens()
			default:
				continue
			}
			if tokens.Input == 0 && tokens.Output == 0 && tokens.CacheRead == 0 && tokens.Reasoning == 0 {
				continue
			}
			previousTotals = nextTotals
			parsedTS, hasTS := ParseRFC3339MS(entry.Timestamp)
			ts := mtimeMS
			if lastAcceptedTS != nil {
				ts = *lastAcceptedTS
			} else if hasTS {
				ts = parsedTS
			}
			if hasTS && (lastAcceptedTS == nil || parsedTS > *lastAcceptedTS) {
				cursor := parsedTS
				lastAcceptedTS = &cursor
			}
			resolvedProvider := provider
			if resolvedProvider == "" {
				resolvedProvider = inferredCodexProvider(model)
			}
			// Fork sibling replays share the fork parent's scope.
			scopeID := forkedFrom
			if scopeID == "" {
				scopeID = sessionFromMeta
			}
			if scopeID == "" {
				scopeID = sessionID
			}
			key := ""
			if totalUsage != nil {
				key = fmt.Sprintf("codex:token_count-total:%s:%s:%s:%d:%d:%d:%d",
					scopeID, resolvedProvider, model,
					nonNeg(totalUsage.Input), nonNeg(totalUsage.Output), nonNeg(totalUsage.Cached), nonNeg(totalUsage.Reasoning))
			} else {
				key = fmt.Sprintf("codex:token_count:%d:%s:%s:%d:%d:%d:%d:%d",
					ts, resolvedProvider, model, tokens.Input, tokens.Output, tokens.CacheRead, tokens.CacheWrite, tokens.Reasoning)
			}
			message := UnifiedEntry{
				Client: p.Client(), ModelID: model, ProviderID: resolvedProvider, SessionID: sessionID,
				WorkspaceKey: wsKey, WorkspaceLabel: wsLabel, TimestampMS: ts, Tokens: tokens,
				DedupKey: key, IsTurnStart: pendingTurnStart,
			}
			pendingTurnStart = false
			if model == "" {
				pending = append(pending, pendingMessage{entry: message})
				continue
			}
			emit(message)
		}
	}
	// Messages still pending without a model keep "unknown".
	for _, message := range pending {
		message.entry.ModelID = "unknown"
		emit(message.entry)
	}
	return nil
}

func flushPending(pending *[]pendingMessage, model string, emit func(UnifiedEntry)) {
	for _, message := range *pending {
		message.entry.ModelID = model
		emit(message.entry)
	}
	*pending = nil
}

// staleRegression mirrors looks_like_stale_regression: totals that went
// backwards while last is large relative to the gap are compaction rewrites,
// not usage.
func staleRegression(total, previous, last codexTotals) bool {
	inputGap := previous.input - total.input
	outputGap := previous.output - total.output
	if inputGap <= 0 && outputGap <= 0 {
		return false
	}
	return last.input > inputGap || last.output > outputGap
}

func codexModelFrom(info *codexModelInfo, model, modelName string) string {
	if info != nil && strings.TrimSpace(info.Slug) != "" {
		return strings.TrimSpace(info.Slug)
	}
	if value := strings.TrimSpace(model); value != "" {
		return value
	}
	if value := strings.TrimSpace(modelName); value != "" {
		return value
	}
	return ""
}

func inferredCodexProvider(model string) string {
	lower := strings.ToLower(model)
	switch {
	case strings.Contains(lower, "gpt"), strings.Contains(lower, "o3"), strings.Contains(lower, "o4"), strings.Contains(lower, "codex"):
		return "openai"
	default:
		return "openai"
	}
}

package sessionstats

import (
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
)

// Tencent CodeBuddy / WorkBuddy shared transcript formats (tencent_buddy.rs).
// Both parse detailed JSONL transcripts and IDE extension logs through the
// same buckets: the usage shape is a union of every field spelling the two
// writers have used, with an explicit "is the reported total cache-inclusive"
// disambiguation for the ambiguous input fields.

type buddyUsage struct {
	CachedMissTokens   *int64 `json:"cachedMissTokens"`
	CacheMissTokens    *int64 `json:"cacheMissTokens"`
	InputTokens        *int64 `json:"input_tokens"`
	InputTokensCamel   *int64 `json:"inputTokens"`
	PromptTokens       *int64 `json:"prompt_tokens"`
	TotalTokens        *int64 `json:"total_tokens"`
	TotalTokensCamel   *int64 `json:"totalTokens"`
	OutputTokens       *int64 `json:"output_tokens"`
	OutputTokensCamel  *int64 `json:"outputTokens"`
	CompletionTokens   *int64 `json:"completion_tokens"`
	CacheRead          *int64 `json:"cache_read_input_tokens"`
	CacheReadCamel     *int64 `json:"cacheReadInputTokens"`
	CacheTokens        *int64 `json:"cacheTokens"`
	PromptCacheHit     *int64 `json:"prompt_cache_hit_tokens"`
	CachedTokens       *int64 `json:"cached_tokens"`
	CacheCreation      *int64 `json:"cache_creation_input_tokens"`
	CacheCreationCamel *int64 `json:"cacheCreationInputTokens"`
	CachedWriteTokens  *int64 `json:"cachedWriteTokens"`
	PromptCacheWrite   *int64 `json:"prompt_cache_write_tokens"`
	ThinkingTokens     *int64 `json:"completion_thinking_tokens"`
	ThinkingCamel      *int64 `json:"completionThinkingTokens"`
	ReasoningTokens    *int64 `json:"reasoningTokens"`
}

func firstPresent(values ...*int64) int64 {
	for _, value := range values {
		if value != nil {
			if *value < 0 {
				return 0
			}
			return *value
		}
	}
	return 0
}

// firstPositive prefers a positive value, falling back to the first present
// one — an upstream nuance: zero cache fields are common and legitimate.
func (u *buddyUsage) breakdown() (TokenBreakdown, bool) {
	cacheRead := firstPositive(u.CacheRead, u.CacheReadCamel, u.CacheTokens, u.PromptCacheHit, u.CachedTokens)
	output := firstPresent(u.OutputTokens, u.OutputTokensCamel, u.CompletionTokens)
	cacheWrite := firstPositive(u.CacheCreation, u.CacheCreationCamel, u.CachedWriteTokens, u.PromptCacheWrite)
	reasoning := firstPresent(u.ThinkingTokens, u.ThinkingCamel, u.ReasoningTokens)
	input := u.inputExclusive(cacheRead, cacheWrite, output)
	tokens := TokenBreakdown{Input: input, Output: output, CacheRead: cacheRead, CacheWrite: cacheWrite, Reasoning: reasoning}
	return tokens, tokens.Total() > 0
}

// inputExclusive: cachedMissTokens (extension-log style) already excludes
// cached input; the other input spellings are ambiguous unless a reported
// total proves they include the cache buckets — then the cached share splits
// out so the buckets stay additive.
func (u *buddyUsage) inputExclusive(cacheRead, cacheWrite, output int64) int64 {
	miss := u.CachedMissTokens
	if miss == nil {
		miss = u.CacheMissTokens
	}
	if miss != nil {
		return clampNonNeg(*miss)
	}
	input := firstPresent(u.InputTokens, u.InputTokensCamel, u.PromptTokens)
	var total int64
	hasTotal := false
	if u.TotalTokens != nil {
		total, hasTotal = *u.TotalTokens, true
	} else if u.TotalTokensCamel != nil {
		total, hasTotal = *u.TotalTokensCamel, true
	}
	if !hasTotal {
		return input
	}
	if total < 0 {
		total = 0
	}
	inclusive := input + output
	exclusive := inclusive + cacheRead + cacheWrite
	if cacheRead > 0 && total == inclusive && inclusive != exclusive {
		return clampNonNeg(input - cacheRead)
	}
	return input
}

type buddyProviderData struct {
	Model          *string     `json:"model"`
	RequestModelID *string     `json:"requestModelId"`
	MessageID      *string     `json:"messageId"`
	TraceID        *string     `json:"traceId"`
	Usage          *buddyUsage `json:"usage"`
	RawUsage       *buddyUsage `json:"rawUsage"`
}

type buddyMessage struct {
	Model *string     `json:"model"`
	Usage *buddyUsage `json:"usage"`
}

type buddyLine struct {
	ID           *string            `json:"id"`
	Timestamp    *int64             `json:"timestamp"`
	Type         *string            `json:"type"`
	Role         *string            `json:"role"`
	Status       *string            `json:"status"`
	SessionID    *string            `json:"sessionId"`
	CWD          *string            `json:"cwd"`
	Message      *buddyMessage      `json:"message"`
	ProviderData *buddyProviderData `json:"providerData"`
}

func parseBuddyJSONLFile(client, defaultModel, file string, mtimeMS int64, emit func(UnifiedEntry)) {
	fallbackSession := strings.TrimSuffix(filepath.Base(file), ".jsonl")
	forEachJSONLine(file, mtimeMS, func(raw []byte) {
		var line buddyLine
		if json.Unmarshal(raw, &line) != nil {
			return
		}
		isAssistant := line.Type != nil && *line.Type == "message" && line.Role != nil && *line.Role == "assistant"
		isCall := line.Type != nil && *line.Type == "function_call"
		if !isAssistant && !isCall {
			return
		}
		if line.Status != nil && *line.Status != "completed" {
			return
		}
		var usage *buddyUsage
		if line.Message != nil && line.Message.Usage != nil {
			usage = line.Message.Usage
		} else if line.ProviderData != nil && line.ProviderData.Usage != nil {
			usage = line.ProviderData.Usage
		} else if line.ProviderData != nil && line.ProviderData.RawUsage != nil {
			usage = line.ProviderData.RawUsage
		}
		if usage == nil {
			return
		}
		tokens, ok := usage.breakdown()
		if !ok {
			return
		}
		model := defaultModel
		if line.ProviderData != nil {
			for _, candidate := range []*string{line.ProviderData.Model, line.ProviderData.RequestModelID} {
				if candidate != nil && strings.TrimSpace(*candidate) != "" {
					model = strings.TrimSpace(*candidate)
					break
				}
			}
		}
		if line.Message != nil && line.Message.Model != nil && strings.TrimSpace(*line.Message.Model) != "" && model == defaultModel {
			model = strings.TrimSpace(*line.Message.Model)
		}
		provider := providerForModel(model)
		if provider == "" {
			provider = "tencent"
		}
		session := fallbackSession
		if line.SessionID != nil && strings.TrimSpace(*line.SessionID) != "" {
			session = strings.TrimSpace(*line.SessionID)
		}
		ts := mtimeMS
		if line.Timestamp != nil && *line.Timestamp > 0 {
			ts = *line.Timestamp
		}
		var key string
		if line.ProviderData != nil {
			for _, candidate := range []*string{line.ProviderData.MessageID, line.ProviderData.TraceID} {
				if candidate != nil && strings.TrimSpace(*candidate) != "" {
					key = client + ":" + session + ":" + strings.TrimSpace(*candidate)
					break
				}
			}
		}
		if key == "" && line.ID != nil && strings.TrimSpace(*line.ID) != "" {
			key = client + ":" + session + ":" + strings.TrimSpace(*line.ID)
		}
		wsKey, wsLabel := "", ""
		if line.CWD != nil {
			if normalized := NormalizeWorkspaceKey(*line.CWD); normalized != "" {
				wsKey, wsLabel = normalized, WorkspaceLabelFromKey(normalized)
			}
		}
		emit(UnifiedEntry{
			Client: client, ModelID: model, ProviderID: provider, SessionID: session,
			WorkspaceKey: wsKey, WorkspaceLabel: wsLabel, TimestampMS: ts, Tokens: tokens,
			DedupKey: key,
		})
	})
}

// parseBuddyExtensionLogFile scrapes the IDE extension's plain-text log: a
// "Model prepared:" line arms an agent's model, and later "Agent execution
// successful with usage:" lines carry a JSON usage object. The dedup key uses
// SECOND granularity because the same reporter line is mirrored to several
// log sinks a few milliseconds apart and a ms key double-counts each mirror.
func parseBuddyExtensionLogFile(client, defaultModel, file string, mtimeMS int64, emit func(UnifiedEntry)) {
	modelsByAgent := map[string]string{}
	forEachTextLine(file, mtimeMS, func(line string) {
		if strings.Contains(line, "[CraftInvokableAgent]") && strings.Contains(line, "Model prepared:") {
			if agent, model, ok := parseModelPreparedLine(line); ok {
				modelsByAgent[agent] = model
			}
			return
		}
		if !strings.Contains(line, "[AgentReporter]") || !strings.Contains(line, "Agent execution successful with usage:") {
			return
		}
		agent, okAgent := bracketValueAfter(line, "[AgentReporter]")
		if !okAgent {
			return
		}
		usagePart := line
		if marker := "Agent execution successful with usage:"; strings.Contains(line, marker) {
			usagePart = line[strings.Index(line, marker)+len(marker):]
		} else {
			return
		}
		usageJSON := strings.TrimSpace(usagePart)
		if end := strings.LastIndex(usageJSON, "}"); end >= 0 {
			usageJSON = usageJSON[:end+1]
		} else {
			return
		}
		var usage buddyUsage
		if json.Unmarshal([]byte(usageJSON), &usage) != nil {
			return
		}
		tokens, valid := usage.breakdown()
		if !valid {
			return
		}
		ts := mtimeMS
		if parsed, ok := parseLogTimestampMS(line); ok {
			ts = parsed
		}
		model := defaultModel
		if known, ok := modelsByAgent[agent]; ok {
			model = known
		}
		provider := providerForModel(model)
		if provider == "" {
			provider = "tencent"
		}
		wsKey, wsLabel := workspaceFromLogPath(file)
		key := client + ":extension-log:" + agent + ":" + strconv.FormatInt(ts/1000, 10) +
			":" + strconv.FormatInt(tokens.Input, 10) + ":" + strconv.FormatInt(tokens.Output, 10) +
			":" + strconv.FormatInt(tokens.CacheRead, 10) + ":" + strconv.FormatInt(tokens.CacheWrite, 10) +
			":" + strconv.FormatInt(tokens.Reasoning, 10)
		emit(UnifiedEntry{
			Client: client, ModelID: model, ProviderID: provider, SessionID: agent,
			WorkspaceKey: wsKey, WorkspaceLabel: wsLabel, TimestampMS: ts, Tokens: tokens,
			DedupKey: key,
		})
	})
}

// parseLogTimestampMS pulls the leading RFC3339 timestamp off an extension
// log line ("2026-01-02T03:04:05.000Z [AgentReporter] ...").
func parseLogTimestampMS(line string) (int64, bool) {
	end := strings.Index(line, " ")
	if end <= 0 {
		return 0, false
	}
	if ts, ok := ParseRFC3339MS(strings.TrimSpace(line[:end])); ok {
		return ts, true
	}
	return 0, false
}

func parseModelPreparedLine(line string) (agent, model string, ok bool) {
	agent, okAgent := bracketValueAfter(line, "[CraftInvokableAgent]")
	if !okAgent {
		return "", "", false
	}
	marker := "Model prepared:"
	idx := strings.Index(line, marker)
	if idx < 0 {
		return "", "", false
	}
	rest := line[idx+len(marker):]
	// "Model prepared: <model> (<provider>)" — keep the model, drop the
	// parenthesized provider.
	rest = strings.TrimSpace(rest)
	if idx := strings.LastIndex(rest, "("); idx > 0 {
		rest = strings.TrimSpace(rest[:idx])
	}
	if rest == "" {
		return "", "", false
	}
	return agent, rest, true
}

func bracketValueAfter(line, marker string) (string, bool) {
	idx := strings.Index(line, marker)
	if idx < 0 {
		return "", false
	}
	rest := line[idx+len(marker):]
	open := strings.Index(rest, "[")
	close_ := strings.Index(rest, "]")
	if open < 0 || close_ < open {
		return "", false
	}
	value := strings.TrimSpace(rest[open+1 : close_])
	if value == "" {
		return "", false
	}
	return value, true
}

func workspaceFromLogPath(file string) (string, string) {
	// The extension log lives under the workspace's log dir; the workspace
	// name is two levels up from the log file (logs/<workspace>/*.log shape).
	parent := fileDir(file)
	grandparent := fileDir(parent)
	if base := dirBase(grandparent); base != "" && base != "." && base != "/" {
		return base, base
	}
	return "", ""
}

// codebuddyParser: ~/.codebuddy/projects/*.jsonl transcripts plus the IDE
// extension's CodeBuddy.log mirrors under ~/.codebuddy/logs.
type codebuddyParser struct{}

func (codebuddyParser) Client() string { return "codebuddy" }

func (codebuddyParser) Roots(home string) []string {
	return []string{home + "/.codebuddy/projects", home + "/.codebuddy/logs"}
}

func (codebuddyParser) Discover(root string) ([]string, error) {
	if strings.HasSuffix(root, "/logs") {
		return WalkFilesWithSuffix(root, ".log")
	}
	return WalkJSONLFiles(root)
}

func (codebuddyParser) ParseFile(file string, mtimeMS int64, emit func(UnifiedEntry)) error {
	if strings.EqualFold(filepath.Ext(file), ".log") {
		parseBuddyExtensionLogFile("codebuddy", "codebuddy", file, mtimeMS, emit)
		return nil
	}
	parseBuddyJSONLFile("codebuddy", "codebuddy", file, mtimeMS, emit)
	return nil
}

// workbuddyParser: ~/.workbuddy/projects and ~/.workbuddy-ai variants.
type workbuddyParser struct{}

func (workbuddyParser) Client() string { return "workbuddy" }

func (workbuddyParser) Roots(home string) []string {
	return []string{home + "/.workbuddy/projects", home + "/.workbuddy-ai/projects"}
}

func (workbuddyParser) Discover(root string) ([]string, error) { return WalkJSONLFiles(root) }

func (workbuddyParser) ParseFile(file string, mtimeMS int64, emit func(UnifiedEntry)) error {
	parseBuddyJSONLFile("workbuddy", "workbuddy", file, mtimeMS, emit)
	return nil
}

package sessionstats

// Claude Code: ~/.claude/projects/<workspace>/*.jsonl
// (sessions/claudecode.rs). Assistant entries carry API usage; streaming
// duplicates of the same message merge per-field max under the
// messageId(:requestId) key. Sidechain (subagent) entries re-attribute to the
// parent session id. <synthetic> placeholder models are suppressed, and
// tool_result usage counts only from explicit structured metadata — a char
// estimate would double-count what the next assistant turn's input_tokens
// already reports. Scope not ported: headless JSON variant, cc-mirror
// variants, parent-file subagent name resolution (the stem agent id stands
// in), workflow journals (path-shaped skip kept).

import (
	"encoding/json"
	"path/filepath"
	"strings"
)

type claudeParser struct{}

func (claudeParser) Client() string { return "claude-code" }

func (p claudeParser) Roots(home string) []string {
	return []string{filepath.Join(home, ".claude", "projects")}
}

func (p claudeParser) Discover(root string) ([]string, error) { return WalkJSONLFiles(root) }

type claudeEntry struct {
	Type          string          `json:"type"`
	IsSidechain   bool            `json:"isSidechain"`
	SessionID     *string         `json:"sessionId"`
	AgentID       *string         `json:"agentId"`
	RequestID     string          `json:"requestId"`
	Timestamp     string          `json:"timestamp"`
	Message       *claudeMessage  `json:"message"`
	ToolUseResult json.RawMessage `json:"toolUseResult"`
}

type claudeMessage struct {
	ID         string       `json:"id"`
	Model      string       `json:"model"`
	ProviderID string       `json:"provider_id"`
	Usage      *claudeUsage `json:"usage"`
}

type claudeUsage struct {
	Input         *int64 `json:"input_tokens"`
	Output        *int64 `json:"output_tokens"`
	CacheCreation *int64 `json:"cache_creation_input_tokens"`
	CacheRead     *int64 `json:"cache_read_input_tokens"`
}

type claudeUsage1h struct {
	Input   *int64 `json:"cache_creation_input_tokens"`
	Input1h *int64 `json:"cache_creation_1h_input_tokens"`
}

func (u *claudeUsage) breakdown() TokenBreakdown {
	return TokenBreakdown{
		Input: nonNeg(u.Input), Output: nonNeg(u.Output),
		CacheRead: nonNeg(u.CacheRead), CacheWrite: nonNeg(u.CacheCreation),
	}
}

// merged keeps the per-field max across streaming duplicates of one message.
type claudeMerged struct {
	entry   UnifiedEntry
	emitted bool
}

func isWorkflowJournal(path string) bool {
	return strings.Contains(filepath.ToSlash(path), "/workflow-journal/")
}

func (p claudeParser) ParseFile(file string, mtimeMS int64, emit func(UnifiedEntry)) error {
	if isWorkflowJournal(file) {
		return nil
	}
	body, ok := readFileLimited(file, maxFileBytes)
	if !ok {
		return nil
	}
	wsKey, wsLabel := WorkspaceAfterMarker(file, "projects", "")
	sessionID := strings.TrimSuffix(filepath.Base(file), ".jsonl")
	// Sidechain files carry the parent session id on every entry; the stem's
	// agent id names the subagent.
	sidechainAgent := ""
	toolResultKeys := map[string]struct{}{}
	var toolResults []UnifiedEntry
	dec := json.NewDecoder(strings.NewReader(string(body)))
	seen := map[string]*claudeMerged{}
	var order []string
	for dec.More() {
		start := dec.InputOffset()
		var entry claudeEntry
		if dec.Decode(&entry) != nil {
			continue
		}
		raw := body[start:dec.InputOffset()]
		if entry.IsSidechain && sidechainAgent == "" {
			if entry.SessionID != nil && *entry.SessionID != "" {
				sessionID = *entry.SessionID
			}
			sidechainAgent = "subagent"
			if entry.AgentID != nil && strings.TrimSpace(*entry.AgentID) != "" {
				sidechainAgent = strings.TrimSpace(*entry.AgentID)
			} else if stem := sidechainAgentFromStem(file); stem != "" {
				sidechainAgent = stem
			}
		}
		if entry.Type == "user" && len(entry.ToolUseResult) > 0 {
			// Explicit token metadata on a tool result counts; a char estimate
			// would double-count what the next assistant turn's input already
			// reports (upstream tokens#1011).
			if entry := claudeToolResultEntry(p, file, mtimeMS, wsKey, wsLabel, sessionID, sidechainAgent, raw); entry != nil {
				if _, seenKey := toolResultKeys[entry.DedupKey]; !seenKey {
					toolResultKeys[entry.DedupKey] = struct{}{}
					toolResults = append(toolResults, *entry)
				}
			}
			continue
		}
		if entry.Type != "assistant" || entry.Message == nil {
			continue
		}
		message := entry.Message
		model := strings.TrimSpace(message.Model)
		if strings.Contains(model, "<synthetic>") {
			continue // local error notices, not API usage
		}
		if message.Usage == nil {
			continue
		}
		usage := message.Usage
		tokens := usage.breakdown()
		if tokens.Total() == 0 {
			continue
		}
		if model == "" {
			model = "unknown"
		}
		provider := strings.TrimSpace(message.ProviderID)
		if provider == "" {
			provider = "anthropic"
		}
		ts := mtimeMS
		if parsed, valid := ParseRFC3339MS(entry.Timestamp); valid {
			ts = parsed
		}
		key := "message:" + message.ID
		if entry.RequestID != "" {
			key = message.ID + ":" + entry.RequestID
		}
		cache1h := claudeCacheWrite1h(raw)
		if existing, found := seen[key]; found {
			// Streaming duplicate: keep the most complete counts seen.
			existing.entry.Tokens.Input = maxInt64(existing.entry.Tokens.Input, tokens.Input)
			existing.entry.Tokens.Output = maxInt64(existing.entry.Tokens.Output, tokens.Output)
			existing.entry.Tokens.CacheRead = maxInt64(existing.entry.Tokens.CacheRead, tokens.CacheRead)
			existing.entry.Tokens.CacheWrite = maxInt64(existing.entry.Tokens.CacheWrite, tokens.CacheWrite)
			existing.entry.CacheWrite1h = maxInt64(existing.entry.CacheWrite1h, cache1h)
			continue
		}
		record := &claudeMerged{entry: UnifiedEntry{
			Client: p.Client(), ModelID: model, ProviderID: provider, SessionID: sessionID,
			WorkspaceKey: wsKey, WorkspaceLabel: wsLabel, TimestampMS: ts, Tokens: tokens,
			CacheWrite1h: cache1h, Agent: sidechainAgent, DedupKey: "claude:" + key,
		}}
		seen[key] = record
		order = append(order, key)
	}
	for _, key := range order {
		emit(seen[key].entry)
	}
	for _, entry := range toolResults {
		emit(entry)
	}
	return nil
}

// claudeToolResultEntry extracts one usage row from a user entry's
// toolUseResult when the tool reported explicit token counts as structured
// metadata (usage objects with total/input tokens). Plain text results and
// char estimates never count.
func claudeToolResultEntry(_ claudeParser, _ string, mtimeMS int64, wsKey, wsLabel, sessionID, agent string, raw []byte) *UnifiedEntry {
	var probe struct {
		Timestamp string          `json:"timestamp"`
		Result    json.RawMessage `json:"toolUseResult"`
	}
	if json.Unmarshal(raw, &probe) != nil {
		return nil
	}
	usage := findToolResultUsage(probe.Result)
	if usage == nil || usage.Total == nil {
		return nil
	}
	total := *usage.Total
	if total <= 0 || usage.Input == nil {
		return nil
	}
	ts := mtimeMS
	if parsed, valid := ParseRFC3339MS(probe.Timestamp); valid {
		ts = parsed
	}
	key := "claude:" + sessionID + ":tool_result:" + itoa(int(total)) + ":" + itoa(int(*usage.Input))
	return &UnifiedEntry{
		Client: "claude-code", ModelID: "unknown", ProviderID: "anthropic",
		SessionID: sessionID, WorkspaceKey: wsKey, WorkspaceLabel: wsLabel,
		TimestampMS: ts, Tokens: TokenBreakdown{Input: *usage.Input, Output: nonNeg(usage.Output)},
		Agent: agent, DedupKey: key,
	}
}

type toolResultUsageProbe struct {
	Input  *int64 `json:"input_tokens"`
	Output *int64 `json:"output_tokens"`
	Total  *int64 `json:"total_tokens"`
}

// findToolResultUsage walks the result JSON for a usage-shaped object.
func findToolResultUsage(raw json.RawMessage) *toolResultUsageProbe {
	var generic any
	if json.Unmarshal(raw, &generic) != nil {
		return nil
	}
	var walk func(value any) *toolResultUsageProbe
	walk = func(value any) *toolResultUsageProbe {
		switch typed := value.(type) {
		case map[string]any:
			for key, child := range typed {
				if key == "usage" {
					if probe := decodeToolUsage(child); probe != nil {
						return probe
					}
				}
				if found := walk(child); found != nil {
					return found
				}
			}
		case []any:
			for _, child := range typed {
				if found := walk(child); found != nil {
					return found
				}
			}
		}
		return nil
	}
	return walk(generic)
}

func decodeToolUsage(value any) *toolResultUsageProbe {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var probe toolResultUsageProbe
	if json.Unmarshal(encoded, &probe) != nil {
		return nil
	}
	return &probe
}

// claudeCacheWrite1h reads the 1-hour cache TTL share when the line carries
// it (Anthropic bills it above the 5-minute default); 0 otherwise.
func claudeCacheWrite1h(raw []byte) int64 {
	var probe struct {
		Message *struct {
			Usage *claudeUsage1h `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &probe) != nil || probe.Message == nil || probe.Message.Usage == nil {
		return 0
	}
	return nonNeg(probe.Message.Usage.Input1h)
}

// sidechainAgentFromStem resolves the subagent name from a sidechain file
// name (…-agent-<id>.jsonl) when the entry carries no agent id.
func sidechainAgentFromStem(file string) string {
	stem := strings.TrimSuffix(filepath.Base(file), ".jsonl")
	if idx := strings.LastIndex(stem, "-agent-"); idx >= 0 && idx+7 < len(stem) {
		return stem[idx+7:]
	}
	return ""
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

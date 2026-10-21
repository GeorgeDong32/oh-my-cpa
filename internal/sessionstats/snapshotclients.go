package sessionstats

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// cherrystudioParser reads CherryStudio's Claude-mirror transcripts under
// ~/Library/Application Support/CherryStudio/.claude/projects (macOS) and
// the V2 Data/Agents/.claude/projects tree; a same-name session in both
// roots is the migration's copy — V2 receives new writes and wins.
type cherrystudioParser struct{}

func cherrystudioRoots(home string) []string {
	return []string{
		home + "/Library/Application Support/CherryStudio/.claude/projects",
		home + "/Library/Application Support/CherryStudio/Data/Agents/.claude/projects",
		home + "/.CherryStudio/.claude/projects",
		home + "/.config/CherryStudio/.claude/projects",
		home + "/.local/share/CherryStudio/Data/Agents/.claude/projects",
		home + "/.config/CherryStudio/Data/Agents/.claude/projects",
	}
}

func (cherrystudioParser) Client() string { return "cherrystudio" }

func (cherrystudioParser) Roots(home string) []string { return cherrystudioRoots(home) }

func (cherrystudioParser) Discover(root string) ([]string, error) { return WalkJSONLFiles(root) }

func (cherrystudioParser) ParseFile(file string, mtimeMS int64, emit func(UnifiedEntry)) error {
	session := strings.TrimSuffix(filepath.Base(file), ".jsonl")
	// A complete row (message id + request id) teaches the uuid→identity
	// mapping; partial rows (uuid-only, or message-id-only) adopt a complete
	// row's key so a snapshot repeated in partial and full forms collapses
	// (upstream holds every row until EOF for the same reason).
	fullKeyByUUID := map[string]string{}
	type partialRow struct {
		entry UnifiedEntry
		uuid  string
	}
	var partials []partialRow
	forEachJSONLine(file, mtimeMS, func(raw []byte) {
		var record struct {
			Type      string `json:"type"`
			RequestID string `json:"requestId"`
			UUID      string `json:"uuid"`
			Timestamp string `json:"timestamp"`
			Message   *struct {
				ID        string `json:"id"`
				Model     string `json:"model"`
				RequestID string `json:"requestId"`
				Usage     *struct {
					Input      *int64 `json:"input_tokens"`
					Output     *int64 `json:"output_tokens"`
					CacheRead  *int64 `json:"cache_read_input_tokens"`
					CacheWrite *int64 `json:"cache_creation_input_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal(raw, &record) != nil || record.Type != "assistant" || record.Message == nil {
			return
		}
		usage := record.Message.Usage
		if usage == nil {
			return
		}
		model := strings.TrimSpace(record.Message.Model)
		if model == "" || model == "<synthetic>" || strings.EqualFold(model, "unknown") {
			return
		}
		// Anthropic-shaped usage reports input exclusive of cache buckets.
		tokens := TokenBreakdown{
			Input: nonNeg(usage.Input), Output: nonNeg(usage.Output),
			CacheRead: nonNeg(usage.CacheRead), CacheWrite: nonNeg(usage.CacheWrite),
		}
		if tokens.Total() <= 0 {
			return
		}
		ts := mtimeMS
		if parsed, ok := ParseRFC3339MS(record.Timestamp); ok {
			ts = parsed
		}
		requestID := strings.TrimSpace(record.RequestID)
		if requestID == "" {
			requestID = strings.TrimSpace(record.Message.RequestID)
		}
		messageID := strings.TrimSpace(record.Message.ID)
		uuid := strings.TrimSpace(record.UUID)
		entry := UnifiedEntry{
			Client: "cherrystudio", ModelID: model, ProviderID: providerForModelOr(model, "anthropic"),
			SessionID: session, TimestampMS: ts, Tokens: tokens,
		}
		if requestID != "" && messageID != "" {
			entry.DedupKey = "cherrystudio:" + messageID + ":" + requestID
			if uuid != "" {
				fullKeyByUUID[uuid] = entry.DedupKey
			}
			emit(entry)
			return
		}
		if uuid != "" {
			entry.DedupKey = "cherrystudio:uuid:" + uuid
		} else if messageID != "" {
			entry.DedupKey = "cherrystudio:message:" + messageID
		} else {
			return
		}
		partials = append(partials, partialRow{entry: entry, uuid: uuid})
	})
	for _, partial := range partials {
		if partial.uuid != "" {
			if full, known := fullKeyByUUID[partial.uuid]; known {
				partial.entry.DedupKey = full
			}
		}
		emit(partial.entry)
	}
	return nil
}

func providerForModelOr(model, fallback string) string {
	if provider := providerForModel(model); provider != "" {
		return provider
	}
	return fallback
}

// junieParser reads ~/.junie/sessions/<session-id>/events.jsonl. Usage rows
// arrive as LlmResponseMetadataEvents whose timestampMs is the call's END —
// usage.time (latency) back-anchors the start so spans do not leak forward.
type junieParser struct{}

const junieUsageKind = "LlmResponseMetadataEvent"

func (junieParser) Client() string { return "junie" }

func (junieParser) Roots(home string) []string { return []string{home + "/.junie/sessions"} }

func (junieParser) Discover(root string) ([]string, error) {
	return WalkFilesWithSuffix(root, "events.jsonl")
}

func (junieParser) ParseFile(file string, mtimeMS int64, emit func(UnifiedEntry)) error {
	session := dirBase(fileDir(file))
	forEachJSONLine(file, mtimeMS, func(raw []byte) {
		if !strings.Contains(string(raw), junieUsageKind) {
			return
		}
		var value struct {
			TimestampMS *int64 `json:"timestampMs"`
			Event       *struct {
				AgentEvent *struct {
					Kind       string `json:"kind"`
					ModelUsage []struct {
						Model        string   `json:"model"`
						InputTokens  *int64   `json:"inputTokens"`
						OutputTokens *int64   `json:"outputTokens"`
						CacheInput   *int64   `json:"cacheInputTokens"`
						CacheWrite   *int64   `json:"cacheCreateTokens"`
						Reasoning    *int64   `json:"reasoningTokens"`
						Time         *float64 `json:"time"`
						Cost         *float64 `json:"cost"`
					} `json:"modelUsage"`
				} `json:"agentEvent"`
			} `json:"event"`
		}
		if json.Unmarshal(raw, &value) != nil || value.Event == nil ||
			value.Event.AgentEvent == nil || value.Event.AgentEvent.Kind != junieUsageKind {
			return
		}
		explicit := int64(0)
		if value.TimestampMS != nil && *value.TimestampMS > 0 {
			explicit = *value.TimestampMS
		}
		timestamp := mtimeMS
		if explicit > 0 {
			timestamp = explicit
		}
		for index, usage := range value.Event.AgentEvent.ModelUsage {
			model := strings.TrimSpace(usage.Model)
			if model == "" {
				continue
			}
			tokens := TokenBreakdown{
				Input: nonNeg(usage.InputTokens), Output: nonNeg(usage.OutputTokens),
				CacheRead: nonNeg(usage.CacheInput), CacheWrite: nonNeg(usage.CacheWrite),
				Reasoning: nonNeg(usage.Reasoning),
			}
			cost := 0.0
			if usage.Cost != nil && *usage.Cost >= 0 {
				cost = *usage.Cost
			}
			if tokens.Total() == 0 && cost == 0 {
				continue
			}
			// The response timestamp is the call's end; latency back-anchors
			// the start so sessionize spans do not project phantom idle time.
			anchored := timestamp
			if explicit > 0 && usage.Time != nil && *usage.Time > 0 {
				anchored = explicit - int64(*usage.Time*1000)
			}
			key := "junie:" + session + ":" + strconv.FormatInt(timestamp, 10) + ":" + model +
				":" + strconv.FormatInt(tokens.Input, 10) + ":" + strconv.FormatInt(tokens.Output, 10) +
				":" + strconv.FormatInt(tokens.CacheRead, 10) + ":" + strconv.FormatInt(tokens.CacheWrite, 10) +
				":" + strconv.FormatInt(tokens.Reasoning, 10) + ":" + itoa(index)
			emit(UnifiedEntry{
				Client: "junie", ModelID: model, ProviderID: providerForModelOr(model, "junie"),
				SessionID: session, TimestampMS: anchored, Tokens: tokens, DedupKey: key,
			})
		}
	})
	return nil
}

// opencodereviewParser reads ~/.opencodereview/sessions/*.jsonl: llm_response
// records with usage; the record timestamp is the response end, duration_ms
// back-anchors the start.
type opencodereviewParser struct{}

func (opencodereviewParser) Client() string { return "opencodereview" }

func (opencodereviewParser) Roots(home string) []string {
	return []string{home + "/.opencodereview/sessions"}
}

func (opencodereviewParser) Discover(root string) ([]string, error) { return WalkJSONLFiles(root) }

func (opencodereviewParser) ParseFile(file string, mtimeMS int64, emit func(UnifiedEntry)) error {
	session := strings.TrimSuffix(filepath.Base(file), ".jsonl")
	forEachJSONLine(file, mtimeMS, func(raw []byte) {
		if !strings.Contains(string(raw), "llm_response") {
			return
		}
		var value struct {
			Type       string          `json:"type"`
			Usage      json.RawMessage `json:"usage"`
			Timestamp  string          `json:"timestamp"`
			Model      string          `json:"model"`
			DurationMS *int64          `json:"duration_ms"`
		}
		if json.Unmarshal(raw, &value) != nil || value.Type != "llm_response" || len(value.Usage) == 0 {
			return
		}
		var usage struct {
			PromptTokens *int64 `json:"input_tokens"`
			InputTokens  *int64 `json:"inputTokens"`
			Prompt       *int64 `json:"prompt_tokens"`
			Output       *int64 `json:"output_tokens"`
			OutputAlt    *int64 `json:"outputTokens"`
			CacheRead    *int64 `json:"cache_read_input_tokens"`
			CacheWrite   *int64 `json:"cache_creation_input_tokens"`
			Reasoning    *int64 `json:"reasoning_tokens"`
		}
		if json.Unmarshal(value.Usage, &usage) != nil {
			return
		}
		tokens := TokenBreakdown{
			Input:     firstPositive(usage.PromptTokens, usage.InputTokens, usage.Prompt),
			Output:    firstPositive(usage.Output, usage.OutputAlt),
			CacheRead: nonNeg(usage.CacheRead), CacheWrite: nonNeg(usage.CacheWrite), Reasoning: nonNeg(usage.Reasoning),
		}
		if tokens.Total() == 0 {
			return
		}
		model := strings.TrimSpace(value.Model)
		if model == "" {
			model = "unknown"
		}
		timestamp := mtimeMS
		explicit := int64(0)
		if parsed, ok := ParseRFC3339MS(value.Timestamp); ok {
			timestamp, explicit = parsed, parsed
		}
		if explicit > 0 && value.DurationMS != nil && *value.DurationMS > 0 {
			timestamp = explicit - *value.DurationMS
		}
		emit(UnifiedEntry{
			Client: "opencodereview", ModelID: model,
			ProviderID: providerForModelOr(model, "opencodereview"),
			SessionID:  session, TimestampMS: timestamp, Tokens: tokens,
			DedupKey: "opencodereview:" + session + ":" + strconv.FormatInt(timestamp, 10) + ":" + model,
		})
	})
	return nil
}

// jcodeParser reads ~/.jcode/sessions/session_*.json snapshots plus their
// append-only *.journal.jsonl sidecars (recent changes live in the journal
// until the next snapshot; journal rows replace, not add, snapshot rows).
type jcodeParser struct{}

type jcodeTokenUsage struct {
	Input      *int64 `json:"input_tokens"`
	Output     *int64 `json:"output_tokens"`
	CacheRead  *int64 `json:"cache_read_input_tokens"`
	CacheWrite *int64 `json:"cache_creation_input_tokens"`
	Reasoning  *int64 `json:"reasoning_output_tokens"`
}

type jcodeMessage struct {
	ID             *string          `json:"id"`
	Role           *string          `json:"role"`
	Timestamp      *string          `json:"timestamp"`
	TokenUsage     *jcodeTokenUsage `json:"token_usage"`
	ToolDurationMS *int64           `json:"tool_duration_ms"`
}

func (u *jcodeTokenUsage) breakdown() (TokenBreakdown, bool) {
	if u == nil {
		return TokenBreakdown{}, false
	}
	input := nonNeg(u.Input)
	cacheRead := nonNeg(u.CacheRead)
	// Upstream splits the cache buckets out of input when the record omits
	// cache_creation_input_tokens (the ambiguous-input case) or the cache
	// read exceeds the plain input (proof the input is cache-inclusive).
	if u.CacheWrite == nil || cacheRead > input {
		input = clampNonNeg(input - cacheRead - nonNeg(u.CacheWrite))
	}
	tokens := TokenBreakdown{
		Input: input, Output: nonNeg(u.Output),
		CacheRead: cacheRead, CacheWrite: nonNeg(u.CacheWrite),
		Reasoning: nonNeg(u.Reasoning),
	}
	return tokens, tokens.Total() > 0
}

type jcodeSession struct {
	ID          *string        `json:"id"`
	ProviderKey *string        `json:"provider_key"`
	Model       *string        `json:"model"`
	WorkingDir  *string        `json:"working_dir"`
	Messages    []jcodeMessage `json:"messages"`
}

func (jcodeParser) Client() string { return "jcode" }

func (jcodeParser) Roots(home string) []string { return []string{home + "/.jcode/sessions"} }

func (jcodeParser) Discover(root string) ([]string, error) {
	files, err := WalkFilesWithSuffix(root, ".json")
	if err != nil {
		return nil, err
	}
	kept := files[:0]
	for _, file := range files {
		if !strings.HasSuffix(filepath.Base(file), ".journal.jsonl") &&
			strings.HasPrefix(filepath.Base(file), "session_") {
			kept = append(kept, file)
		}
	}
	return kept, nil
}

func (jcodeParser) ParseFile(file string, mtimeMS int64, emit func(UnifiedEntry)) error {
	body, ok := readFileLimited(file, maxFileBytes)
	if !ok {
		return nil
	}
	var session jcodeSession
	if json.Unmarshal(body, &session) != nil {
		return nil
	}
	sessionID := strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))
	if session.ID != nil && strings.TrimSpace(*session.ID) != "" {
		sessionID = strings.TrimSpace(*session.ID)
	}
	provider := "jcode"
	if session.ProviderKey != nil && strings.TrimSpace(*session.ProviderKey) != "" {
		provider = canonicalProvider(strings.TrimSpace(*session.ProviderKey))
	}
	model := "unknown"
	if session.Model != nil && strings.TrimSpace(*session.Model) != "" {
		model = strings.TrimSpace(*session.Model)
	}
	wsKey, wsLabel := "", ""
	if session.WorkingDir != nil {
		if key := NormalizeWorkspaceKey(*session.WorkingDir); key != "" {
			wsKey, wsLabel = key, WorkspaceLabelFromKey(key)
		}
	}
	// Snapshot rows first; journal rows with the same dedup key replace them
	// (upstream's known-keys pass) — emit collectors keep the max per key.
	for _, message := range session.Messages {
		emitJcodeMessage("jcode", sessionID, provider, model, wsKey, wsLabel, mtimeMS, message, emit)
	}
	journal := strings.TrimSuffix(file, ".json") + ".journal.jsonl"
	if _, err := os.Stat(journal); err != nil {
		return nil
	}
	var journalState = struct {
		provider string
		model    string
		wsKey    string
		wsLabel  string
	}{provider, model, wsKey, wsLabel}
	forEachJSONLine(journal, mtimeMS, func(raw []byte) {
		var entry struct {
			Meta *struct {
				ProviderKey *string `json:"provider_key"`
				Model       *string `json:"model"`
				WorkingDir  *string `json:"working_dir"`
			} `json:"meta"`
			AppendMessages []jcodeMessage `json:"append_messages"`
		}
		if json.Unmarshal(raw, &entry) != nil {
			return
		}
		if entry.Meta != nil {
			if entry.Meta.Model != nil && strings.TrimSpace(*entry.Meta.Model) != "" {
				journalState.model = strings.TrimSpace(*entry.Meta.Model)
			}
			if entry.Meta.ProviderKey != nil && strings.TrimSpace(*entry.Meta.ProviderKey) != "" {
				journalState.provider = canonicalProvider(strings.TrimSpace(*entry.Meta.ProviderKey))
			}
			if entry.Meta.WorkingDir != nil {
				if key := NormalizeWorkspaceKey(*entry.Meta.WorkingDir); key != "" {
					journalState.wsKey, journalState.wsLabel = key, WorkspaceLabelFromKey(key)
				}
			}
		}
		for _, message := range entry.AppendMessages {
			emitJcodeMessage("jcode", sessionID, journalState.provider, journalState.model,
				journalState.wsKey, journalState.wsLabel, mtimeMS, message, emit)
		}
	})
	return nil
}

func emitJcodeMessage(client, sessionID, provider, model, wsKey, wsLabel string, mtimeMS int64, message jcodeMessage, emit func(UnifiedEntry)) {
	tokens, ok := message.TokenUsage.breakdown()
	if !ok {
		return
	}
	messageID := ""
	if message.ID != nil {
		messageID = strings.TrimSpace(*message.ID)
	}
	timestamp := mtimeMS
	if message.Timestamp != nil {
		if parsed, valid := ParseRFC3339MS(*message.Timestamp); valid {
			timestamp = parsed
			if message.ToolDurationMS != nil && *message.ToolDurationMS > 0 {
				timestamp = parsed - *message.ToolDurationMS
			}
		}
	}
	key := "jcode:" + sessionID + ":snapshot:" + itoa(int(mtimeMS)) + ":" + messageID
	if messageID != "" {
		key = "jcode:" + sessionID + ":" + messageID
	}
	emit(UnifiedEntry{
		Client: client, ModelID: model, ProviderID: provider, SessionID: sessionID,
		WorkspaceKey: wsKey, WorkspaceLabel: wsLabel, TimestampMS: timestamp,
		Tokens: tokens, DedupKey: key,
	})
}

// augmentParser reads ~/.augment/sessions/<sessionId>.json: one snapshot per
// session with turns; only completed turns count, priced off the LAST
// response node's token_usage.
type augmentParser struct{}

func (augmentParser) Client() string { return "augment" }

func (augmentParser) Roots(home string) []string { return []string{home + "/.augment/sessions"} }

func (augmentParser) Discover(root string) ([]string, error) {
	return WalkFilesWithSuffix(root, ".json")
}

func (augmentParser) ParseFile(file string, mtimeMS int64, emit func(UnifiedEntry)) error {
	body, ok := readFileLimited(file, maxFileBytes)
	if !ok {
		return nil
	}
	var session struct {
		SessionID  string `json:"sessionId"`
		AgentState *struct {
			ModelID string `json:"modelId"`
		} `json:"agent_state"`
		Turns []struct {
			FinishedAt *string `json:"finishedAt"`
			Completed  *bool   `json:"completed"`
			Exchange   *struct {
				ModelID       string `json:"model_id"`
				RequestID     string `json:"request_id"`
				ResponseNodes []struct {
					TokenUsage *struct {
						Input      *int64 `json:"input_tokens"`
						Output     *int64 `json:"output_tokens"`
						CacheRead  *int64 `json:"cache_read_input_tokens"`
						CacheWrite *int64 `json:"cache_creation_input_tokens"`
					} `json:"token_usage"`
				} `json:"response_nodes"`
			} `json:"exchange"`
			SequenceID json.RawMessage `json:"sequenceId"`
		} `json:"turns"`
	}
	if json.Unmarshal(body, &session) != nil {
		return nil
	}
	sessionID := strings.TrimSpace(session.SessionID)
	if sessionID == "" {
		sessionID = strings.TrimSuffix(filepath.Base(file), ".json")
	}
	defaultModel := "unknown"
	if session.AgentState != nil && strings.TrimSpace(session.AgentState.ModelID) != "" {
		defaultModel = strings.TrimSpace(session.AgentState.ModelID)
	}
	for index, turn := range session.Turns {
		if turn.Completed == nil || !*turn.Completed {
			continue
		}
		if turn.Exchange == nil || len(turn.Exchange.ResponseNodes) == 0 {
			continue
		}
		var usage *struct {
			Input      *int64 `json:"input_tokens"`
			Output     *int64 `json:"output_tokens"`
			CacheRead  *int64 `json:"cache_read_input_tokens"`
			CacheWrite *int64 `json:"cache_creation_input_tokens"`
		}
		for i := len(turn.Exchange.ResponseNodes) - 1; i >= 0; i-- {
			if node := turn.Exchange.ResponseNodes[i].TokenUsage; node != nil {
				usage = node
				break
			}
		}
		if usage == nil {
			continue
		}
		tokens := TokenBreakdown{Input: nonNeg(usage.Input), Output: nonNeg(usage.Output), CacheRead: nonNeg(usage.CacheRead), CacheWrite: nonNeg(usage.CacheWrite)}
		if tokens.Total() == 0 {
			continue
		}
		model := defaultModel
		if strings.TrimSpace(turn.Exchange.ModelID) != "" {
			model = strings.TrimSpace(turn.Exchange.ModelID)
		}
		timestamp := mtimeMS
		if turn.FinishedAt != nil {
			if parsed, valid := ParseRFC3339MS(*turn.FinishedAt); valid {
				timestamp = parsed
			}
		}
		key := "augment:" + sessionID + ":turn:" + itoa(index)
		if requestID := strings.TrimSpace(turn.Exchange.RequestID); requestID != "" {
			key = "augment:" + sessionID + ":req:" + requestID
		} else if len(turn.SequenceID) > 0 && string(turn.SequenceID) != "null" {
			var seqString string
			if json.Unmarshal(turn.SequenceID, &seqString) == nil && strings.TrimSpace(seqString) != "" {
				key = "augment:" + sessionID + ":seq:" + seqString
			} else if raw := strings.Trim(string(turn.SequenceID), "\""); raw != "" && raw != "null" {
				key = "augment:" + sessionID + ":seq:" + raw
			}
		}
		emit(UnifiedEntry{
			Client: "augment", ModelID: model, ProviderID: providerForModelOr(model, "augment"),
			SessionID: sessionID, TimestampMS: timestamp, Tokens: tokens, DedupKey: key,
		})
	}
	return nil
}

// fxParser reads ~/.fx/sessions/<id>/usage-v2.json (one cumulative snapshot
// per session, per-model buckets) with session.json carrying the timestamp
// and workspace.
type fxParser struct{}

func (fxParser) Client() string { return "fx" }

func (fxParser) Roots(home string) []string { return []string{home + "/.fx/sessions"} }

func (fxParser) Discover(root string) ([]string, error) {
	return WalkFilesWithSuffix(root, "usage-v2.json")
}

func (fxParser) ParseFile(file string, mtimeMS int64, emit func(UnifiedEntry)) error {
	body, ok := readFileLimited(file, maxFileBytes)
	if !ok {
		return nil
	}
	var usage struct {
		SessionID string `json:"session_id"`
		Snapshot  *struct {
			Models []struct {
				Model        string `json:"model"`
				InputTokens  int64  `json:"input_tokens"`
				OutputTokens int64  `json:"output_tokens"`
				CacheRead    int64  `json:"cache_read_tokens"`
				CacheWrite   int64  `json:"cache_write_tokens"`
				Reasoning    *int64 `json:"reasoning_tokens"`
				RequestCount *int64 `json:"request_count"`
			} `json:"models"`
		} `json:"snapshot"`
	}
	if json.Unmarshal(body, &usage) != nil || usage.Snapshot == nil {
		return nil
	}
	sessionDir := fileDir(file)
	sessionID := strings.TrimSpace(usage.SessionID)
	if sessionID == "" {
		sessionID = dirBase(sessionDir)
	}
	timestamp := mtimeMS
	wsKey, wsLabel := "", ""
	if meta, ok := readFileLimited(sessionDir+"/session.json", maxFileBytes); ok {
		var metaValue struct {
			WorkspaceRoot string `json:"workspace_root"`
			CreatedAtMS   *int64 `json:"created_at_ms"`
			UpdatedAtMS   *int64 `json:"updated_at_ms"`
		}
		if json.Unmarshal(meta, &metaValue) == nil {
			if metaValue.UpdatedAtMS != nil && *metaValue.UpdatedAtMS > 0 {
				timestamp = *metaValue.UpdatedAtMS
			} else if metaValue.CreatedAtMS != nil && *metaValue.CreatedAtMS > 0 {
				timestamp = *metaValue.CreatedAtMS
			}
			if key := NormalizeWorkspaceKey(metaValue.WorkspaceRoot); key != "" {
				wsKey, wsLabel = key, WorkspaceLabelFromKey(key)
			}
		}
	}
	for _, modelUsage := range usage.Snapshot.Models {
		rawModel := strings.TrimSpace(modelUsage.Model)
		if rawModel == "" {
			rawModel = "fx-unknown"
		}
		reasoning := int64(0)
		if modelUsage.Reasoning != nil {
			reasoning = clampNonNeg(*modelUsage.Reasoning)
		}
		tokens := TokenBreakdown{
			Input: clampNonNeg(modelUsage.InputTokens), Output: clampNonNeg(modelUsage.OutputTokens),
			CacheRead: clampNonNeg(modelUsage.CacheRead), CacheWrite: clampNonNeg(modelUsage.CacheWrite), Reasoning: reasoning,
		}
		if tokens.Total() == 0 {
			continue
		}
		provider, model := "", rawModel
		if idx := strings.Index(rawModel, "/"); idx > 0 {
			provider, model = rawModel[:idx], rawModel[idx+1:]
			provider = canonicalProvider(provider)
		}
		if provider == "" {
			provider = providerForModelOr(model, "zai")
		}
		messages := int64(0)
		if modelUsage.RequestCount != nil {
			messages = clampNonNeg(*modelUsage.RequestCount)
		}
		emit(UnifiedEntry{
			Client: "fx", ModelID: model, ProviderID: provider, SessionID: sessionID,
			WorkspaceKey: wsKey, WorkspaceLabel: wsLabel, TimestampMS: timestamp,
			Tokens: tokens, MessageCount: int32(messages),
			DedupKey: "fx:" + sessionID + ":" + model,
		})
	}
	return nil
}

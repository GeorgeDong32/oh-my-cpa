package sessionstats

import (
	"bytes"
	"encoding/json"
	"io"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// dshParser reads DeepSeek Harness session transcripts
// (~/.dsh/sessions/<id>/transcript.jsonl), optionally zstd-frame compressed
// with one frame per flush — a torn trailing frame recovers its prefix the
// same way DSH itself does.
type dshParser struct{}

const (
	dshZSTDMagic    = "\x28\xb5\x2f\xfd"
	dshMaxFileBytes = 64 << 20
	dshMaxDecoded   = 64 << 20
)

func (dshParser) Client() string { return "dsh" }

func (dshParser) Roots(home string) []string { return []string{home + "/.dsh/sessions"} }

// Discover accepts both spellings of the DSH session log: session.jsonl and
// the versioned session.v<N>.jsonl, each optionally zstd-compressed.
func (dshParser) Discover(root string) ([]string, error) {
	files, err := WalkFilesWithSuffix(root, ".jsonl")
	if err != nil {
		return nil, err
	}
	zstFiles, err := WalkFilesWithSuffix(root, ".jsonl.zstd")
	if err == nil {
		files = append(files, zstFiles...)
	}
	kept := files[:0]
	for _, file := range files {
		base := filepath.Base(file)
		base = strings.TrimSuffix(base, ".zstd")
		if base == "session.jsonl" {
			kept = append(kept, file)
			continue
		}
		rest := strings.TrimPrefix(base, "session.v")
		if rest != base && strings.HasSuffix(rest, ".jsonl") {
			version := strings.TrimSuffix(rest, ".jsonl")
			if version != "" && isAllDigits(version) {
				kept = append(kept, file)
			}
		}
	}
	return kept, nil
}

func isAllDigits(value string) bool {
	for _, ch := range value {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

func (dshParser) ParseFile(file string, mtimeMS int64, emit func(UnifiedEntry)) error {
	decoded, ok := readDSHTranscript(file)
	if !ok || len(decoded) == 0 {
		return nil
	}
	sessionFromPath := dirBase(fileDir(file))
	session := sessionFromPath
	wsKey, wsLabel := "", ""
	seedLength := int64(0)
	fallbackModel := ""
	fallbackProvider := ""
	seen := map[string]struct{}{}
	for _, line := range strings.Split(strings.TrimPrefix(string(decoded), "\xef\xbb\xbf"), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		var value struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			CWD  string `json:"cwd"`
			Seed *int64 `json:"seedLength"`
			Seq  *int64 `json:"seq"`
			Time *int64 `json:"time"`
			Data *struct {
				Header *struct {
					Config *struct {
						Provider string `json:"provider"`
						Model    string `json:"model"`
					} `json:"config"`
				} `json:"header"`
				Usage *struct {
					Input      *int64 `json:"inputTokens"`
					Output     *int64 `json:"outputTokens"`
					CacheRead  *int64 `json:"cacheReadTokens"`
					CacheWrite *int64 `json:"cacheWriteTokens"`
					Reasoning  *int64 `json:"reasoningTokens"`
				} `json:"usage"`
				Message *struct {
					ID     string `json:"id"`
					Source *struct {
						Model         string `json:"model"`
						ResponseModel string `json:"responseModel"`
						Provider      string `json:"provider"`
					} `json:"source"`
				} `json:"message"`
				CompactionID string `json:"compactionId"`
			} `json:"data"`
		}
		if json.Unmarshal([]byte(line), &value) != nil {
			continue
		}
		switch value.Type {
		case "session":
			if strings.TrimSpace(value.ID) != "" {
				session = strings.TrimSpace(value.ID)
			}
			if key := NormalizeWorkspaceKey(value.CWD); key != "" {
				wsKey, wsLabel = key, WorkspaceLabelFromKey(key)
			}
			if value.Seed != nil && *value.Seed > 0 {
				seedLength = *value.Seed
			}
		case "request/header":
			if value.Data != nil && value.Data.Header != nil && value.Data.Header.Config != nil {
				if model := strings.TrimSpace(value.Data.Header.Config.Model); model != "" {
					fallbackModel = model
				}
				if provider := strings.TrimSpace(value.Data.Header.Config.Provider); provider != "" {
					fallbackProvider = provider
				}
			}
		case "assistant/message", "compaction/summary":
			isSummary := value.Type == "compaction/summary"
			// Fork ownership: events before seedLength were inherited verbatim
			// from the parent session; counting them bills the parent twice.
			if seedLength > 0 && value.Seq != nil && *value.Seq < seedLength {
				continue
			}
			if value.Data == nil || value.Data.Usage == nil {
				continue
			}
			usage := value.Data.Usage
			reasoning := nonNeg(usage.Reasoning)
			tokens := TokenBreakdown{
				Input: nonNeg(usage.Input), Output: clampNonNeg(nonNeg(usage.Output) - reasoning),
				CacheRead: nonNeg(usage.CacheRead), CacheWrite: nonNeg(usage.CacheWrite),
				Reasoning: reasoning,
			}
			if tokens.Total() == 0 || value.Time == nil || *value.Time <= 0 {
				continue
			}
			identity := ""
			servedModel := ""
			providerID := ""
			if value.Data.Message != nil {
				if value.Data.Message.Source != nil {
					if responseModel := strings.TrimSpace(value.Data.Message.Source.ResponseModel); responseModel != "" {
						servedModel = responseModel
					} else if model := strings.TrimSpace(value.Data.Message.Source.Model); model != "" {
						servedModel = model
					}
					providerID = strings.TrimSpace(value.Data.Message.Source.Provider)
				}
				if servedModel != "" {
					identity = "msg:" + servedModel
				} else if strings.TrimSpace(value.Data.Message.ID) != "" {
					identity = "msg:" + strings.TrimSpace(value.Data.Message.ID)
				}
			}
			if identity == "" && isSummary && strings.TrimSpace(value.Data.CompactionID) != "" {
				identity = "cmp:" + strings.TrimSpace(value.Data.CompactionID)
			}
			if identity == "" && value.Seq != nil {
				identity = "seq:" + strconv.FormatInt(*value.Seq, 10)
			}
			if identity == "" {
				identity = "sid:" + session
			}
			kind := ""
			if isSummary {
				kind = "summary:"
			}
			key := "dsh:" + kind + identity + ":" + strconv.FormatInt(*value.Time, 10) + ":" +
				strconv.FormatInt(tokens.Input, 10) + ":" + strconv.FormatInt(tokens.Output, 10) + ":" +
				strconv.FormatInt(tokens.CacheRead, 10) + ":" + strconv.FormatInt(tokens.CacheWrite, 10) + ":" +
				strconv.FormatInt(tokens.Reasoning, 10)
			if _, duplicate := seen[key]; duplicate {
				continue
			}
			seen[key] = struct{}{}
			model := servedModel
			if model == "" {
				model = fallbackModel
			}
			provider := providerID
			if provider == "" {
				provider = fallbackProvider
			}
			if provider == "" {
				provider = "deepseek"
			}
			emit(UnifiedEntry{
				Client: "dsh", ModelID: model, ProviderID: provider,
				SessionID: session, WorkspaceKey: wsKey, WorkspaceLabel: wsLabel,
				TimestampMS: *value.Time, Tokens: tokens, DedupKey: key,
			})
		}
	}
	return nil
}

// readDSHTranscript reads a transcript, streaming zstd frames so a torn
// trailing frame (a scan racing the writer) keeps its decoded prefix.
func readDSHTranscript(file string) ([]byte, bool) {
	raw, ok := readFileLimited(file, dshMaxFileBytes)
	if !ok {
		return nil, false
	}
	if len(raw) < 4 || string(raw[:4]) != dshZSTDMagic {
		if len(raw) > dshMaxDecoded {
			return nil, false
		}
		return raw, true
	}
	decoder, err := zstd.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, false
	}
	defer decoder.Close()
	// Cap the decode: zstd's expansion ratio is unbounded and a finished
	// buffer has already allocated whatever the frames chose.
	limited := io.LimitReader(decoder, dshMaxDecoded+1)
	decoded, err := io.ReadAll(limited)
	if err != nil && len(decoded) == 0 {
		return nil, false
	}
	if len(decoded) > dshMaxDecoded {
		return nil, false
	}
	return decoded, true
}

// codebuffParser reads ~/.config/manicode/projects/<project>/chats/<chatId>/
// chat-messages.json (Codebuff, formerly Manicode). freebuff shares the shape
// under the base2-free root agent and estimates tokens from message text.
type codebuffParser struct{}

type codebuffAssistantUsage struct {
	model      string
	credits    float64
	input      int64
	output     int64
	cacheRead  int64
	cacheWrite int64
}

func (u codebuffAssistantUsage) hasSignal() bool {
	return u.input > 0 || u.output > 0 || u.cacheRead > 0 || u.cacheWrite > 0 || u.credits > 0
}

func (codebuffParser) Client() string { return "codebuff" }

func (codebuffParser) Roots(home string) []string {
	// Channel roots: manicode, manicode-dev, manicode-staging.
	return []string{
		home + "/.config/manicode/projects",
		home + "/.config/manicode-dev/projects",
		home + "/.config/manicode-staging/projects",
	}
}

func (codebuffParser) Discover(root string) ([]string, error) {
	return WalkFilesWithSuffix(root, "chat-messages.json")
}

func (codebuffParser) ParseFile(file string, mtimeMS int64, emit func(UnifiedEntry)) error {
	parseCodebuffChat("codebuff", file, mtimeMS, false, emit)
	return nil
}

type freebuffParser struct{}

func (freebuffParser) Client() string { return "freebuff" }

func (freebuffParser) Roots(home string) []string {
	// Freebuff is not a separate program: it is Codebuff's free agent, so its
	// chats live in the manicode channel trees (gated by the base2-free root
	// agent at parse time).
	return []string{
		home + "/.config/manicode/projects",
		home + "/.config/manicode-dev/projects",
		home + "/.config/manicode-staging/projects",
	}
}

func (freebuffParser) Discover(root string) ([]string, error) {
	return WalkFilesWithSuffix(root, "chat-messages.json")
}

func (freebuffParser) ParseFile(file string, mtimeMS int64, emit func(UnifiedEntry)) error {
	parseCodebuffChat("freebuff", file, mtimeMS, true, emit)
	return nil
}

// parseCodebuffChat walks the chat array; assistant rows take usage from
// metadata.usage, metadata.codebuff.usage, or the stashed RunState history
// (where OpenRouter-routed calls land their final counts).
func parseCodebuffChat(client, file string, mtimeMS int64, freebuff bool, emit func(UnifiedEntry)) {
	body, ok := readFileLimited(file, maxFileBytes)
	if !ok {
		return
	}
	var messages []map[string]any
	if json.Unmarshal(body, &messages) != nil {
		return
	}
	if freebuff {
		// Belt and braces (upstream): only chats the base2-free root agent
		// ran count, AND any chat carrying authoritative provider usage is
		// left to the codebuff scan — the two clients share one tree.
		if !isFreebuffChat(messages) || chatHasAuthoritativeUsage(messages) {
			return
		}
	}
	chatID := dirBase(fileDir(file))
	// chats/<chatId>/chat-messages.json → project two dirs up.
	project := dirBase(fileDir(fileDir(fileDir(file))))
	session := client + "/" + project + "/" + chatID
	chatTS := parseChatIDToMillis(chatID)
	for ordinal, message := range messages {
		variant, _ := message["variant"].(string)
		if variant == "" {
			variant, _ = message["role"].(string)
		}
		if variant != "ai" && variant != "agent" && variant != "assistant" {
			continue
		}
		usage := extractCodebuffUsage(message)
		if freebuff && usage.input == 0 && usage.output == 0 {
			// Freebuff rows carry no provider usage; estimate from text length.
			usage.input = estimateTokensFromChars(codebuffMessageChars(message))
		}
		if !usage.hasSignal() {
			continue
		}
		ts := mtimeMS
		if stamp := codebuffMessageTimestamp(message); stamp > 0 {
			ts = stamp
		} else if chatTS > 0 {
			ts = chatTS
		}
		model := usage.model
		if strings.TrimSpace(model) == "" {
			model = client + "-unknown"
		}
		key := ""
		if id, _ := message["id"].(string); strings.TrimSpace(id) != "" {
			key = client + ":" + id
		} else {
			key = client + ":" + session + ":" + strconv.FormatInt(ts, 10) + ":" + model + ":" + itoa(ordinal)
		}
		emit(UnifiedEntry{
			Client: client, ModelID: model, ProviderID: providerForModelOr(model, "unknown"),
			SessionID: session, TimestampMS: ts,
			Tokens: TokenBreakdown{
				Input: clampNonNeg(usage.input), Output: clampNonNeg(usage.output),
				CacheRead: clampNonNeg(usage.cacheRead), CacheWrite: clampNonNeg(usage.cacheWrite),
			},
			DedupKey: key,
		})
	}
}

// chatHasAuthoritativeUsage reports whether any assistant row carries a
// provider usage object (metadata.usage or metadata.codebuff.usage).
func chatHasAuthoritativeUsage(messages []map[string]any) bool {
	for _, message := range messages {
		variant, _ := message["variant"].(string)
		if variant == "" {
			variant, _ = message["role"].(string)
		}
		if variant != "ai" && variant != "agent" && variant != "assistant" {
			continue
		}
		if usage := extractCodebuffUsage(message); usage.hasSignal() {
			return true
		}
	}
	return false
}

// isFreebuffChat reports whether any message's run state names the free
// (base2-free) root agent.
func isFreebuffChat(messages []map[string]any) bool {
	for _, message := range messages {
		if agentType, ok := grokPathAny(message, "metadata", "runState", "sessionState", "mainAgentState", "agentType"); ok {
			if agent, ok := agentType.(string); ok && strings.HasPrefix(strings.TrimSpace(agent), "base2-free") {
				return true
			}
		}
	}
	return false
}

func extractCodebuffUsage(message map[string]any) codebuffAssistantUsage {
	usage := codebuffAssistantUsage{}
	metadata, _ := message["metadata"].(map[string]any)
	if metadata == nil {
		return usage
	}
	if model, _ := metadata["model"].(string); strings.TrimSpace(model) != "" {
		usage.model = strings.TrimSpace(model)
	}
	if object, ok := metadata["usage"].(map[string]any); ok {
		usage.merge(codebuffUsageObject(object))
	}
	if codebuff, ok := metadata["codebuff"].(map[string]any); ok {
		if object, ok := codebuff["usage"].(map[string]any); ok {
			usage.merge(codebuffUsageObject(object))
		}
	}
	if runState, ok := metadata["runState"].(map[string]any); ok {
		if sessionState, ok := runState["sessionState"].(map[string]any); ok {
			if mainAgent, ok := sessionState["mainAgentState"].(map[string]any); ok {
				if history, ok := mainAgent["messageHistory"].([]any); ok && len(history) > 0 {
					for i := len(history) - 1; i >= 0; i-- {
						entry, ok := history[i].(map[string]any)
						if !ok {
							continue
						}
						role, _ := entry["role"].(string)
						if role != "assistant" {
							continue
						}
						if providerOptions, ok := entry["providerOptions"].(map[string]any); ok {
							if object, ok := providerOptions["usage"].(map[string]any); ok {
								usage.merge(codebuffUsageObject(object))
								break
							}
							if codebuff, ok := providerOptions["codebuff"].(map[string]any); ok {
								if object, ok := codebuff["usage"].(map[string]any); ok {
									usage.merge(codebuffUsageObject(object))
									break
								}
							}
						}
					}
				}
			}
		}
	}
	return usage
}

func (u *codebuffAssistantUsage) merge(other codebuffAssistantUsage) {
	if u.input <= 0 {
		u.input = other.input
	}
	if u.output <= 0 {
		u.output = other.output
	}
	if u.cacheRead <= 0 {
		u.cacheRead = other.cacheRead
	}
	if u.cacheWrite <= 0 {
		u.cacheWrite = other.cacheWrite
	}
	if u.model == "" {
		u.model = other.model
	}
	if u.credits <= 0 {
		u.credits = other.credits
	}
}

func codebuffUsageObject(object map[string]any) codebuffAssistantUsage {
	usage := codebuffAssistantUsage{}
	number := func(keys ...string) int64 {
		for _, key := range keys {
			if raw, exists := object[key]; exists {
				if floated, ok := raw.(float64); ok && floated > 0 {
					return int64(floated)
				}
			}
		}
		return 0
	}
	usage.input = number("input_tokens", "inputTokens", "prompt_tokens")
	usage.output = number("output_tokens", "outputTokens", "completion_tokens")
	usage.cacheRead = number("cache_read_input_tokens", "cacheReadInputTokens")
	usage.cacheWrite = number("cache_creation_input_tokens", "cacheCreationInputTokens")
	if model, _ := object["model"].(string); strings.TrimSpace(model) != "" {
		usage.model = strings.TrimSpace(model)
	}
	return usage
}

func codebuffMessageTimestamp(message map[string]any) int64 {
	for _, key := range []string{"timestamp", "createdAt"} {
		if raw, exists := message[key]; exists {
			if ts, ok := anyTimestampMS(raw); ok {
				return ts
			}
		}
	}
	if metadata, ok := message["metadata"].(map[string]any); ok {
		if raw, exists := metadata["timestamp"]; exists {
			if ts, ok := anyTimestampMS(raw); ok {
				return ts
			}
		}
	}
	return 0
}

func anyTimestampMS(raw any) (int64, bool) {
	switch value := raw.(type) {
	case string:
		if ts, ok := ParseRFC3339MS(value); ok {
			return ts, true
		}
	case float64:
		if value > 0 {
			return int64(value), true
		}
	}
	return 0, false
}

func codebuffMessageChars(message map[string]any) int64 {
	chars := int64(0)
	if content, ok := message["content"].(string); ok {
		chars += int64(len([]rune(content)))
	}
	if blocks, ok := message["blocks"].([]any); ok {
		for _, block := range blocks {
			if entry, ok := block.(map[string]any); ok {
				if content, ok := entry["content"].(string); ok {
					chars += int64(len([]rune(content)))
				}
			}
		}
	}
	return chars
}

func estimateTokensFromChars(chars int64) int64 {
	// Upstream estimate_tokens: ~4 characters per token.
	if chars <= 0 {
		return 0
	}
	tokens := chars / 4
	if tokens == 0 {
		tokens = 1
	}
	return tokens
}

// parseChatIDToMillis rebuilds the ISO timestamp Codebuff encodes in a
// filesystem-safe chat id (HH-MM-SS after the T). Only those two separators
// flip back; the date keeps its dashes.
func parseChatIDToMillis(chatID string) int64 {
	tIndex := strings.Index(chatID, "T")
	if tIndex < 0 {
		return 0
	}
	time := chatID[tIndex:]
	time = strings.Replace(time, "-", ":", 2)
	if ts, ok := ParseRFC3339MS(chatID[:tIndex] + time); ok {
		return ts
	}
	return 0
}

package sessionstats

// Qwen CLI: ~/.qwen/projects/<workspace>/chats/*.jsonl — Gemini-style
// usageMetadata on assistant lines (sessions/qwen.rs).

import (
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
)

// maxFileBytes is the scan's hard per-file cap (spec §4: 512 MiB; upstream
// caps individual blobs at 32 MiB).
const maxFileBytes = 512 << 20

type qwenParser struct{ home string }

func (qwenParser) Client() string { return "qwen" }

func (p qwenParser) Roots(home string) []string {
	return []string{filepath.Join(home, ".qwen", "projects")}
}

func (p qwenParser) Discover(root string) ([]string, error) { return WalkJSONLFiles(root) }

type qwenLine struct {
	Type      string     `json:"type"`
	Model     string     `json:"model"`
	Timestamp string     `json:"timestamp"`
	SessionID string     `json:"sessionId"`
	Usage     *qwenUsage `json:"usageMetadata"`
}

type qwenUsage struct {
	PromptTokenCount     *int64 `json:"promptTokenCount"`
	CandidatesTokenCount *int64 `json:"candidatesTokenCount"`
	ThoughtsTokenCount   *int64 `json:"thoughtsTokenCount"`
	CachedContentTokens  *int64 `json:"cachedContentTokenCount"`
}

func (p qwenParser) ParseFile(file string, mtimeMS int64, emit func(UnifiedEntry)) error {
	body, ok := readFileLimited(file, maxFileBytes)
	if !ok {
		return nil // missing or oversized: scanner marks via size cap separately
	}
	wsKey, wsLabel := WorkspaceAfterMarker(file, "projects", "chats")
	fallbackSession := strings.TrimSuffix(filepath.Base(file), ".jsonl")
	messageIndex := 0
	dec := json.NewDecoder(strings.NewReader(string(body)))
	for {
		var line qwenLine
		if err := dec.Decode(&line); err != nil {
			break
		}
		if line.Type != "assistant" || line.Usage == nil {
			continue
		}
		usage := line.Usage
		tokens := TokenBreakdown{
			Input:     nonNeg(usage.PromptTokenCount),
			Output:    nonNeg(usage.CandidatesTokenCount),
			CacheRead: nonNeg(usage.CachedContentTokens),
			Reasoning: nonNeg(usage.ThoughtsTokenCount),
		}
		if tokens.Input+tokens.Output+tokens.CacheRead+tokens.Reasoning == 0 {
			continue
		}
		ts := mtimeMS
		if parsed, valid := ParseRFC3339MS(line.Timestamp); valid {
			ts = parsed
		}
		session := line.SessionID
		if strings.TrimSpace(session) == "" {
			session = fallbackSession
		}
		model := strings.TrimSpace(line.Model)
		if model == "" {
			model = "unknown"
		}
		emit(UnifiedEntry{
			Client: p.Client(), ModelID: model, ProviderID: "qwen", SessionID: session,
			WorkspaceKey: wsKey, WorkspaceLabel: wsLabel, TimestampMS: ts, Tokens: tokens,
			DedupKey: "qwen:" + session + ":" + itoa(messageIndex),
		})
		messageIndex++
	}
	return nil
}

func nonNeg(value *int64) int64 {
	if value == nil || *value < 0 {
		return 0
	}
	return *value
}

func itoa(v int) string { return strconv.Itoa(v) }

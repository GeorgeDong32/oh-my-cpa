package sessionstats

// Kimi CLI / Kimi Code: ~/.kimi/sessions/<group>/<session>/wire.jsonl
// (sessions/kimi.rs). Two wire generations share the file name: legacy Kimi
// CLI nests type under message (StatusUpdate payloads with token_usage and
// message_id), Kimi Code puts type at the top level (usage.record lines,
// turn-scoped only — step.end duplicates them and session-scoped records are
// aggregates).

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type kimiParser struct{}

func (kimiParser) Client() string { return "kimi" }

func (p kimiParser) Roots(home string) []string {
	return []string{filepath.Join(home, ".kimi", "sessions")}
}

func (p kimiParser) Discover(root string) ([]string, error) {
	if root == "" {
		return nil, nil
	}
	var files []string
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return nil
		}
		if entry.Name() == "wire.jsonl" {
			files = append(files, path)
		}
		return nil
	})
	sort.Strings(files)
	return files, nil
}

type kimiTokensMixed struct {
	InputOther       *int64 `json:"inputOther"`
	InputOtherSnake  *int64 `json:"input_other"`
	Output           *int64 `json:"output"`
	InputCacheRead   *int64 `json:"inputCacheRead"`
	InputCacheReadS  *int64 `json:"input_cache_read"`
	InputCacheCreate *int64 `json:"inputCacheCreation"`
	InputCacheCreatS *int64 `json:"input_cache_creation"`
}

func (u *kimiTokensMixed) breakdown() (TokenBreakdown, bool) {
	if u == nil {
		return TokenBreakdown{}, false
	}
	tokens := TokenBreakdown{
		Input:      firstPositive(u.InputOther, u.InputOtherSnake),
		Output:     nonNeg(u.Output),
		CacheRead:  firstPositive(u.InputCacheRead, u.InputCacheReadS),
		CacheWrite: firstPositive(u.InputCacheCreate, u.InputCacheCreatS),
	}
	return tokens, tokens.Total() > 0
}

func firstPositive(values ...*int64) int64 {
	for _, value := range values {
		if value != nil && *value > 0 {
			return *value
		}
	}
	return 0
}

// kimiCodeLine is the Kimi Code wire shape.
type kimiCodeLine struct {
	Type       string           `json:"type"`
	Model      string           `json:"model"`
	Time       *int64           `json:"time"`
	UsageScope string           `json:"usageScope"`
	Usage      *kimiTokensMixed `json:"usage"`
}

// kimiLegacyLine is the legacy Kimi CLI wire shape.
type kimiLegacyLine struct {
	Timestamp *float64       `json:"timestamp"`
	Type      string         `json:"type"`
	Message   *kimiLegacyMsg `json:"message"`
}

type kimiLegacyMsg struct {
	Type    string             `json:"type"`
	Payload *kimiLegacyPayload `json:"payload"`
}

type kimiLegacyPayload struct {
	MessageID  string           `json:"message_id"`
	TokenUsage *kimiTokensMixed `json:"token_usage"`
}

func (p kimiParser) ParseFile(file string, mtimeMS int64, emit func(UnifiedEntry)) error {
	body, ok := readFileLimited(file, maxFileBytes)
	if !ok {
		return nil
	}
	session := "unknown"
	if parent := filepath.Dir(file); parent != "" {
		session = filepath.Base(parent)
	}
	// Legacy sessions read the model from the sibling config.json.
	legacyModel := readKimiConfigModel(file)
	latestModel := ""
	var pendingRequest *int64
	index := 0
	dec := json.NewDecoder(strings.NewReader(string(body)))
	for dec.More() {
		start := dec.InputOffset()
		var generic map[string]json.RawMessage
		if dec.Decode(&generic) != nil {
			continue
		}
		raw := body[start:dec.InputOffset()]
		if _, nested := generic["message"]; nested {
			// Legacy generation.
			var line kimiLegacyLine
			if json.Unmarshal(raw, &line) != nil || line.Message == nil || line.Message.Type != "StatusUpdate" {
				continue
			}
			payload := line.Message.Payload
			if payload == nil || payload.TokenUsage == nil {
				continue
			}
			tokens, hasTokens := payload.TokenUsage.breakdown()
			if !hasTokens {
				continue
			}
			ts := mtimeMS
			if line.Timestamp != nil && *line.Timestamp > 0 {
				ts = int64(*line.Timestamp * 1000)
			}
			key := "kimi:" + session + ":status:" + payload.MessageID
			if payload.MessageID == "" {
				key = "kimi:" + session + ":status:" + itoa(index)
				index++
			}
			emit(UnifiedEntry{
				Client: p.Client(), ModelID: legacyModel, ProviderID: "moonshot",
				SessionID: session, TimestampMS: ts, Tokens: tokens, DedupKey: key,
			})
			continue
		}
		// Kimi Code generation.
		var line kimiCodeLine
		if json.Unmarshal(raw, &line) != nil {
			continue
		}
		switch line.Type {
		case "llm.request":
			if model := concreteKimiModel(line.Model); model != "" {
				latestModel = model
			}
			pendingRequest = positive(line.Time)
		case "usage.record":
			if line.UsageScope != "turn" {
				continue
			}
			requestTime := pendingRequest
			pendingRequest = nil
			tokens, hasTokens := line.Usage.breakdown()
			if !hasTokens {
				continue
			}
			model := concreteKimiModel(line.Model)
			if model == "" {
				model = latestModel
			}
			if model == "" {
				model = "kimi-for-coding"
			}
			wireTime := positive(line.Time)
			duration, ts := int64(0), mtimeMS
			if requestTime != nil && wireTime != nil && *wireTime > *requestTime {
				duration = *wireTime - *requestTime
				ts = *requestTime
			} else if wireTime != nil {
				ts = *wireTime
			}
			emit(UnifiedEntry{
				Client: p.Client(), ModelID: model, ProviderID: "moonshot",
				SessionID: session, TimestampMS: ts, Tokens: tokens, DurationMS: duration,
				DedupKey: "kimi:" + session + ":turn:" + itoa(index),
			})
			index++
		}
	}
	return nil
}

// readKimiConfigModel reads the legacy CLI's config.json three levels above
// the session directory; empty falls back to the default model.
func readKimiConfigModel(wirePath string) string {
	sessions := filepath.Dir(filepath.Dir(filepath.Dir(wirePath)))
	content, err := os.ReadFile(filepath.Join(filepath.Dir(sessions), "config.json"))
	if err != nil {
		return "kimi-for-coding"
	}
	var config struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(content, &config) != nil || strings.TrimSpace(config.Model) == "" {
		return "kimi-for-coding"
	}
	return config.Model
}

// concreteKimiModel keeps only concrete model ids: symbolic config references
// (e.g. "$config.model") are not model names.
func concreteKimiModel(model string) string {
	model = strings.TrimSpace(model)
	if model == "" || strings.HasPrefix(model, "$") {
		return ""
	}
	return model
}

func positive(value *int64) *int64 {
	if value != nil && *value > 0 {
		v := *value
		return &v
	}
	return nil
}

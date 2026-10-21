package sessionstats

// Factory (droid): ~/.factory/sessions/<ws>/<uuid>.settings.json carries the
// session's cumulative token_usage, plus a sibling .jsonl for model fallback
// (sessions/droid.rs).

import (
	"encoding/json"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
)

type droidParser struct{}

func (droidParser) Client() string { return "droid" }

func (p droidParser) Roots(home string) []string {
	return []string{filepath.Join(home, ".factory", "sessions")}
}

func (p droidParser) Discover(root string) ([]string, error) {
	files, err := walkNamedSuffix(root, ".settings.json")
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}

func walkNamedSuffix(root, suffix string) ([]string, error) {
	if root == "" {
		return nil, nil
	}
	var files []string
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return nil
		}
		if strings.HasSuffix(entry.Name(), suffix) {
			files = append(files, path)
		}
		return nil
	})
	return files, nil
}

type droidSettings struct {
	Model        *string      `json:"model"`
	ProviderLock *string      `json:"providerLock"`
	TokenUsage   *droidTokens `json:"tokenUsage"`
}

type droidTokens struct {
	InputTokens   *int64 `json:"inputTokens"`
	OutputTokens  *int64 `json:"outputTokens"`
	CacheCreation *int64 `json:"cacheCreationTokens"`
	CacheRead     *int64 `json:"cacheReadTokens"`
	Thinking      *int64 `json:"thinkingTokens"`
}

func (p droidParser) ParseFile(file string, mtimeMS int64, emit func(UnifiedEntry)) error {
	body, ok := readFileLimited(file, maxFileBytes)
	if !ok {
		return nil
	}
	var settings droidSettings
	if json.Unmarshal(body, &settings) != nil || settings.TokenUsage == nil {
		return nil
	}
	usage := settings.TokenUsage
	tokens := TokenBreakdown{
		Input: nonNeg(usage.InputTokens), Output: nonNeg(usage.OutputTokens),
		CacheRead: nonNeg(usage.CacheRead), CacheWrite: nonNeg(usage.CacheCreation),
		Reasoning: nonNeg(usage.Thinking),
	}
	if tokens.Total() == 0 {
		return nil
	}
	session := strings.TrimSuffix(filepath.Base(file), ".settings.json")
	session = strings.TrimSuffix(session, ".json")
	model := ""
	if settings.Model != nil {
		model = strings.TrimSpace(*settings.Model)
	}
	if model == "" {
		model = "unknown"
	}
	provider := ""
	if settings.ProviderLock != nil {
		provider = strings.TrimSpace(*settings.ProviderLock)
	}
	wsKey, wsLabel := WorkspaceAfterMarker(file, "sessions", "")
	// One cumulative row per settings file; the file identity is the dedup key.
	emit(UnifiedEntry{
		Client: p.Client(), ModelID: model, ProviderID: provider, SessionID: session,
		WorkspaceKey: wsKey, WorkspaceLabel: wsLabel, TimestampMS: mtimeMS, Tokens: tokens,
		DedupKey: "droid:" + session + ":token_usage",
	})
	return nil
}

package sessionstats

// Amp: ~/.local/share/amp/threads/*.json — thread documents with a usage
// ledger plus per-message usage; ledger events are authoritative, message
// records that match a ledger event merge into it, and unmatched message
// records still count (sessions/amp.rs).

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
)

type ampParser struct{}

func (ampParser) Client() string { return "amp" }

func (p ampParser) Roots(home string) []string {
	return []string{filepath.Join(home, ".local", "share", "amp", "threads")}
}

func (p ampParser) Discover(root string) ([]string, error) {
	files, err := WalkFilesWithSuffix(root, ".json")
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}

type ampThread struct {
	ID          *string         `json:"id"`
	Created     *int64          `json:"created"`
	Messages    []ampMessage    `json:"messages"`
	UsageLedger *ampUsageLedger `json:"usageLedger"`
}

type ampUsageLedger struct {
	Events []ampUsageEvent `json:"events"`
}

type ampUsageEvent struct {
	Timestamp   *string    `json:"timestamp"`
	Model       *string    `json:"model"`
	ToMessageID *int64     `json:"toMessageId"`
	Tokens      *ampTokens `json:"tokens"`
}

type ampTokens struct {
	Input         *int64 `json:"input"`
	Output        *int64 `json:"output"`
	CacheRead     *int64 `json:"cacheReadInputTokens"`
	CacheCreation *int64 `json:"cacheCreationInputTokens"`
}

type ampMessage struct {
	ID    *int64     `json:"id"`
	Role  *string    `json:"role"`
	Usage *ampTokens `json:"usage"`
}

// ampRecord is the merged ledger/message record both sources reduce to.
type ampRecord struct {
	ts     int64
	model  string
	tokens TokenBreakdown
}

func (p ampParser) ParseFile(file string, mtimeMS int64, emit func(UnifiedEntry)) error {
	body, ok := readFileLimited(file, maxFileBytes)
	if !ok {
		return nil
	}
	var thread ampThread
	if json.Unmarshal(body, &thread) != nil {
		return nil
	}
	threadID := "unknown"
	if thread.ID != nil && strings.TrimSpace(*thread.ID) != "" {
		threadID = strings.TrimSpace(*thread.ID)
	} else {
		threadID = strings.TrimSuffix(filepath.Base(file), ".json")
	}
	var records []ampRecord
	if thread.UsageLedger != nil {
		for _, event := range thread.UsageLedger.Events {
			tokens, ok := ampBreakdown(event.Tokens)
			if !ok {
				continue
			}
			ts := mtimeMS
			if event.Timestamp != nil {
				if parsed, valid := ParseRFC3339MS(*event.Timestamp); valid {
					ts = parsed
				}
			}
			records = append(records, ampRecord{ts: ts, model: ampModel(event.Model), tokens: tokens})
		}
	}
	// Message records without a ledger counterpart still count: the upstream
	// matcher pairs by message id; a simple positional pairing over role-less
	// usage messages is the Go port's equivalent for unmatched extras.
	if len(records) == 0 {
		for _, message := range thread.Messages {
			tokens, ok := ampBreakdown(message.Usage)
			if !ok {
				continue
			}
			records = append(records, ampRecord{ts: mtimeMS, model: "unknown", tokens: tokens})
		}
	}
	sort.Slice(records, func(i, j int) bool { return records[i].ts < records[j].ts })
	for index, record := range records {
		emit(UnifiedEntry{
			Client: p.Client(), ModelID: record.model, ProviderID: "anthropic",
			SessionID: threadID, TimestampMS: record.ts, Tokens: record.tokens,
			DedupKey: "amp:" + threadID + ":" + itoa(index),
		})
	}
	return nil
}

func ampBreakdown(tokens *ampTokens) (TokenBreakdown, bool) {
	if tokens == nil {
		return TokenBreakdown{}, false
	}
	breakdown := TokenBreakdown{
		Input: nonNeg(tokens.Input), Output: nonNeg(tokens.Output),
		CacheRead: nonNeg(tokens.CacheRead), CacheWrite: nonNeg(tokens.CacheCreation),
	}
	if breakdown.Total() == 0 {
		return TokenBreakdown{}, false
	}
	return breakdown, true
}

func ampModel(model *string) string {
	if model == nil {
		return "unknown"
	}
	value := strings.TrimSpace(*model)
	if value == "" {
		return "unknown"
	}
	return value
}

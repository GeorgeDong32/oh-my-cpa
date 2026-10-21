package sessionstats

import (
	"encoding/json"
	"strings"
)

// synthetic.go: the synthetic.new / Octofriend gateway re-attribution.
// Traffic other agents route through these gateways reports the gateway's
// own model/provider spellings; normalization rewrites them so the row
// prices and groups under the underlying model.

// isSyntheticModel reports whether a model id is a gateway alias.
func isSyntheticModel(model string) bool {
	lowered := strings.ToLower(model)
	return strings.HasPrefix(lowered, "hf:") ||
		strings.HasPrefix(lowered, "accounts/fireworks/") ||
		strings.HasPrefix(lowered, "accounts/together/")
}

// isSyntheticProvider reports whether a provider id names the gateway.
func isSyntheticProvider(provider string) bool {
	switch strings.ToLower(provider) {
	case "synthetic", "glhf", "synthetic.new", "octofriend":
		return true
	}
	return false
}

func isSyntheticGateway(model, provider string) bool {
	return isSyntheticModel(model) || isSyntheticProvider(provider)
}

// normalizeSyntheticModel strips the gateway prefixes: "hf:<org>/<model>"
// keeps the model; "accounts/<host>/models/<model>" keeps the model.
func normalizeSyntheticModel(model string) string {
	lowered := strings.ToLower(model)
	if rest, ok := strings.CutPrefix(lowered, "hf:"); ok {
		if _, model, found := strings.Cut(rest, "/"); found {
			return model
		}
		return rest
	}
	if rest, ok := strings.CutPrefix(lowered, "accounts/"); ok {
		if _, model, found := strings.Cut(rest, "/models/"); found {
			return model
		}
	}
	return lowered
}

// normalizeSyntheticEntry rewrites an entry in place; false when the entry is
// not gateway traffic.
func normalizeSyntheticEntry(entry *UnifiedEntry) bool {
	if !isSyntheticGateway(entry.ModelID, entry.ProviderID) {
		return false
	}
	entry.ModelID = normalizeSyntheticModel(entry.ModelID)
	if entry.ProviderID == "" || strings.EqualFold(entry.ProviderID, "unknown") {
		entry.ProviderID = "synthetic"
	}
	return true
}

// octofriendParser reads the Octofriend gateway's own message ledger
// (messages table with per-row buckets and cost).
type octofriendParser struct{}

func (octofriendParser) Client() string { return "synthetic" }

func (octofriendParser) Roots(home string) []string {
	// Upstream reads only $XDG_DATA_HOME/octofriend/sqlite.db.
	return []string{home + "/.local/share/octofriend"}
}

func (octofriendParser) Discover(root string) ([]string, error) {
	return WalkFilesWithSuffix(root, "sqlite.db")
}

func (octofriendParser) ParseFile(file string, mtimeMS int64, emit func(UnifiedEntry)) error {
	db, err := openReadonlyDB(file)
	if err != nil {
		return nil
	}
	defer db.Close()
	var hasTable int
	if err := db.QueryRow(
		"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN ('messages','sessions','token_usage')").Scan(&hasTable); err != nil || hasTable == 0 {
		return nil
	}
	rows, err := db.Query(`
		SELECT id, model, input_tokens, output_tokens, cache_read_tokens,
		       cache_write_tokens, reasoning_tokens, cost, timestamp, session_id, provider
		FROM messages
		WHERE input_tokens IS NOT NULL OR output_tokens IS NOT NULL`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	for rows.Next() {
		var id, model, session, provider string
		var input, output, cacheRead, cacheWrite, reasoning int64
		var cost, timestamp float64
		if err := rows.Scan(&id, &model, &input, &output, &cacheRead, &cacheWrite, &reasoning, &cost, &timestamp, &session, &provider); err != nil {
			continue
		}
		tokens := TokenBreakdown{
			Input: clampNonNeg(input), Output: clampNonNeg(output),
			CacheRead: clampNonNeg(cacheRead), CacheWrite: clampNonNeg(cacheWrite),
			Reasoning: clampNonNeg(reasoning),
		}
		if tokens.Total() == 0 {
			continue
		}
		ts := int64(timestamp)
		if timestamp <= 1e12 {
			ts = secondsToMS(timestamp)
		}
		if ts <= 0 {
			continue
		}
		if strings.TrimSpace(session) == "" {
			session = "unknown"
		}
		if strings.TrimSpace(provider) == "" {
			provider = "synthetic"
		}
		emit(UnifiedEntry{
			Client: "synthetic", ModelID: normalizeSyntheticModel(model),
			ProviderID: provider, SessionID: session, TimestampMS: ts,
			Tokens: tokens, DedupKey: strings.TrimSpace(id),
		})
	}
	return nil
}

var _ = json.Marshal

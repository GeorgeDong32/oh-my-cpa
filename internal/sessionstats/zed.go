package sessionstats

// Zed Agent: ~/Library/Application Support/Zed/threads/threads.db —
// third-party SQLite, read-only (sessions/zed.rs). Each hosted thread row
// carries a JSON blob (plain or zstd) whose request_token_usage /
// cumulative_token_usage holds per-thread totals; one row emits one entry.

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
)

type zedParser struct{}

func (zedParser) Client() string { return "zed" }

func (p zedParser) Roots(home string) []string {
	return []string{filepath.Join(home, "Library", "Application Support", "Zed", "threads", "threads.db")}
}

func (p zedParser) Discover(root string) ([]string, error) {
	if filepath.Ext(root) == ".db" {
		return []string{root}, nil
	}
	return nil, nil
}

type zedThreadRow struct {
	ID        string
	UpdatedAt string
	CreatedAt sql.NullString
	DataType  string
	Data      []byte
}

type zedUsage struct {
	Input      *int64 `json:"inputTokens"`
	Output     *int64 `json:"outputTokens"`
	CacheRead  *int64 `json:"cacheReadTokens"`
	CacheWrite *int64 `json:"cacheCreationTokens"`
}

func (u *zedUsage) breakdown() (TokenBreakdown, bool) {
	if u == nil {
		return TokenBreakdown{}, false
	}
	tokens := TokenBreakdown{
		Input: nonNeg(u.Input), Output: nonNeg(u.Output),
		CacheRead: nonNeg(u.CacheRead), CacheWrite: nonNeg(u.CacheWrite),
	}
	return tokens, tokens.Total() > 0
}

type zedThreadJSON struct {
	// Only Zed-hosted rows count: an external ACP agent is billed and logged
	// by its own provider, and counting it here would double-book it.
	Provider string `json:"provider"`
	Model    *struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
	} `json:"model"`
	RequestTokenUsage    map[string]*zedUsage `json:"request_token_usage"`
	CumulativeTokenUsage *zedUsage            `json:"cumulative_token_usage"`
}

func (p zedParser) ParseFile(file string, mtimeMS int64, emit func(UnifiedEntry)) error {
	db, err := openReadonlySQLite(file)
	if err != nil {
		return err
	}
	defer db.Close()
	// Optional columns degrade like upstream: pre-created_at schemas answer NULL.
	rows, err := db.Query(`SELECT id, updated_at, created_at, data_type, data FROM threads`)
	if err != nil {
		return err
	}
	defer rows.Close()
	decoder, _ := zstd.NewReader(nil)
	defer decoder.Close()
	for rows.Next() {
		var row zedThreadRow
		var created sql.NullString
		if scanErr := rows.Scan(&row.ID, &row.UpdatedAt, &created, &row.DataType, &row.Data); scanErr != nil {
			return scanErr
		}
		row.CreatedAt = created
		blob := row.Data
		if row.DataType == "zstd" {
			plain, decodeErr := decoder.DecodeAll(blob, nil)
			if decodeErr != nil {
				continue // one undecodable thread never fails the scan
			}
			blob = plain
		}
		var thread zedThreadJSON
		if json.Unmarshal(blob, &thread) != nil {
			continue
		}
		if thread.Model != nil {
			if !strings.EqualFold(strings.TrimSpace(thread.Model.Provider), "zed.dev") {
				continue
			}
		} else if thread.Provider != "" && !strings.EqualFold(strings.TrimSpace(thread.Provider), "zed.dev") {
			continue
		}
		// Per-request usage summed wins; the cumulative total is the fallback.
		tokens, count := TokenBreakdown{}, int32(0)
		for _, usage := range thread.RequestTokenUsage {
			if breakdown, ok := usage.breakdown(); ok {
				tokens.Input += breakdown.Input
				tokens.Output += breakdown.Output
				tokens.CacheRead += breakdown.CacheRead
				tokens.CacheWrite += breakdown.CacheWrite
				count++
			}
		}
		if tokens.Total() == 0 {
			if breakdown, ok := thread.CumulativeTokenUsage.breakdown(); ok {
				tokens, count = breakdown, 1
			}
		}
		if tokens.Total() == 0 {
			continue
		}
		ts := mtimeMS
		if parsed, valid := ParseRFC3339MS(row.UpdatedAt); valid {
			ts = parsed
		} else if row.CreatedAt.Valid {
			if parsed, valid := ParseRFC3339MS(row.CreatedAt.String); valid {
				ts = parsed
			}
		}
		model := "unknown"
		if thread.Model != nil && strings.TrimSpace(thread.Model.Model) != "" {
			model = strings.TrimSpace(thread.Model.Model)
		}
		emit(UnifiedEntry{
			Client: p.Client(), ModelID: model, ProviderID: "zed", SessionID: row.ID,
			TimestampMS: ts, Tokens: tokens, MessageCount: count,
			DedupKey: "zed:" + row.ID + ":token_usage",
		})
	}
	return rows.Err()
}

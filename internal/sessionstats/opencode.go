package sessionstats

// OpenCode: ~/.local/share/opencode/opencode.db — third-party SQLite, opened
// read-only (sessions/opencode.rs + opencode_schema.rs). Assistant rows live
// in session_message (v2) or message (v1) with a JSON data column; the
// session table contributes workspace directory and title. Query variants
// degrade when the metadata table or its columns are absent, exactly as
// upstream degrades; a schema that matches none marks the client unreadable
// for the scan.

import (
	"database/sql"
	"encoding/json"
	"path/filepath"

	_ "modernc.org/sqlite"
)

type opencodeParser struct{}

func (opencodeParser) Client() string { return "opencode" }

func (p opencodeParser) Roots(home string) []string {
	return []string{filepath.Join(home, ".local", "share", "opencode", "opencode.db")}
}

// Discover returns the database itself: SQLite sources are one file per
// client; the watermark is the file's mtime/size.
func (p opencodeParser) Discover(root string) ([]string, error) {
	if filepath.Ext(root) == ".db" {
		return []string{root}, nil
	}
	return nil, nil
}

type opencodeRow struct {
	ID        string
	SessionID string
	Data      string
	Workspace string
}

var opencodeQueries = []string{
	// v2 with title, session_v2 metadata
	`SELECT sm.id, sm.session_id, sm.data, NULLIF(s.directory, '') FROM session_message sm
	 LEFT JOIN session_v2 s ON s.id = sm.session_id
	 WHERE sm.type = 'assistant' AND json_extract(sm.data, '$.tokens') IS NOT NULL ORDER BY sm.id`,
	// v2 without title column
	`SELECT sm.id, sm.session_id, sm.data, NULLIF(s.directory, '') FROM session_message sm
	 LEFT JOIN session s ON s.id = sm.session_id
	 WHERE sm.type = 'assistant' AND json_extract(sm.data, '$.tokens') IS NOT NULL ORDER BY sm.id`,
	// v2 without metadata table
	`SELECT sm.id, sm.session_id, sm.data, NULL AS workspace FROM session_message sm
	 WHERE sm.type = 'assistant' AND json_extract(sm.data, '$.tokens') IS NOT NULL ORDER BY sm.id`,
	// v1 with metadata
	`SELECT m.id, m.session_id, m.data, NULLIF(s.directory, '') FROM message m
	 LEFT JOIN session s ON s.id = m.session_id
	 WHERE json_extract(m.data, '$.role') = 'assistant' AND json_extract(m.data, '$.tokens') IS NOT NULL ORDER BY m.id`,
	// v1 without metadata
	`SELECT m.id, m.session_id, m.data, NULL AS workspace FROM message m
	 WHERE json_extract(m.data, '$.role') = 'assistant' AND json_extract(m.data, '$.tokens') IS NOT NULL ORDER BY m.id`,
}

type opencodeData struct {
	Model *struct {
		ProviderID string `json:"providerID"`
		ID         string `json:"id"`
	} `json:"model"`
	Tokens *struct {
		Input     *int64 `json:"input"`
		Output    *int64 `json:"output"`
		Reasoning *int64 `json:"reasoning"`
		Cache     *struct {
			Read  *int64 `json:"read"`
			Write *int64 `json:"write"`
		} `json:"cache"`
	} `json:"tokens"`
}

func (p opencodeParser) ParseFile(file string, mtimeMS int64, emit func(UnifiedEntry)) error {
	db, err := openReadonlySQLite(file)
	if err != nil {
		return err
	}
	defer db.Close()
	var rows []opencodeRow
	var lastErr error
	for _, query := range opencodeQueries {
		rows = nil
		result, err := db.Query(query)
		if err != nil {
			lastErr = err
			continue
		}
		for result.Next() {
			var row opencodeRow
			var workspace sql.NullString
			if scanErr := result.Scan(&row.ID, &row.SessionID, &row.Data, &workspace); scanErr != nil {
				result.Close()
				lastErr = scanErr
				rows = nil
				break
			}
			if workspace.Valid {
				row.Workspace = workspace.String
			}
			rows = append(rows, row)
		}
		result.Close()
		if len(rows) > 0 || lastErr == nil {
			break // first variant that answered wins (degradation order)
		}
	}
	for _, row := range rows {
		var data opencodeData
		if json.Unmarshal([]byte(row.Data), &data) != nil || data.Tokens == nil {
			continue
		}
		tokens := TokenBreakdown{
			Input: nonNeg(data.Tokens.Input), Output: nonNeg(data.Tokens.Output),
			Reasoning: nonNeg(data.Tokens.Reasoning),
		}
		if data.Tokens.Cache != nil {
			tokens.CacheRead = nonNeg(data.Tokens.Cache.Read)
			tokens.CacheWrite = nonNeg(data.Tokens.Cache.Write)
		}
		if tokens.Total() == 0 {
			continue
		}
		model, provider := "unknown", ""
		if data.Model != nil {
			if data.Model.ID != "" {
				model = data.Model.ID
			}
			provider = data.Model.ProviderID
		}
		wsKey := NormalizeWorkspaceKey(row.Workspace)
		emit(UnifiedEntry{
			Client: p.Client(), ModelID: model, ProviderID: provider, SessionID: row.SessionID,
			WorkspaceKey: wsKey, WorkspaceLabel: WorkspaceLabelFromKey(wsKey),
			TimestampMS: mtimeMS, Tokens: tokens,
			// Row ids are stable upstream identities; the read-time merge keeps
			// one row per key across table variants.
			DedupKey: "opencode:" + row.SessionID + ":" + row.ID,
		})
	}
	return nil
}

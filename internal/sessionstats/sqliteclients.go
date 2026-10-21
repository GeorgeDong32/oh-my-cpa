package sessionstats

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// sqliteFamily.go: Batch 2's SQLite-only clients (goose, crush, openclaw).
// All open the source database strictly read-only via the registered
// sqliteDriver name from sqlite.go (modernc.org/sqlite, URI readonly mode).

func openReadonlyDB(file string) (*sql.DB, error) {
	return openReadonlySQLite(file)
}

// gooseParser reads Goose's sessions.db: one row per session with
// accumulated token counts. Goose's schema has no reasoning column; the gap
// between the reported total and input+output is heuristically attributed to
// reasoning (an inference, not a measured count).
type gooseParser struct{}

func (gooseParser) Client() string { return "goose" }

func (gooseParser) Roots(home string) []string {
	return []string{
		home + "/.local/share/goose/sessions",
		home + "/Library/Application Support/goose/sessions",
		home + "/.local/share/Block/goose/sessions",
	}
}

func (gooseParser) Discover(root string) ([]string, error) {
	return WalkFilesWithSuffix(root, "sessions.db")
}

type gooseModelConfig struct {
	ModelName string `json:"model_name"`
}

func (gooseParser) ParseFile(file string, mtimeMS int64, emit func(UnifiedEntry)) error {
	db, err := openReadonlyDB(file)
	if err != nil {
		return nil
	}
	defer db.Close()
	rows, err := db.Query(`
		SELECT id, model_config_json, provider_name, created_at,
		       total_tokens, input_tokens, output_tokens,
		       accumulated_total_tokens, accumulated_input_tokens, accumulated_output_tokens
		FROM sessions
		WHERE model_config_json IS NOT NULL AND TRIM(model_config_json) != ''`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var modelConfigJSON *string
		var providerName *string
		var createdAt string
		var total, input, output *int64
		var accTotal, accInput, accOutput *int64
		if err := rows.Scan(&id, &modelConfigJSON, &providerName, &createdAt, &total, &input, &output, &accTotal, &accInput, &accOutput); err != nil {
			continue
		}
		if modelConfigJSON == nil {
			continue
		}
		var config gooseModelConfig
		if json.Unmarshal([]byte(*modelConfigJSON), &config) != nil || strings.TrimSpace(config.ModelName) == "" {
			continue
		}
		model := strings.TrimSpace(config.ModelName)
		use := func(primary, fallback *int64) int64 {
			if primary != nil {
				return clampNonNeg(*primary)
			}
			if fallback != nil {
				return clampNonNeg(*fallback)
			}
			return 0
		}
		in := use(accInput, input)
		out := use(accOutput, output)
		tot := use(accTotal, total)
		if in == 0 && out == 0 && tot == 0 {
			continue
		}
		reasoning := int64(0)
		if tot > in+out {
			reasoning = tot - in - out
		}
		provider := ""
		if providerName != nil {
			provider = strings.TrimSpace(*providerName)
		}
		if provider == "" {
			provider = providerForModelOr(model, "goose")
		}
		ts := parseGooseCreatedAt(createdAt)
		if ts <= 0 {
			ts = mtimeMS
		}
		emit(UnifiedEntry{
			Client: "goose", ModelID: model, ProviderID: provider, SessionID: id,
			TimestampMS: ts, Tokens: TokenBreakdown{Input: in, Output: out, Reasoning: reasoning},
			DedupKey: "goose:" + id,
		})
	}
	return nil
}

func parseGooseCreatedAt(value string) int64 {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05", "2006-01-02"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UnixMilli()
		}
	}
	return 0
}

// crushParser reads per-project crush.db files registered in
// ~/.config/crush/projects.json (or the macOS/Windows equivalents). Crush
// tracks cost, not tokens, so rows carry cost only against a synthetic
// session-total model; OMC prices nothing for them (no token buckets) but
// the message counts and activity still land.
type crushParser struct{}

func (crushParser) Client() string { return "crush" }

func (crushParser) Roots(home string) []string {
	return []string{
		home + "/.config/crush/projects.json",
		home + "/.local/share/crush/projects.json",
		home + "/Library/Application Support/crush/projects.json",
	}
}

// Discover resolves the registry file into the per-project crush.db paths;
// each discovered db path is prefixed with the project's workspace key so
// rows carry the workspace the registry names (encoded as <ws-key>|<db>).
func (crushParser) Discover(root string) ([]string, error) {
	body, ok := readFileLimited(root, maxFileBytes)
	if !ok {
		return nil, nil
	}
	var registry struct {
		Projects []struct {
			Path    string `json:"path"`
			DataDir string `json:"data_dir"`
		} `json:"projects"`
	}
	if json.Unmarshal(body, &registry) != nil {
		return nil, nil
	}
	var dbs []string
	for _, project := range registry.Projects {
		if strings.TrimSpace(project.Path) == "" {
			continue
		}
		dataDir := strings.TrimSpace(project.DataDir)
		if dataDir == "" || !filepath.IsAbs(dataDir) {
			dataDir = filepath.Join(strings.TrimSpace(project.Path), dataDir)
		}
		candidate := filepath.Join(dataDir, "crush.db")
		if fileExists(candidate) {
			if wsKey := NormalizeWorkspaceKey(strings.TrimSpace(project.Path)); wsKey != "" {
				dbs = append(dbs, wsKey+"|"+candidate)
			} else {
				dbs = append(dbs, candidate)
			}
		}
	}
	return dbs, nil
}

// crushSplitPath unpacks the registry-annotated discovery path.
func crushSplitPath(file string) (wsKey, dbPath string) {
	if idx := strings.Index(file, "|"); idx > 0 {
		return file[:idx], file[idx+1:]
	}
	return "", file
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func (crushParser) ParseFile(file string, mtimeMS int64, emit func(UnifiedEntry)) error {
	wsKey, dbPath := crushSplitPath(file)
	db, err := openReadonlyDB(dbPath)
	if err != nil {
		return nil
	}
	defer db.Close()
	type dayBucket struct {
		ts    int64
		count int32
	}
	buckets := map[string][]dayBucket{}
	rows, err := db.Query(`
		WITH RECURSIVE session_tree(root_session_id, session_id) AS (
			SELECT id, id FROM sessions WHERE parent_session_id IS NULL
			UNION ALL
			SELECT st.root_session_id, s.id FROM sessions s
			JOIN session_tree st ON s.parent_session_id = st.session_id
		)
		SELECT st.root_session_id, m.created_at, COUNT(*)
		FROM session_tree st JOIN messages m ON m.session_id = st.session_id
		WHERE m.role = 'assistant'
		GROUP BY st.root_session_id, date(m.created_at/1000, 'unixepoch')
		ORDER BY st.root_session_id, MIN(m.created_at)`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var sessionID string
			var createdAt, count int64
			if err := rows.Scan(&sessionID, &createdAt, &count); err != nil {
				continue
			}
			ts := normalizeCrushTimestamp(createdAt)
			if ts <= 0 {
				continue
			}
			buckets[sessionID] = append(buckets[sessionID], dayBucket{ts: ts, count: int32(count)})
		}
		rows.Close()
	}
	sessionRows, err := db.Query(`
		SELECT id, COALESCE(cost, 0), COALESCE(created_at, 0), COALESCE(updated_at, 0)
		FROM sessions
		WHERE parent_session_id IS NULL
		  AND (COALESCE(message_count, 0) > 0 OR COALESCE(cost, 0) > 0)
		ORDER BY created_at ASC`)
	if err != nil {
		return nil
	}
	defer sessionRows.Close()
	type crushSession struct {
		id        string
		cost      float64
		createdAt int64
		updatedAt int64
	}
	var sessions []crushSession
	for sessionRows.Next() {
		var session crushSession
		if err := sessionRows.Scan(&session.id, &session.cost, &session.createdAt, &session.updatedAt); err != nil {
			continue
		}
		sessions = append(sessions, session)
	}
	sessionRows.Close()
	// Deterministic emission order (SQL may return rows unordered per session).
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].createdAt < sessions[j].createdAt })
	for _, session := range sessions {
		sessionKey := dbPath + ":" + session.id
		days := buckets[session.id]
		if len(days) == 0 {
			if session.cost <= 0 {
				continue
			}
			ts := normalizeCrushTimestamp(session.updatedAt)
			if ts <= 0 {
				ts = normalizeCrushTimestamp(session.createdAt)
			}
			if ts <= 0 {
				ts = mtimeMS
			}
			wsKey, wsLabel := wsKey, ""
			if wsKey != "" {
				wsLabel = WorkspaceLabelFromKey(wsKey)
			}
			emit(UnifiedEntry{
				Client: "crush", ModelID: "session-total", ProviderID: "crush",
				SessionID: sessionKey, WorkspaceKey: wsKey, WorkspaceLabel: wsLabel,
				TimestampMS: ts,
				Tokens:      TokenBreakdown{}, DedupKey: "crush:" + sessionKey + ":total",
			})
			continue
		}
		sort.Slice(days, func(i, j int) bool { return days[i].ts < days[j].ts })
		totalMessages := int32(0)
		for _, day := range days {
			totalMessages += day.count
		}
		wsLabel := ""
		if wsKey != "" {
			wsLabel = WorkspaceLabelFromKey(wsKey)
		}
		for _, day := range days {
			emit(UnifiedEntry{
				Client: "crush", ModelID: "session-total", ProviderID: "crush",
				SessionID: sessionKey, WorkspaceKey: wsKey, WorkspaceLabel: wsLabel,
				TimestampMS: day.ts,
				Tokens:      TokenBreakdown{}, MessageCount: day.count,
				DedupKey: "crush:" + sessionKey + ":" + fmt.Sprintf("%d", day.ts),
			})
		}
	}
	return nil
}

func normalizeCrushTimestamp(raw int64) int64 {
	if raw <= 0 {
		return 0
	}
	if raw >= 100_000_000_000 {
		return raw
	}
	return raw * 1000
}

// openclawParser reads OpenClaw transcripts: live SQLite agent databases
// (openclaw-agent.sqlite) and legacy ~/.openclaw/agents/**/*.jsonl session
// files (including the .clawdbot/.moltbot/.moldbot rebrand roots).
type openclawParser struct{}

func (openclawParser) Client() string { return "openclaw" }

func (openclawParser) Roots(home string) []string {
	return []string{
		home + "/.openclaw/agents",
		home + "/.clawdbot/agents",
		home + "/.moltbot/agents",
		home + "/.moldbot/agents",
	}
}

func (openclawParser) Discover(root string) ([]string, error) {
	jsonl, err := WalkJSONLFiles(root)
	if err != nil {
		return nil, err
	}
	dbs, err := WalkFilesWithSuffix(root, "openclaw-agent.sqlite")
	if err != nil {
		return jsonl, nil
	}
	return append(jsonl, dbs...), nil
}

func (openclawParser) ParseFile(file string, mtimeMS int64, emit func(UnifiedEntry)) error {
	if strings.HasSuffix(file, ".sqlite") {
		parseOpenclawSQLite(file, emit)
		return nil
	}
	session := strings.TrimSuffix(filepath.Base(file), ".jsonl")
	parseOpenclawTranscript(file, session, mtimeMS, emit)
	return nil
}

type openclawState struct {
	model    string
	provider string
}

// openclawEvent is the wire shape BOTH lanes share (upstream OpenClawEntry):
// camelCase ids, `message.model` (not modelId), camel usage buckets.
type openclawEvent struct {
	Type       string `json:"type"`
	ID         string `json:"id"`
	ModelID    string `json:"modelId"`
	Provider   string `json:"provider"`
	CustomType string `json:"customType"`
	Data       *struct {
		ModelID  string `json:"modelId"`
		Provider string `json:"provider"`
	} `json:"data"`
	Message *struct {
		Role      string `json:"role"`
		Model     string `json:"model"`
		Provider  string `json:"provider"`
		Timestamp *int64 `json:"timestamp"`
		Usage     *struct {
			Input      *int64 `json:"input"`
			Output     *int64 `json:"output"`
			CacheRead  *int64 `json:"cacheRead"`
			CacheWrite *int64 `json:"cacheWrite"`
			Reasoning  *int64 `json:"reasoningTokens"`
		} `json:"usage"`
	} `json:"message"`
}

func parseOpenclawTranscript(file, session string, mtimeMS int64, emit func(UnifiedEntry)) {
	state := openclawState{}
	forEachJSONLine(file, mtimeMS, func(raw []byte) {
		var entry openclawEvent
		if json.Unmarshal(raw, &entry) != nil {
			return
		}
		switch entry.Type {
		case "model_change":
			if strings.TrimSpace(entry.ModelID) != "" {
				state.model = strings.TrimSpace(entry.ModelID)
			}
			if strings.TrimSpace(entry.Provider) != "" {
				state.provider = strings.TrimSpace(entry.Provider)
			}
			return
		case "custom":
			if entry.CustomType != "model-snapshot" || entry.Data == nil {
				return
			}
			if strings.TrimSpace(entry.Data.ModelID) != "" {
				state.model = strings.TrimSpace(entry.Data.ModelID)
			}
			if strings.TrimSpace(entry.Data.Provider) != "" {
				state.provider = strings.TrimSpace(entry.Data.Provider)
			}
			return
		case "message":
			message := entry.Message
			if message == nil || message.Role != "assistant" || message.Usage == nil {
				return
			}
			model := strings.TrimSpace(message.Model)
			if model == "" {
				model = state.model
			}
			if model == "" {
				return
			}
			provider := strings.TrimSpace(message.Provider)
			if provider == "" {
				provider = state.provider
			}
			if provider == "" {
				provider = "unknown"
			}
			state.model, state.provider = model, provider
			usage := message.Usage
			reasoning := nonNeg(usage.Reasoning)
			tokens := TokenBreakdown{
				Input: nonNeg(usage.Input), Output: clampNonNeg(nonNeg(usage.Output) - reasoning),
				CacheRead: nonNeg(usage.CacheRead), CacheWrite: nonNeg(usage.CacheWrite),
				Reasoning: reasoning,
			}
			if tokens.Total() == 0 {
				return
			}
			ts := mtimeMS
			if message.Timestamp != nil && *message.Timestamp > 0 {
				ts = *message.Timestamp
			}
			key := ""
			if strings.TrimSpace(entry.ID) != "" {
				key = "openclaw:" + strings.TrimSpace(entry.ID)
			}
			emit(UnifiedEntry{
				Client: "openclaw", ModelID: model, ProviderID: provider,
				SessionID: session, TimestampMS: ts, Tokens: tokens, DedupKey: key,
			})
		}
	})
}

func parseOpenclawSQLite(file string, emit func(UnifiedEntry)) {
	db, err := openReadonlyDB(file)
	if err != nil {
		return
	}
	defer db.Close()
	var hasTable int
	if err := db.QueryRow(
		"SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'transcript_events'").Scan(&hasTable); err != nil || hasTable == 0 {
		return
	}
	rows, err := db.Query(`
		SELECT e.session_id, e.event_json, COALESCE(e.created_at, 0)
		FROM transcript_events e
		WHERE instr(e.event_json, '"usage"') > 0
		   OR instr(e.event_json, '"model_change"') > 0
		   OR instr(e.event_json, '"model-snapshot"') > 0
		ORDER BY e.session_id, e.seq`)
	if err != nil {
		return
	}
	defer rows.Close()
	states := map[string]*openclawState{}
	type pendingMessage struct {
		session   string
		entry     openclawEvent
		createdAt int64
	}
	var pending []pendingMessage
	for rows.Next() {
		var session, eventJSON string
		var createdAt int64
		if err := rows.Scan(&session, &eventJSON, &createdAt); err != nil {
			continue
		}
		var entry openclawEvent
		if json.Unmarshal([]byte(eventJSON), &entry) != nil {
			continue
		}
		pending = append(pending, pendingMessage{session: session, entry: entry, createdAt: createdAt})
	}
	rows.Close()
	for _, item := range pending {
		state := states[item.session]
		if state == nil {
			state = &openclawState{}
			states[item.session] = state
		}
		entry := item.entry
		switch entry.Type {
		case "model_change":
			if strings.TrimSpace(entry.ModelID) != "" {
				state.model = strings.TrimSpace(entry.ModelID)
			}
			if strings.TrimSpace(entry.Provider) != "" {
				state.provider = strings.TrimSpace(entry.Provider)
			}
		case "custom":
			if entry.Data != nil {
				if strings.TrimSpace(entry.Data.ModelID) != "" {
					state.model = strings.TrimSpace(entry.Data.ModelID)
				}
				if strings.TrimSpace(entry.Data.Provider) != "" {
					state.provider = strings.TrimSpace(entry.Data.Provider)
				}
			}
		case "message":
			message := entry.Message
			if message == nil || message.Role != "assistant" || message.Usage == nil {
				continue
			}
			model := strings.TrimSpace(message.Model)
			if model == "" {
				model = state.model
			}
			if model == "" {
				continue
			}
			provider := strings.TrimSpace(message.Provider)
			if provider == "" {
				provider = state.provider
			}
			if provider == "" {
				provider = "unknown"
			}
			usage := message.Usage
			reasoning := nonNeg(usage.Reasoning)
			tokens := TokenBreakdown{
				Input: nonNeg(usage.Input), Output: clampNonNeg(nonNeg(usage.Output) - reasoning),
				CacheRead: nonNeg(usage.CacheRead), CacheWrite: nonNeg(usage.CacheWrite),
				Reasoning: reasoning,
			}
			if tokens.Total() == 0 {
				continue
			}
			ts := int64(0)
			if message.Timestamp != nil && *message.Timestamp > 0 {
				ts = *message.Timestamp
			} else if item.createdAt > 0 {
				ts = normalizeCrushTimestamp(item.createdAt)
			}
			if ts <= 0 {
				continue
			}
			// Same prefix as the JSONL lane: an event id survives the
			// migration into the SQLite store and must dedup across both.
			key := ""
			if strings.TrimSpace(entry.ID) != "" {
				key = "openclaw:" + strings.TrimSpace(entry.ID)
			}
			emit(UnifiedEntry{
				Client: "openclaw", ModelID: model, ProviderID: provider,
				SessionID: item.session, TimestampMS: ts, Tokens: tokens, DedupKey: key,
			})
		}
	}
}

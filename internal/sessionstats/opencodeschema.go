package sessionstats

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// opencodeschema.go: the shared OpenCode-schema SQLite engine that kilo and
// micode ride (upstream opencode_schema.rs). Each client is a dialect over
// the same message table shape: a `data` JSON blob with role/modelID/
// providerID/tokens{input,output,reasoning,cache{read,write}}/time{created}.

type opencodeSchemaConfig struct {
	client            string
	queries           []string
	fallbackProvider  string
	inferFromModel    bool
	payloadSessionID  bool
	normalizeSeconds  bool
	fallbackTimestamp int64
	namespaceRowKey   bool
	// strictCache drops rows whose cache object is absent or half-filled;
	// the lenient dialect (micode) zero-fills instead.
	strictCache bool
}

type openCodeSchemaMessage struct {
	ID         *string `json:"id"`
	SessionID  *string `json:"session_id"`
	Role       *string `json:"role"`
	ModelID    *string `json:"modelID"`
	ProviderID *string `json:"providerID"`
	Model      *struct {
		ID         *string `json:"id"`
		ProviderID *string `json:"providerID"`
	} `json:"model"`
	Cost   *float64 `json:"cost"`
	Tokens *struct {
		Input     int64  `json:"input"`
		Output    int64  `json:"output"`
		Reasoning *int64 `json:"reasoning"`
		Cache     *struct {
			Read  *int64 `json:"read"`
			Write *int64 `json:"write"`
		} `json:"cache"`
	} `json:"tokens"`
	Time *struct {
		Created   float64  `json:"created"`
		Completed *float64 `json:"completed"`
	} `json:"time"`
	Agent *string `json:"agent"`
	Mode  *string `json:"mode"`
	Path  *struct {
		Root string `json:"root"`
	} `json:"path"`
}

func (m *openCodeSchemaMessage) resolveModelID() string {
	if m.ModelID != nil && strings.TrimSpace(*m.ModelID) != "" {
		return strings.TrimSpace(*m.ModelID)
	}
	if m.Model != nil && m.Model.ID != nil {
		return strings.TrimSpace(*m.Model.ID)
	}
	return ""
}

func (m *openCodeSchemaMessage) resolveProviderID() string {
	if m.ProviderID != nil && strings.TrimSpace(*m.ProviderID) != "" {
		return strings.TrimSpace(*m.ProviderID)
	}
	if m.Model != nil && m.Model.ProviderID != nil {
		return strings.TrimSpace(*m.Model.ProviderID)
	}
	return ""
}

// parseOpenCodeSchemaSQLite walks the dialect's queries (first that prepares
// wins — a schema probe, not a fallback) and converts assistant rows.
func parseOpenCodeSchemaSQLite(file string, mtimeMS int64, config opencodeSchemaConfig, emit func(UnifiedEntry)) {
	db, err := openReadonlyDB(file)
	if err != nil {
		return
	}
	defer db.Close()
	dbNamespace := file
	for _, query := range config.queries {
		rows, err := db.Query(query)
		if err != nil {
			continue
		}
		defer rows.Close()
		for rows.Next() {
			var rowID string
			var sessionID *string
			var dataJSON string
			var workspaceRoot *string
			if err := rows.Scan(&rowID, &sessionID, &dataJSON, &workspaceRoot); err != nil {
				continue
			}
			emitOpenCodeSchemaRow(config, dbNamespace, rowID, sessionID, dataJSON, workspaceRoot, mtimeMS, emit)
		}
		return
	}
}

func emitOpenCodeSchemaRow(config opencodeSchemaConfig, dbNamespace, rowID string, sessionID *string, dataJSON string, workspaceRoot *string, mtimeMS int64, emit func(UnifiedEntry)) {
	var message openCodeSchemaMessage
	if json.Unmarshal([]byte(dataJSON), &message) != nil {
		return
	}
	if message.Role == nil || strings.TrimSpace(*message.Role) != "assistant" {
		return
	}
	if message.Tokens == nil {
		return
	}
	cacheRead, cacheWrite := int64(0), int64(0)
	if message.Tokens.Cache != nil {
		if message.Tokens.Cache.Read == nil || message.Tokens.Cache.Write == nil {
			if config.strictCache {
				return
			}
			if message.Tokens.Cache.Read != nil {
				cacheRead = clampNonNeg(*message.Tokens.Cache.Read)
			}
			if message.Tokens.Cache.Write != nil {
				cacheWrite = clampNonNeg(*message.Tokens.Cache.Write)
			}
		} else {
			cacheRead, cacheWrite = clampNonNeg(*message.Tokens.Cache.Read), clampNonNeg(*message.Tokens.Cache.Write)
		}
	} else if config.strictCache {
		return
	}
	model := message.resolveModelID()
	if model == "" {
		return
	}
	provider := message.resolveProviderID()
	if provider == "" {
		if config.inferFromModel {
			provider = providerForModelOr(model, config.fallbackProvider)
		} else {
			provider = config.fallbackProvider
		}
	}
	created := int64(0)
	if message.Time != nil {
		created = normalizeSchemaEpoch(message.Time.Created, config.normalizeSeconds)
	} else if config.fallbackTimestamp > 0 {
		created = config.fallbackTimestamp
	} else {
		return
	}
	session := ""
	if sessionID != nil {
		session = strings.TrimSpace(*sessionID)
	}
	if config.payloadSessionID && message.SessionID != nil && strings.TrimSpace(*message.SessionID) != "" {
		session = strings.TrimSpace(*message.SessionID)
	}
	agent := ""
	if message.Mode != nil {
		agent = strings.TrimSpace(*message.Mode)
	}
	if agent == "" && message.Agent != nil {
		agent = strings.TrimSpace(*message.Agent)
	}
	reasoning := int64(0)
	if message.Tokens.Reasoning != nil {
		reasoning = clampNonNeg(*message.Tokens.Reasoning)
	}
	tokens := TokenBreakdown{
		Input: clampNonNeg(message.Tokens.Input), Output: clampNonNeg(message.Tokens.Output),
		CacheRead: cacheRead, CacheWrite: cacheWrite, Reasoning: reasoning,
	}
	if tokens.Total() == 0 {
		return
	}
	wsKey, wsLabel := "", ""
	if workspaceRoot != nil {
		if key := NormalizeWorkspaceKey(*workspaceRoot); key != "" {
			wsKey, wsLabel = key, WorkspaceLabelFromKey(key)
		}
	} else if message.Path != nil {
		if key := NormalizeWorkspaceKey(message.Path.Root); key != "" {
			wsKey, wsLabel = key, WorkspaceLabelFromKey(key)
		}
	}
	var key string
	if message.ID != nil && strings.TrimSpace(*message.ID) != "" {
		// Embedded ids are globally unique: un-namespaced so the same message
		// in sibling databases collapses.
		key = strings.TrimSpace(*message.ID)
	} else if config.namespaceRowKey {
		key = dbNamespace + ":" + rowID
	} else {
		key = rowID
	}
	emit(UnifiedEntry{
		Client: config.client, ModelID: model, ProviderID: provider,
		SessionID: session, WorkspaceKey: wsKey, WorkspaceLabel: wsLabel,
		TimestampMS: created, Tokens: tokens, Agent: agent, DedupKey: key,
	})
}

func normalizeSchemaEpoch(timestamp float64, seconds bool) int64 {
	if seconds && timestamp <= 1e12 {
		return int64(timestamp * 1000)
	}
	return int64(timestamp)
}

var kiloSchemaQueries = []string{`
	SELECT m.id, m.session_id, m.data, NULL AS workspace_root
	FROM message m
	WHERE json_valid(m.data)
	  AND json_extract(m.data, '$.role') = 'assistant'
	  AND json_extract(m.data, '$.tokens') IS NOT NULL`}

// kiloParser reads ~/.local/share/kilo/kilo.db: a single message table in the
// OpenCode schema; rows without `time` keep the db mtime as their timestamp
// (kilo-specific tolerance).
type kiloParser struct{}

func (kiloParser) Client() string { return "kilo" }

func (kiloParser) Roots(home string) []string {
	return []string{home + "/.local/share/kilo", home + "/Library/Application Support/kilo"}
}

func (kiloParser) Discover(root string) ([]string, error) {
	return WalkFilesWithSuffix(root, "kilo.db")
}

func (kiloParser) ParseFile(file string, mtimeMS int64, emit func(UnifiedEntry)) error {
	parseOpenCodeSchemaSQLite(file, mtimeMS, opencodeSchemaConfig{
		client: "kilo", queries: kiloSchemaQueries,
		fallbackProvider: "kilo", inferFromModel: true,
		payloadSessionID: true, fallbackTimestamp: mtimeMS,
		strictCache: true,
	}, emit)
	return nil
}

var micodeSchemaQueries = []string{`
	SELECT m.id, m.session_id, m.data, NULLIF(s.directory, '') AS workspace_root
	FROM message m
	LEFT JOIN session s ON s.id = m.session_id
	WHERE json_extract(m.data, '$.role') = 'assistant'
	  AND json_extract(m.data, '$.tokens') IS NOT NULL
	ORDER BY m.id, m.session_id`, `
	SELECT m.id, m.session_id, m.data, NULL AS workspace_root
	FROM message m
	WHERE json_extract(m.data, '$.role') = 'assistant'
	  AND json_extract(m.data, '$.tokens') IS NOT NULL
	ORDER BY m.id, m.session_id`}

// micodeParser reads ~/.local/share/mimocode (SQLite, MiMo Code). The lenient
// dialect: half-filled cache objects survive with zeros, epoch seconds
// normalize, and rowid keys namespace per database.
type micodeParser struct{}

func (micodeParser) Client() string { return "micode" }

func (micodeParser) Roots(home string) []string {
	return []string{home + "/.local/share/mimocode", home + "/Library/Application Support/mimocode"}
}

func (micodeParser) Discover(root string) ([]string, error) { return WalkFilesWithSuffix(root, ".db") }

func (micodeParser) ParseFile(file string, mtimeMS int64, emit func(UnifiedEntry)) error {
	parseOpenCodeSchemaSQLite(file, mtimeMS, opencodeSchemaConfig{
		client: "micode", queries: micodeSchemaQueries,
		normalizeSeconds: true, namespaceRowKey: true,
		// MiMo assistant messages may omit cache (or half of it); requiring it
		// would silently drop the message (upstream strict_cache: false).
		strictCache: false,
	}, emit)
	return nil
}

// ---------------------------------------------------------------------------
// Hermes: aggregated per-model rows from the state.db, with a session-totals
// fallback for the sessions the per-model table does not cover.

type hermesParser struct{}

func (hermesParser) Client() string { return "hermes" }

func (hermesParser) Roots(home string) []string { return []string{home + "/.hermes"} }

func (hermesParser) Discover(root string) ([]string, error) {
	dbs, err := WalkFilesWithSuffix(root, "state.db")
	if err != nil {
		return nil, err
	}
	// Named profiles: <hermes-home>/profiles/<profile>/state.db.
	profileDbs, _ := WalkFilesWithSuffix(root+"/profiles", "state.db")
	return append(dbs, profileDbs...), nil
}

const hermesPerModelQuery = `
	SELECT
	    smu.session_id,
	    smu.model,
	    smu.billing_provider,
	    s.started_at,
	    COALESCE(s.message_count, 0),
	    SUM(smu.input_tokens),
	    SUM(smu.output_tokens),
	    SUM(smu.cache_read_tokens),
	    SUM(smu.cache_write_tokens),
	    SUM(smu.reasoning_tokens),
	    SUM(COALESCE(NULLIF(smu.actual_cost_usd, 0), smu.estimated_cost_usd, 0))
	FROM session_model_usage smu
	JOIN sessions s ON s.id = smu.session_id
	WHERE smu.model IS NOT NULL AND TRIM(smu.model) != ''
	GROUP BY smu.session_id, smu.model, smu.billing_provider, s.started_at, s.message_count
	HAVING SUM(smu.input_tokens) > 0 OR SUM(smu.output_tokens) > 0
	    OR SUM(smu.cache_read_tokens) > 0 OR SUM(smu.cache_write_tokens) > 0
	    OR SUM(smu.reasoning_tokens) > 0
	    OR SUM(COALESCE(NULLIF(smu.actual_cost_usd, 0), smu.estimated_cost_usd, 0)) > 0
	ORDER BY smu.session_id,
	    CASE WHEN smu.model = s.model THEN 0 ELSE 1 END,
	    smu.model, smu.billing_provider`

const hermesSessionTotalsQuery = `
	SELECT id, model, billing_provider, started_at, COALESCE(message_count, 0),
	    input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
	    reasoning_tokens, COALESCE(actual_cost_usd, estimated_cost_usd, 0)
	FROM sessions
	WHERE model IS NOT NULL AND TRIM(model) != ''
	  AND (COALESCE(input_tokens, 0) > 0 OR COALESCE(output_tokens, 0) > 0
	    OR COALESCE(cache_read_tokens, 0) > 0 OR COALESCE(cache_write_tokens, 0) > 0
	    OR COALESCE(reasoning_tokens, 0) > 0
	    OR COALESCE(actual_cost_usd, estimated_cost_usd, 0) > 0)`

func (hermesParser) ParseFile(file string, mtimeMS int64, emit func(UnifiedEntry)) error {
	db, err := openReadonlyDB(file)
	if err != nil {
		return nil
	}
	defer db.Close()
	emitHermes := func(session, model string, provider *string, started float64, messages int64, tokens TokenBreakdown, key string) {
		providerID := ""
		if provider != nil {
			providerID = strings.TrimSpace(*provider)
		}
		if providerID == "" {
			providerID = providerForModelOr(model, "hermes")
		}
		if started <= 0 {
			return
		}
		if messages < 0 {
			messages = 0
		}
		emit(UnifiedEntry{
			Client: "hermes", ModelID: model, ProviderID: providerID,
			SessionID: session, Agent: "Hermes Agent",
			TimestampMS: secondsToMS(started), Tokens: tokens,
			MessageCount: int32(messages), DedupKey: key,
		})
	}
	covered := map[string]struct{}{}
	var hasModelUsage int
	if err := db.QueryRow(
		"SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'session_model_usage'").Scan(&hasModelUsage); err == nil && hasModelUsage > 0 {
		rows, err := db.Query(hermesPerModelQuery)
		if err == nil {
			countedSessions := map[string]struct{}{}
			for rows.Next() {
				var session, model string
				var provider *string
				var started float64
				var messages int64
				var in, out, cacheRead, cacheWrite, reasoning int64
				if err := rows.Scan(&session, &model, &provider, &started, &messages, &in, &out, &cacheRead, &cacheWrite, &reasoning, new(any)); err != nil {
					continue
				}
				covered[session] = struct{}{}
				// Only the FIRST row per session carries its message count;
				// later models in the same session zero it.
				if _, seen := countedSessions[session]; seen {
					messages = 0
				} else {
					countedSessions[session] = struct{}{}
				}
				providerKey := "<null>"
				if provider != nil && strings.TrimSpace(*provider) != "" {
					providerKey = strings.TrimSpace(*provider)
				}
				emitHermes(session, model, provider, started, messages, TokenBreakdown{
					Input: clampNonNeg(in), Output: clampNonNeg(out),
					CacheRead: clampNonNeg(cacheRead), CacheWrite: clampNonNeg(cacheWrite),
					Reasoning: clampNonNeg(reasoning),
				}, "hermes:"+session+":"+model+":"+providerKey)
			}
			rows.Close()
		}
	}
	rows, err := db.Query(hermesSessionTotalsQuery)
	if err != nil {
		return nil
	}
	defer rows.Close()
	for rows.Next() {
		var session, model string
		var provider *string
		var started float64
		var messages int64
		var in, out, cacheRead, cacheWrite, reasoning int64
		if err := rows.Scan(&session, &model, &provider, &started, &messages, &in, &out, &cacheRead, &cacheWrite, &reasoning, new(any)); err != nil {
			continue
		}
		// Sessions the per-model pass covered are trusted wholesale; topping
		// up the difference would double-count the models it did record.
		if _, isCovered := covered[session]; isCovered {
			continue
		}
		emitHermes(session, model, provider, started, messages, TokenBreakdown{
			Input: clampNonNeg(in), Output: clampNonNeg(out),
			CacheRead: clampNonNeg(cacheRead), CacheWrite: clampNonNeg(cacheWrite),
			Reasoning: clampNonNeg(reasoning),
		}, session)
	}
	return nil
}

// secondsToMS scales an epoch to milliseconds, leaving values already in
// milliseconds (> 1e12) untouched — some writers mix units.
func secondsToMS(seconds float64) int64 {
	if seconds > 1e12 {
		return int64(seconds)
	}
	return int64(seconds * 1000)
}

// ---------------------------------------------------------------------------
// Unsloth Studio: chat usage from chat_messages.contextUsage plus external
// API events; provider routing decides pricing relevance ("local" and the
// metered set price; anything else is unpriced-prefixed).

type unslothParser struct{}

func (unslothParser) Client() string { return "unsloth" }

func (unslothParser) Roots(home string) []string { return []string{home + "/.unsloth/studio"} }

func (unslothParser) Discover(root string) ([]string, error) {
	return WalkFilesWithSuffix(root, "studio.db")
}

var unslothMeteredProviders = map[string]bool{
	"anthropic": true, "deepseek": true, "gemini": true, "huggingface": true,
	"kimi": true, "mistral": true, "openai": true, "openrouter": true, "qwen": true,
}

func unslothProviderForPricing(providerType string) (string, bool) {
	lowered := strings.ToLower(strings.TrimSpace(providerType))
	if lowered == "" {
		lowered = "unknown"
	}
	if lowered == "local" {
		return "local", true
	}
	if unslothMeteredProviders[lowered] {
		if lowered == "kimi" {
			return "moonshotai", false
		}
		if canonical := canonicalProvider(lowered); canonical != "" {
			return canonical, false
		}
		return lowered, false
	}
	return "unpriced:" + lowered, false
}

func unslothNormalizedTokens(prompt, completion, total, cached, cacheWrite, reasoning int64) (TokenBreakdown, bool) {
	p := clampNonNeg(prompt)
	c := clampNonNeg(completion)
	tot := clampNonNeg(total)
	cacheRead := min64(clampNonNeg(cached), p)
	write := min64(clampNonNeg(cacheWrite), clampNonNeg(p-cacheRead))
	think := min64(clampNonNeg(reasoning), c)
	if tot == 0 && p+c+cacheRead+write+think == 0 {
		return TokenBreakdown{}, false
	}
	// Upstream: input derives from the larger of the reported total and the
	// prompt+completion pair, minus the completion and cache shares.
	input := clampNonNeg(max64(tot, p+c) - c - cacheRead - write)
	return TokenBreakdown{
		Input:     input,
		Output:    clampNonNeg(c - think),
		CacheRead: cacheRead, CacheWrite: write, Reasoning: think,
	}, true
}

func (unslothParser) ParseFile(file string, mtimeMS int64, emit func(UnifiedEntry)) error {
	db, err := openReadonlyDB(file)
	if err != nil {
		return nil
	}
	defer db.Close()
	// Chat lane.
	if rows, err := db.Query(`
		SELECT m.id, m.thread_id, m.created_at,
		    CASE WHEN json_valid(m.metadata_json) THEN json_extract(m.metadata_json, '$.contextUsage.promptTokens') END,
		    CASE WHEN json_valid(m.metadata_json) THEN json_extract(m.metadata_json, '$.contextUsage.completionTokens') END,
		    CASE WHEN json_valid(m.metadata_json) THEN json_extract(m.metadata_json, '$.contextUsage.totalTokens') END,
		    CASE WHEN json_valid(m.metadata_json) THEN json_extract(m.metadata_json, '$.contextUsage.cachedTokens') END,
		    CASE WHEN json_valid(m.metadata_json) THEN json_extract(m.metadata_json, '$.contextUsage.cacheWriteTokens') END,
		    CASE WHEN json_valid(m.metadata_json) THEN json_extract(m.metadata_json, '$.contextUsage.reasoningTokens') END,
		    CASE WHEN json_valid(m.metadata_json) THEN json_extract(m.metadata_json, '$.contextUsage.modelId') END,
		    CASE WHEN json_valid(m.metadata_json) THEN json_extract(m.metadata_json, '$.responseDetails.responseModelId') END,
		    CASE WHEN json_valid(m.metadata_json) THEN json_extract(m.metadata_json, '$.responseDetails.providerType') END,
		    t.model_id
		FROM chat_messages m
		LEFT JOIN chat_threads t ON t.id = m.thread_id
		WHERE m.role = 'assistant'
		ORDER BY m.created_at, m.id`); err == nil {
		for rows.Next() {
			var id, thread string
			var createdAt int64
			var prompt, completion, total, cached, cacheWrite, reasoning *int64
			var requestedModel, responseModel, providerType, threadModel *string
			if err := rows.Scan(&id, &thread, &createdAt, &prompt, &completion, &total, &cached, &cacheWrite, &reasoning, &requestedModel, &responseModel, &providerType, &threadModel); err != nil {
				continue
			}
			tokens, ok := unslothNormalizedTokens(derefI64(prompt), derefI64(completion), derefI64(total), derefI64(cached), derefI64(cacheWrite), derefI64(reasoning))
			if !ok {
				continue
			}
			timestamp := secondsToMS(float64(createdAt))
			if timestamp <= 0 || strings.TrimSpace(id) == "" {
				continue
			}
			model := firstNonEmptyPtr(responseModel, requestedModel, threadModel)
			if model == "" {
				model = "unknown"
			}
			provider, _ := unslothProviderForPricing(derefString(providerType))
			session := thread
			if strings.TrimSpace(session) == "" {
				session = "unsloth:chat:" + id
			}
			emit(UnifiedEntry{
				Client: "unsloth", ModelID: model, ProviderID: provider,
				SessionID: session, Agent: "Unsloth", TimestampMS: timestamp,
				Tokens: tokens, IsTurnStart: true, DedupKey: "unsloth:chat:" + id,
			})
		}
		rows.Close()
	}
	// External API lane.
	if rows, err := db.Query(`
		SELECT id, endpoint, model, prompt_tokens, completion_tokens, total_tokens, created_at
		FROM api_usage_events
		ORDER BY created_at, id`); err == nil {
		for rows.Next() {
			var id, endpoint, model string
			var prompt, completion, total, createdAt int64
			if err := rows.Scan(&id, &endpoint, &model, &prompt, &completion, &total, &createdAt); err != nil {
				continue
			}
			tokens, ok := unslothNormalizedTokens(prompt, completion, total, 0, 0, 0)
			if !ok {
				continue
			}
			timestamp := secondsToMS(float64(createdAt))
			if timestamp <= 0 {
				continue
			}
			model = strings.TrimSpace(model)
			if model == "" {
				model = "unknown"
			}
			emit(UnifiedEntry{
				Client: "unsloth", ModelID: model, ProviderID: "local",
				SessionID: "unsloth:api", Agent: "Unsloth API", TimestampMS: timestamp,
				Tokens: tokens, IsTurnStart: true, DedupKey: "unsloth:api:" + id,
			})
		}
		rows.Close()
	}
	return nil
}

func derefI64(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func firstNonEmptyPtr(values ...*string) string {
	for _, value := range values {
		if value != nil && strings.TrimSpace(*value) != "" {
			return strings.TrimSpace(*value)
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// ZCode: JSONL transcripts under ~/.zcode/projects plus the v2 CLI usage
// database ~/.zcode/cli/db/db.sqlite.

type zcodeParser struct{}

func (zcodeParser) Client() string { return "zcode" }

func (zcodeParser) Roots(home string) []string {
	return []string{home + "/.zcode/projects", home + "/.zcode/cli/db"}
}

func (zcodeParser) Discover(root string) ([]string, error) {
	if strings.HasSuffix(filepath.Clean(root), filepath.Join(".zcode", "cli", "db")) || filepath.Base(filepath.Clean(root)) == "db" {
		return WalkFilesWithSuffix(root, "db.sqlite")
	}
	return WalkJSONLFiles(root)
}

type zcodeUsage struct {
	Input      *int64 `json:"input_tokens"`
	Output     *int64 `json:"output_tokens"`
	CacheRead  *int64 `json:"input_cache_read"`
	CacheWrite *int64 `json:"input_cache_creation"`
	Reasoning  *int64 `json:"reasoning_tokens"`
	Total      *int64 `json:"total_tokens"`
	// Alias spellings older and newer writers used; the first populated
	// spelling wins per bucket.
	InputAlt       *int64 `json:"prompt_tokens"`
	InputAlt2      *int64 `json:"inputTokens"`
	OutputAlt      *int64 `json:"completion_tokens"`
	OutputAlt2     *int64 `json:"outputTokens"`
	CacheReadAlt   *int64 `json:"cache_read_tokens"`
	CacheReadAlt2  *int64 `json:"cacheReadTokens"`
	CacheWriteAlt  *int64 `json:"cache_write_tokens"`
	CacheWriteAlt2 *int64 `json:"cacheCreationTokens"`
	ReasoningAlt   *int64 `json:"reasoningTokens"`
	TotalAlt       *int64 `json:"totalTokens"`
}

func (u *zcodeUsage) UnmarshalJSON(raw []byte) error {
	type plain zcodeUsage
	var value plain
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	*u = zcodeUsage(value)
	first := func(values ...*int64) *int64 {
		for _, candidate := range values {
			if candidate != nil {
				return candidate
			}
		}
		return nil
	}
	u.Input = first(u.Input, u.InputAlt, u.InputAlt2)
	u.Output = first(u.Output, u.OutputAlt, u.OutputAlt2)
	u.CacheRead = first(u.CacheRead, u.CacheReadAlt, u.CacheReadAlt2)
	u.CacheWrite = first(u.CacheWrite, u.CacheWriteAlt, u.CacheWriteAlt2)
	u.Reasoning = first(u.Reasoning, u.ReasoningAlt)
	u.Total = first(u.Total, u.TotalAlt)
	return nil
}

// normalizeZcodeIO applies the inclusive-total test: when the reported total
// equals input+output but not the exclusive total, the input side carries the
// cache buckets and the output side the reasoning.
func normalizeZcodeIO(input, output, cacheRead, cacheWrite, reasoning int64, total *int64) (int64, int64) {
	input, output = clampNonNeg(input), clampNonNeg(output)
	cacheOverlap := clampNonNeg(cacheRead) + clampNonNeg(cacheWrite)
	reasoning = clampNonNeg(reasoning)
	if total == nil {
		return input, output
	}
	inclusive := input + output
	exclusive := inclusive + cacheOverlap + reasoning
	if (cacheOverlap > 0 || reasoning > 0) && *total == inclusive && inclusive != exclusive {
		return clampNonNeg(input - cacheOverlap), clampNonNeg(output - reasoning)
	}
	return input, output
}

func (u *zcodeUsage) breakdown() (TokenBreakdown, bool) {
	if u == nil {
		return TokenBreakdown{}, false
	}
	rawIn, rawOut := nonNeg(u.Input), nonNeg(u.Output)
	cacheRead, cacheWrite, reasoning := nonNeg(u.CacheRead), nonNeg(u.CacheWrite), nonNeg(u.Reasoning)
	if rawIn+rawOut+cacheRead+cacheWrite+reasoning == 0 {
		return TokenBreakdown{}, false
	}
	input, output := normalizeZcodeIO(rawIn, rawOut, cacheRead, cacheWrite, reasoning, u.Total)
	tokens := TokenBreakdown{Input: input, Output: output, CacheRead: cacheRead, CacheWrite: cacheWrite, Reasoning: reasoning}
	return tokens, tokens.Total() > 0
}

func (zcodeParser) ParseFile(file string, mtimeMS int64, emit func(UnifiedEntry)) error {
	if strings.HasSuffix(file, ".sqlite") {
		parseZcodeSQLite(file, mtimeMS, emit)
		return nil
	}
	parseZcodeJSONL(file, mtimeMS, emit)
	return nil
}

func parseZcodeJSONL(file string, mtimeMS int64, emit func(UnifiedEntry)) {
	session := strings.TrimSuffix(filepath.Base(file), ".jsonl")
	var sessionID, modelID string
	contextChars := int64(0)
	pendingTurnStart := false
	assistantIndex := 0
	forEachJSONLine(file, mtimeMS, func(raw []byte) {
		var entry struct {
			Role      *string         `json:"role"`
			Content   json.RawMessage `json:"content"`
			Usage     *zcodeUsage     `json:"usage"`
			TokenUse  *zcodeUsage     `json:"token_usage"`
			Model     *string         `json:"model"`
			Timestamp *string         `json:"timestamp"`
			SessionID *string         `json:"sessionId"`
		}
		if json.Unmarshal(raw, &entry) != nil {
			return
		}
		if sessionID == "" && entry.SessionID != nil && strings.TrimSpace(*entry.SessionID) != "" {
			sessionID = strings.TrimSpace(*entry.SessionID)
		}
		if entry.Model != nil && strings.TrimSpace(*entry.Model) != "" {
			modelID = strings.ToLower(strings.TrimSpace(*entry.Model))
		}
		model := modelID
		if model == "" {
			model = "glm-5.2"
		}
		chars := contentCharCount(entry.Content)
		role := ""
		if entry.Role != nil {
			role = *entry.Role
		}
		if role != "assistant" {
			if role == "user" {
				pendingTurnStart = true
			}
			contextChars += chars
			return
		}
		tokens, hasUsage := entry.Usage.breakdown()
		if !hasUsage {
			tokens, hasUsage = entry.TokenUse.breakdown()
		}
		if !hasUsage {
			input := estimateTokensFromChars(contextChars)
			output := estimateTokensFromChars(chars)
			contextChars += chars
			if input+output == 0 {
				return
			}
			tokens = TokenBreakdown{Input: input, Output: output}
		} else {
			contextChars += chars
		}
		resolvedSession := sessionID
		if resolvedSession == "" {
			resolvedSession = session
		}
		timestamp := mtimeMS
		if entry.Timestamp != nil {
			if parsed, ok := ParseRFC3339MS(*entry.Timestamp); ok {
				timestamp = parsed
			}
		}
		emit(UnifiedEntry{
			Client: "zcode", ModelID: model, ProviderID: "zhipu",
			SessionID: resolvedSession, TimestampMS: timestamp, Tokens: tokens,
			IsTurnStart: pendingTurnStart, MessageCount: 1,
			DedupKey: "zcode:" + resolvedSession + ":" + itoa(assistantIndex),
		})
		assistantIndex++
		pendingTurnStart = false
	})
}

func parseZcodeSQLite(file string, mtimeMS int64, emit func(UnifiedEntry)) {
	db, err := openReadonlyDB(file)
	if err != nil {
		return
	}
	defer db.Close()
	query := `
		SELECT mu.id, NULLIF(mu.session_id, ''), NULLIF(mu.turn_id, ''),
		    NULLIF(mu.model_id, ''), mu.started_at, mu.completed_at, mu.duration_ms,
		    mu.input_tokens, mu.output_tokens, mu.reasoning_tokens,
		    mu.cache_read_input_tokens, mu.cache_creation_input_tokens,
		    mu.computed_total_tokens, NULLIF(mu.agent, ''), NULLIF(mu.mode, ''),
		    NULLIF(s.directory, ''), NULLIF(s.path, '')
		FROM model_usage mu
		LEFT JOIN session s ON s.id = mu.session_id
		WHERE COALESCE(mu.input_tokens, 0) + COALESCE(mu.output_tokens, 0)
		    + COALESCE(mu.reasoning_tokens, 0) + COALESCE(mu.cache_read_input_tokens, 0)
		    + COALESCE(mu.cache_creation_input_tokens, 0) > 0
		ORDER BY COALESCE(mu.completed_at, mu.started_at, 0), mu.id`
	rows, err := db.Query(query)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var session, turn, model, agent, mode, directory, path *string
		var startedAt, completedAt, duration, in, out, reasoning, cacheRead, cacheWrite, total *int64
		if err := rows.Scan(&id, &session, &turn, &model, &startedAt, &completedAt, &duration, &in, &out, &reasoning, &cacheRead, &cacheWrite, &total, &agent, &mode, &directory, &path); err != nil {
			continue
		}
		rawIn, rawOut := derefI64(in), derefI64(out)
		cacheReadN, cacheWriteN, reasoningN := derefI64(cacheRead), derefI64(cacheWrite), derefI64(reasoning)
		input, output := normalizeZcodeIO(rawIn, rawOut, cacheReadN, cacheWriteN, reasoningN, total)
		tokens := TokenBreakdown{
			Input: input, Output: output,
			CacheRead: clampNonNeg(cacheReadN), CacheWrite: clampNonNeg(cacheWriteN),
			Reasoning: clampNonNeg(reasoningN),
		}
		if tokens.Total() == 0 {
			continue
		}
		modelID := "glm-5.2"
		if model != nil && strings.TrimSpace(*model) != "" {
			modelID = strings.ToLower(strings.TrimSpace(*model))
		}
		timestamp := resolveZcodeTimestamp(startedAt, completedAt, duration, mtimeMS)
		sessionID := "unknown"
		if session != nil {
			sessionID = *session
		}
		wsRoot := firstNonEmptyPtr(directory, path)
		wsKey, wsLabel := "", ""
		if key := NormalizeWorkspaceKey(wsRoot); key != "" {
			wsKey, wsLabel = key, WorkspaceLabelFromKey(key)
		}
		agentName := firstNonEmptyPtr(agent, mode)
		emit(UnifiedEntry{
			Client: "zcode", ModelID: modelID, ProviderID: "zhipu",
			SessionID: sessionID, WorkspaceKey: wsKey, WorkspaceLabel: wsLabel,
			TimestampMS: timestamp, Tokens: tokens, Agent: agentName,
			DedupKey: "zcode-sqlite:" + id,
		})
	}
}

func resolveZcodeTimestamp(startedAt, completedAt, duration *int64, fallback int64) int64 {
	if startedAt != nil && *startedAt > 0 {
		return *startedAt
	}
	if completedAt != nil && *completedAt > 0 {
		if duration != nil && *duration > 0 {
			return clampNonNeg(*completedAt - *duration)
		}
		return *completedAt
	}
	return fallback
}

// ---------------------------------------------------------------------------
// Kiro: CLI snapshots (~/.kiro/sessions/cli/*.json + messages jsonl) and the
// IDE sess_<uuid> session.json + messages.jsonl pair. Token counts in both
// sources are estimates from context usage and text length.

type kiroParser struct{}

func (kiroParser) Client() string { return "kiro" }

func (kiroParser) Roots(home string) []string { return []string{home + "/.kiro/sessions"} }

func (kiroParser) Discover(root string) ([]string, error) {
	// cli/*.json session headers (the messages jsonl is read as a sibling);
	// IDE sess_<uuid>/session.json pairs.
	var files []string
	cli, _ := WalkFilesWithSuffix(root+"/cli", ".json")
	for _, file := range cli {
		if !strings.HasSuffix(file, ".jsonl") {
			files = append(files, file)
		}
	}
	entries, err := filepath.Glob(filepath.Join(root, "*", "sess_*", "session.json"))
	if err == nil {
		files = append(files, entries...)
	}
	return files, nil
}

func (kiroParser) ParseFile(file string, mtimeMS int64, emit func(UnifiedEntry)) error {
	if filepath.Base(file) == "session.json" {
		parseKiroIDESession(file, mtimeMS, emit)
		return nil
	}
	parseKiroCLIFile(file, mtimeMS, emit)
	return nil
}

// kiroTurnMeta is one user turn's snapshot metadata: explicit token counts
// (usually zero), the context-usage estimate inputs, and the message ids the
// turn aggregates.
type kiroTurnMeta struct {
	InputTokenCount        *int64          `json:"input_token_count"`
	OutputTokenCount       *int64          `json:"output_token_count"`
	ContextUsagePercentage *float64        `json:"context_usage_percentage"`
	EndTimestamp           json.RawMessage `json:"end_timestamp"`
	TotalRequestCount      *int64          `json:"total_request_count"`
	MessageIDs             []*string       `json:"message_ids"`
}

func parseKiroCLIFile(file string, mtimeMS int64, emit func(UnifiedEntry)) {
	body, ok := readFileLimited(file, maxFileBytes)
	if !ok {
		return
	}
	var header struct {
		SessionID    *string `json:"session_id"`
		CWD          *string `json:"cwd"`
		SessionState *struct {
			RTSModelState *struct {
				ModelInfo *struct {
					ModelID             *string `json:"model_id"`
					ContextWindowTokens *int64  `json:"context_window_tokens"`
				} `json:"model_info"`
			} `json:"rts_model_state"`
			ConversationMetadata *struct {
				UserTurnMetadatas []kiroTurnMeta `json:"user_turn_metadatas"`
			} `json:"conversation_metadata"`
		} `json:"session_state"`
	}
	if json.Unmarshal(body, &header) != nil {
		return
	}
	session := ""
	if header.SessionID != nil {
		session = strings.TrimSpace(*header.SessionID)
	}
	if session == "" {
		session = strings.TrimSuffix(filepath.Base(file), ".json")
	}
	model := "auto"
	contextWindow := int64(0)
	if header.SessionState != nil && header.SessionState.RTSModelState != nil && header.SessionState.RTSModelState.ModelInfo != nil {
		info := header.SessionState.RTSModelState.ModelInfo
		if info.ModelID != nil && strings.TrimSpace(*info.ModelID) != "" {
			model = strings.TrimSpace(*info.ModelID)
		}
		if info.ContextWindowTokens != nil {
			contextWindow = *info.ContextWindowTokens
		}
	}
	wsKey, wsLabel := "", ""
	if header.CWD != nil {
		if key := NormalizeWorkspaceKey(*header.CWD); key != "" {
			wsKey, wsLabel = key, WorkspaceLabelFromKey(key)
		}
	}
	turns := []kiroTurnMeta{}
	if header.SessionState != nil && header.SessionState.ConversationMetadata != nil {
		turns = header.SessionState.ConversationMetadata.UserTurnMetadatas
	}
	// The sibling messages jsonl supplies prompt/assistant character counts.
	// The CLI keeps messages in <stem>.jsonl beside the header (upstream
	// with_extension).
	messagesPath := strings.TrimSuffix(file, ".json") + ".jsonl"
	if _, err := os.Stat(messagesPath); err != nil {
		return
	}
	type turnContent struct {
		promptChars    int64
		assistantChars int64
		promptTS       int64
	}
	contents := map[string]*turnContent{}
	promptChars := int64(0)
	var promptTS int64
	var lastMessageID string
	forEachJSONLine(messagesPath, mtimeMS, func(raw []byte) {
		var entry struct {
			Kind string `json:"kind"`
			Data *struct {
				MessageID *string `json:"message_id"`
				Content   []struct {
					Kind *string `json:"kind"`
					Data *string `json:"data"`
				} `json:"content"`
				Meta *struct {
					Timestamp *float64 `json:"timestamp"`
				} `json:"meta"`
			} `json:"data"`
		}
		if json.Unmarshal(raw, &entry) != nil || entry.Data == nil {
			return
		}
		textChars := int64(0)
		for _, part := range entry.Data.Content {
			if part.Kind == nil || *part.Kind == "text" {
				if part.Data != nil {
					textChars += int64(len([]rune(*part.Data)))
				}
			}
		}
		switch entry.Kind {
		case "Prompt":
			promptChars = textChars
			promptTS = 0
			if entry.Data.Meta != nil && entry.Data.Meta.Timestamp != nil {
				promptTS = secondsToMS(*entry.Data.Meta.Timestamp)
			}
		case "AssistantMessage":
			messageID := lastMessageID
			if entry.Data.MessageID != nil && strings.TrimSpace(*entry.Data.MessageID) != "" {
				messageID = strings.TrimSpace(*entry.Data.MessageID)
			}
			if messageID == "" {
				return
			}
			bucket := contents[messageID]
			if bucket == nil {
				bucket = &turnContent{}
				contents[messageID] = bucket
			}
			bucket.promptChars += promptChars
			if bucket.promptTS == 0 {
				bucket.promptTS = promptTS
			}
			bucket.assistantChars += textChars
			promptChars = 0
		}
		if entry.Data.MessageID != nil && strings.TrimSpace(*entry.Data.MessageID) != "" {
			lastMessageID = strings.TrimSpace(*entry.Data.MessageID)
		}
	})
	for index, turn := range turns {
		promptChars, assistantChars, promptTS := int64(0), int64(0), int64(0)
		for _, messageID := range turn.MessageIDs {
			content := contents[strings.TrimSpace(derefString(messageID))]
			if content == nil {
				continue
			}
			promptChars += content.promptChars
			assistantChars += content.assistantChars
			if promptTS == 0 {
				promptTS = content.promptTS
			}
		}
		explicitInput, explicitOutput := int64(0), int64(0)
		if turn.InputTokenCount != nil {
			explicitInput = clampNonNeg(*turn.InputTokenCount)
		}
		if turn.OutputTokenCount != nil {
			explicitOutput = clampNonNeg(*turn.OutputTokenCount)
		}
		input := explicitInput
		if input == 0 {
			if turn.ContextUsagePercentage != nil && *turn.ContextUsagePercentage > 0 && contextWindow > 0 {
				input = int64(float64(contextWindow) * *turn.ContextUsagePercentage / 100.0)
			} else {
				input = estimateTokensFromChars(promptChars)
			}
		}
		output := explicitOutput
		if output == 0 {
			output = estimateTokensFromChars(assistantChars)
		}
		if input+output == 0 {
			continue
		}
		endTS := int64(0)
		if ts, ok := parseKiroTimestamp(turn.EndTimestamp); ok {
			endTS = ts
		}
		timestamp := promptTS
		if timestamp == 0 {
			timestamp = endTS
		}
		if timestamp == 0 {
			timestamp = mtimeMS
		}
		requests := int64(1)
		if turn.TotalRequestCount != nil && *turn.TotalRequestCount > 1 {
			requests = *turn.TotalRequestCount
		}
		emit(UnifiedEntry{
			Client: "kiro", ModelID: model, ProviderID: "aws",
			SessionID: session, WorkspaceKey: wsKey, WorkspaceLabel: wsLabel,
			TimestampMS: timestamp, Tokens: TokenBreakdown{Input: input, Output: output},
			MessageCount: int32(requests), IsTurnStart: true,
			DedupKey: "kiro:" + session + ":" + itoa(index),
		})
	}
}

func parseKiroIDESession(file string, mtimeMS int64, emit func(UnifiedEntry)) {
	body, ok := readFileLimited(file, maxFileBytes)
	if !ok {
		return
	}
	var session struct {
		ID             *string  `json:"id"`
		ModelID        *string  `json:"model_id"`
		WorkspacePaths []string `json:"workspace_paths"`
	}
	if json.Unmarshal(body, &session) != nil {
		return
	}
	messagesPath := fileDir(file) + "/messages.jsonl"
	if _, err := os.Stat(messagesPath); err != nil {
		return
	}
	sessionID := ""
	if session.ID != nil && strings.TrimSpace(*session.ID) != "" {
		sessionID = strings.TrimSpace(*session.ID)
	} else {
		sessionID = dirBase(fileDir(file))
	}
	wsSource := ""
	if len(session.WorkspacePaths) > 0 {
		wsSource = session.WorkspacePaths[0]
	} else {
		wsSource = dirBase(fileDir(fileDir(file)))
	}
	wsKey, wsLabel := "", ""
	if key := NormalizeWorkspaceKey(wsSource); key != "" {
		wsKey, wsLabel = key, WorkspaceLabelFromKey(key)
	}
	model := "auto"
	if session.ModelID != nil && strings.TrimSpace(*session.ModelID) != "" {
		model = strings.TrimSpace(*session.ModelID)
	}
	const defaultContextWindow = 200_000
	type ideTurn struct {
		promptChars    int64
		assistantChars int64
		promptTS       int64
	}
	var turns []*ideTurn
	var current *ideTurn
	forEachJSONLine(messagesPath, mtimeMS, func(raw []byte) {
		var entry struct {
			Timestamp string `json:"timestamp"`
			Payload   *struct {
				Type    string `json:"type"`
				Content string `json:"content"`
			} `json:"payload"`
		}
		if json.Unmarshal(raw, &entry) != nil || entry.Payload == nil {
			return
		}
		timestamp := int64(0)
		if parsed, ok := ParseRFC3339MS(entry.Timestamp); ok {
			timestamp = parsed
		}
		chars := int64(len([]rune(entry.Payload.Content)))
		switch entry.Payload.Type {
		case "user":
			if current == nil {
				current = &ideTurn{}
				turns = append(turns, current)
			}
			current.promptChars += chars
			if current.promptTS == 0 {
				current.promptTS = timestamp
			}
		case "assistant":
			if current != nil {
				current.assistantChars += chars
			}
		}
	})
	for index, turn := range turns {
		input := estimateTokensFromChars(turn.promptChars)
		output := estimateTokensFromChars(turn.assistantChars)
		if input+output == 0 {
			continue
		}
		_ = defaultContextWindow
		timestamp := turn.promptTS
		if timestamp == 0 {
			timestamp = mtimeMS
		}
		emit(UnifiedEntry{
			Client: "kiro", ModelID: model, ProviderID: "amazon-bedrock",
			SessionID: sessionID, WorkspaceKey: wsKey, WorkspaceLabel: wsLabel,
			TimestampMS: timestamp, Tokens: TokenBreakdown{Input: input, Output: output},
			IsTurnStart: index == 0,
			DedupKey:    "kiro-ide:" + sessionID + ":" + itoa(index),
		})
	}
}

func parseKiroTimestamp(raw json.RawMessage) (int64, bool) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return 0, false
	}
	if trimmed[0] == '"' {
		var text string
		if json.Unmarshal(raw, &text) != nil {
			return 0, false
		}
		if ts, ok := ParseRFC3339MS(text); ok {
			return ts, true
		}
		return 0, false
	}
	var number float64
	if json.Unmarshal(raw, &number) != nil {
		return 0, false
	}
	if number <= 0 {
		return 0, false
	}
	if number < 1e12 {
		return secondsToMS(number), true
	}
	return int64(number), true
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

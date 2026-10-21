package sessionstats

import (
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// Batch 3 golden fixtures: VS Code extension storages, the OpenCode-schema
// SQLite dialects, and the remaining SQLite clients.

func TestParseTaskLogFamily(t *testing.T) {
	root := t.TempDir()
	history := "<environment_details>\n<model>claude-sonnet-4</model>\n<name>ClineAgent</name>\n</environment_details>"
	uiBody := `[
  {"type":"say","say":"api_req_started","ts":"2026-01-01T00:00:01Z","text":"{\"cost\":0.05,\"tokensIn\":50,\"tokensOut\":15,\"cacheReads\":7,\"cacheWrites\":3}","modelInfo":{"providerId":"anthropic","modelId":"claude-sonnet-4.6"}},
  {"type":"say","say":"assistant_message","text":"noise"}
]`
	cases := []struct {
		name   string
		parser Parser
	}{
		{"cline", clineParser{}},
		{"roocode", roocodeParser{}},
		{"kilocode", kilocodeParser{}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			dir := filepath.Join(root, testCase.name, "task-1")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "ui_messages.json"), []byte(uiBody), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "api_conversation_history.json"), []byte(history), 0o644); err != nil {
				t.Fatal(err)
			}
			entries := collectEntries(t, testCase.parser, filepath.Join(dir, "ui_messages.json"))
			entry := mustEmit(t, testCase.name, entries)
			if entry.Client != testCase.name || entry.SessionID != "task-1" {
				t.Fatalf("identity: %+v", entry)
			}
			// Per-entry modelInfo wins over the file-level <model> tag.
			if entry.ModelID != "claude-sonnet-4.6" || entry.ProviderID != "anthropic" {
				t.Fatalf("routing: %+v", entry)
			}
			if entry.Tokens.Input != 50 || entry.Tokens.Output != 15 || entry.Tokens.CacheRead != 7 || entry.Tokens.CacheWrite != 3 {
				t.Fatalf("buckets: %+v", entry.Tokens)
			}
		})
	}
}

func TestParseClineCLI(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "sess-1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"sessionId":"cli-1","messages":[
  {"id":"m1","role":"user","ts":"2026-01-01T00:00:00Z","content":[{"type":"text","text":"hi"}]},
  {"id":"m2","role":"assistant","ts":"2026-01-01T00:00:02Z","modelInfo":{"id":"glm-5.3","provider":"zhipu"},"metrics":{"inputTokens":100,"outputTokens":20,"cacheReadTokens":40,"cacheWriteTokens":10,"cost":0.01}}
]}`
	if err := os.WriteFile(filepath.Join(dir, "sess-1.messages.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sess-1.json"), []byte(`{"session_id":"cli-1","provider":"zhipu","model":"glm-5.3","workspace_root":"/tmp/w"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	entry := mustEmit(t, "cline-cli", collectEntries(t, clineParser{}, filepath.Join(dir, "sess-1.messages.json")))
	if entry.SessionID != "cli-1" || entry.ModelID != "glm-5.3" {
		t.Fatalf("manifest identity: %+v", entry)
	}
	if entry.Tokens.Input != 50 || entry.Tokens.CacheRead != 40 || entry.Tokens.CacheWrite != 10 {
		t.Fatalf("input splits the cache out: %+v", entry.Tokens)
	}
	if entry.WorkspaceKey == "" {
		t.Fatalf("workspace from manifest: %+v", entry)
	}
}

func TestParseCopilotVSCode(t *testing.T) {
	root := t.TempDir()
	chatDir := filepath.Join(root, "hash1", "chatSessions")
	if err := os.MkdirAll(chatDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// kind-0 full state, kind-1 patch landing promptTokens via
	// result.metadata, kind-2 truncation noise.
	body := `{"kind":0,"v":{"requests":[{"timestamp":1700000000000,"modelId":"copilot/gpt-5.3","result":{"metadata":{"outputTokens":77,"resolvedModel":"gpt-5.3","toolCallRounds":[{"thinking":{"tokens":12}}]}}}]}}
{"kind":1,"k":["requests",0,"promptTokens"],"v":4242}
{"kind":2,"k":["other"],"v":{}}`
	file := filepath.Join(chatDir, "session-a.jsonl")
	if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "hash1", "workspace.json"), []byte(`{"folder":"file:///tmp/copilot-ws"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	entry := mustEmit(t, "copilot-vscode", collectEntries(t, copilotVSCodeParser{}, file))
	if entry.ModelID != "gpt-5.3" {
		t.Fatalf("resolvedModel wins: %+v", entry)
	}
	if entry.Tokens.Input != 4242 || entry.Tokens.Output != 77 || entry.Tokens.Reasoning != 12 {
		t.Fatalf("projection buckets: %+v", entry.Tokens)
	}
	if entry.WorkspaceKey == "" {
		t.Fatalf("workspace.json: %+v", entry)
	}
	if entry.DedupKey != "copilot-vscode:session-a:1700000000000" {
		t.Fatalf("key: %s", entry.DedupKey)
	}
}

func TestParseKiloSQLite(t *testing.T) {
	root := t.TempDir()
	dbFile := filepath.Join(root, "kilo.db")
	db, err := sql.Open("sqlite", dbFile)
	if err != nil {
		t.Skip("sqlite unavailable")
	}
	if _, err := db.Exec(`CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, data TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	data := `{"id":"embedded-msg-1","session_id":"sess-1","role":"assistant","modelID":"claude-sonnet-4","providerID":"anthropic","cost":0.42,"agent":"architect","tokens":{"input":1200,"output":300,"reasoning":40,"cache":{"read":75,"write":25}},"time":{"created":1700000000123}}`
	if _, err := db.Exec(`INSERT INTO message VALUES ('row-1', 'sess-1', ?)`, data); err != nil {
		t.Fatal(err)
	}
	db.Close()
	entry := mustEmit(t, "kilo", collectEntries(t, kiloParser{}, dbFile))
	if entry.ModelID != "claude-sonnet-4" || entry.ProviderID != "anthropic" {
		t.Fatalf("identity: %+v", entry)
	}
	if entry.Tokens.Input != 1200 || entry.Tokens.CacheRead != 75 || entry.Tokens.Reasoning != 40 {
		t.Fatalf("buckets: %+v", entry.Tokens)
	}
	// Kilo keys on the embedded id, un-namespaced for cross-db collapse.
	if entry.DedupKey != "embedded-msg-1" {
		t.Fatalf("key: %s", entry.DedupKey)
	}
	if entry.Agent != "architect" {
		t.Fatalf("agent passthrough: %s", entry.Agent)
	}
}

func TestParseHermesSQLite(t *testing.T) {
	root := t.TempDir()
	dbFile := filepath.Join(root, "state.db")
	db, err := sql.Open("sqlite", dbFile)
	if err != nil {
		t.Skip("sqlite unavailable")
	}
	for _, statement := range []string{
		`CREATE TABLE sessions (id TEXT PRIMARY KEY, model TEXT, billing_provider TEXT, started_at REAL, message_count INTEGER, input_tokens INTEGER, output_tokens INTEGER, cache_read_tokens INTEGER, cache_write_tokens INTEGER, reasoning_tokens INTEGER, actual_cost_usd REAL, estimated_cost_usd REAL)`,
		`CREATE TABLE session_model_usage (session_id TEXT, model TEXT, billing_provider TEXT, input_tokens INTEGER, output_tokens INTEGER, cache_read_tokens INTEGER, cache_write_tokens INTEGER, reasoning_tokens INTEGER, actual_cost_usd REAL, estimated_cost_usd REAL)`,
		`INSERT INTO sessions VALUES ('s1','glm-5.3','zhipu',1700000000,5,100,20,0,0,0,0.5,0)`,
		`INSERT INTO sessions VALUES ('s2','unknown','',1700000100,0,30,5,0,0,0,0,0)`,
		`INSERT INTO session_model_usage VALUES ('s1','glm-5.3','zhipu',90,18,10,2,4,0.3,0)`,
		`INSERT INTO session_model_usage VALUES ('s1','glm-5.3-flash','zhipu',10,2,0,0,0,0.1,0)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	entries := collectEntries(t, hermesParser{}, dbFile)
	if len(entries) != 3 {
		t.Fatalf("per-model rows for s1 + session totals for uncovered s2: %d", len(entries))
	}
	s1, s2 := entries[0], entries[2]
	if s1.SessionID != "s1" || s1.Tokens.Input != 90 || s1.MessageCount != 5 {
		t.Fatalf("first per-model row carries the session's message count: %+v", s1)
	}
	if entries[1].MessageCount != 0 {
		t.Fatalf("later models in one session zero the count: %+v", entries[1])
	}
	if s2.SessionID != "s2" || s2.Tokens.Input != 30 || s2.DedupKey != "s2" {
		t.Fatalf("session-totals fallback: %+v", s2)
	}
}

func TestParseUnslothSQLite(t *testing.T) {
	root := t.TempDir()
	dbFile := filepath.Join(root, "studio.db")
	db, err := sql.Open("sqlite", dbFile)
	if err != nil {
		t.Skip("sqlite unavailable")
	}
	for _, statement := range []string{
		`CREATE TABLE chat_threads (id TEXT PRIMARY KEY, model_id TEXT)`,
		`CREATE TABLE chat_messages (id TEXT PRIMARY KEY, thread_id TEXT NOT NULL, role TEXT NOT NULL, metadata_json TEXT, created_at INTEGER NOT NULL)`,
		`CREATE TABLE api_usage_events (id TEXT PRIMARY KEY, subject TEXT, endpoint TEXT, model TEXT, status TEXT, prompt_tokens INTEGER, completion_tokens INTEGER, total_tokens INTEGER, created_at INTEGER)`,
		`INSERT INTO chat_threads VALUES ('th-1','fallback-model')`,
		`INSERT INTO chat_messages VALUES ('m-1','th-1','assistant','{"contextUsage":{"promptTokens":100,"completionTokens":30,"cachedTokens":40,"reasoningTokens":10},"contextUsage2":{},"responseDetails":{"responseModelId":"qwen/qwen3-coder","providerType":"qwen"}}',1700000000)`,
		`INSERT INTO api_usage_events VALUES ('e-1','u','/v1','local-model','done',50,10,60,1700000100)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	entries := collectEntries(t, unslothParser{}, dbFile)
	if len(entries) != 2 {
		t.Fatalf("chat + api lanes: %d", len(entries))
	}
	chat, api := entries[0], entries[1]
	if chat.ModelID != "qwen/qwen3-coder" || chat.ProviderID != "alibaba" {
		t.Fatalf("response model + metered provider: %+v", chat)
	}
	if chat.Tokens.Input != 60 || chat.Tokens.CacheRead != 40 || chat.Tokens.Output != 20 || chat.Tokens.Reasoning != 10 {
		t.Fatalf("buckets: %+v", chat.Tokens)
	}
	if api.ProviderID != "local" || api.SessionID != "unsloth:api" || api.Agent != "Unsloth API" {
		t.Fatalf("api lane: %+v", api)
	}
}

func TestParseZcode(t *testing.T) {
	root := t.TempDir()
	// JSONL lane: total==input+output proves inclusive input → split cache.
	body := `{"role":"user","content":"hello"}
{"role":"assistant","model":"GLM-5.3","timestamp":"2026-01-01T00:00:01Z","sessionId":"z-1","content":"hi","usage":{"input_tokens":100,"output_tokens":30,"input_cache_read":40,"reasoning_tokens":10,"total_tokens":130}}`
	file := writeFixture(t, root, "proj/z-1.jsonl", body)
	entry := mustEmit(t, "zcode", collectEntries(t, zcodeParser{}, file))
	if entry.ModelID != "glm-5.3" || entry.ProviderID != "zhipu" {
		t.Fatalf("identity: %+v", entry)
	}
	if entry.Tokens.Input != 60 || entry.Tokens.CacheRead != 40 || entry.Tokens.Output != 20 || entry.Tokens.Reasoning != 10 {
		t.Fatalf("inclusive split: %+v", entry.Tokens)
	}
}

func TestParseKiroCLI(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "cli")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	header := `{"session_id":"k-1","cwd":"/tmp/kiro-ws","session_state":{"rts_model_state":{"model_info":{"model_id":"claude-sonnet-4-kiro","context_window_tokens":200000}},"conversation_metadata":{"user_turn_metadatas":[{"context_usage_percentage":25.0,"end_timestamp":"2026-01-01T00:00:10Z","total_request_count":2,"message_ids":["a1"]}]}}}`
	if err := os.WriteFile(filepath.Join(dir, "k-1.json"), []byte(header), 0o644); err != nil {
		t.Fatal(err)
	}
	// The messages file is <stem>.jsonl; turns join through message_ids.
	messages := `{"kind":"Prompt","data":{"message_id":"p1","content":[{"kind":"text","data":"hello kiro"}],"meta":{"timestamp":1700000000}}}
{"kind":"AssistantMessage","data":{"message_id":"a1","content":[{"kind":"text","data":"response here"}]}}`
	if err := os.WriteFile(filepath.Join(dir, "k-1.jsonl"), []byte(messages), 0o644); err != nil {
		t.Fatal(err)
	}
	entry := mustEmit(t, "kiro", collectEntries(t, kiroParser{}, filepath.Join(dir, "k-1.json")))
	if entry.ModelID != "claude-sonnet-4-kiro" || entry.SessionID != "k-1" {
		t.Fatalf("identity: %+v", entry)
	}
	// 25% of a 200k window: the context-usage estimate wins over chars.
	if entry.Tokens.Input != 50000 {
		t.Fatalf("context estimate: %+v", entry.Tokens)
	}
	if entry.MessageCount != 2 || !entry.IsTurnStart {
		t.Fatalf("meta: %+v", entry)
	}
}

func TestSyntheticFilter(t *testing.T) {
	root := t.TempDir()
	// A pi session routed through the synthetic.new gateway: model/provider
	// carry the gateway spellings, the filter rewrites them at scan time.
	body := `{"type":"session","id":"s1"}
{"type":"message","id":"m1","timestamp":"2026-01-01T00:00:01Z","message":{"role":"assistant","model":"accounts/fireworks/accounts/deepseek/models/deepseek-v4","provider":"glhf","responseId":"r1","usage":{"input":10,"output":2,"totalTokens":12}}}`
	file := writeFixture(t, root, "s1.jsonl", body)
	entries := collectEntries(t, piParser{client: "pi", roots: piRoots}, file)
	entry := mustEmit(t, "pi", entries)
	if !normalizeSyntheticEntry(&entry) {
		t.Fatal("gateway traffic must be detected")
	}
	if entry.ModelID != "deepseek-v4" || entry.ProviderID != "glhf" {
		t.Fatalf("rewrite: %+v", entry)
	}
	normalized := UnifiedEntry{ModelID: "hf:deepseek-ai/deepseek-v3.2", ProviderID: ""}
	normalizeSyntheticEntry(&normalized)
	if normalized.ModelID != "deepseek-v3.2" || normalized.ProviderID != "synthetic" {
		t.Fatalf("empty provider becomes the gateway: %+v", normalized)
	}
	plain := UnifiedEntry{ModelID: "glm-5.3", ProviderID: "zhipu"}
	if normalizeSyntheticEntry(&plain) {
		t.Fatal("non-gateway traffic is untouched")
	}
}

func TestParseWorkbuddySQLite(t *testing.T) {
	root := t.TempDir()
	dbFile := filepath.Join(root, "workbuddy.db")
	db, err := sql.Open("sqlite", dbFile)
	if err != nil {
		t.Skip("sqlite unavailable")
	}
	for _, statement := range []string{
		`CREATE TABLE sessions (id TEXT PRIMARY KEY, model TEXT, cwd TEXT)`,
		`CREATE TABLE session_usage (session_id TEXT, used INTEGER, updated_at INTEGER)`,
		`INSERT INTO sessions VALUES ('wb-9','hunyuan-pro','/tmp/wb')`,
		`INSERT INTO session_usage VALUES ('wb-9', 5000, 1700000000)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	entry := mustEmit(t, "workbuddy", collectEntries(t, workbuddyParser{}, dbFile))
	if entry.Tokens.Input != 5000 || entry.ModelID != "hunyuan-pro" {
		t.Fatalf("aggregate row: %+v", entry)
	}
	// Seconds-scale updated_at normalizes to ms.
	if entry.TimestampMS != 1700000000000 {
		t.Fatalf("timestamp: %d", entry.TimestampMS)
	}
	if entry.DedupKey != "workbuddy:wb-9:"+strconv.FormatInt(1700000000, 10) {
		t.Fatalf("key: %s", entry.DedupKey)
	}
}

func TestParseCopilotVSCodeAppendShape(t *testing.T) {
	root := t.TempDir()
	chatDir := filepath.Join(root, "hash2", "chatSessions")
	if err := os.MkdirAll(chatDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Streaming shape: kind-0 opens a stub with r0, kind-2 APPENDS r1 — the
	// working set must keep both (upstream extend, not replace).
	body := "{\"kind\":0,\"v\":{\"requests\":[{\"timestamp\":1700000000000,\"modelId\":\"copilot/gpt-5.3\",\"promptTokens\":10,\"completionTokens\":2}]}}\n" +
		`{"kind":2,"k":["requests"],"v":[{"timestamp":1700000100000,"modelId":"copilot/claude-sonnet-4.6","promptTokens":20,"completionTokens":4}]}`
	file := filepath.Join(chatDir, "session-b.jsonl")
	if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	entries := collectEntries(t, copilotVSCodeParser{}, file)
	if len(entries) != 2 {
		t.Fatalf("appends must grow the set: %d", len(entries))
	}
	if entries[0].Tokens.Input != 10 || entries[1].Tokens.Input != 20 {
		t.Fatalf("buckets: %+v %+v", entries[0].Tokens, entries[1].Tokens)
	}
}

func TestParseMicodeLenientCache(t *testing.T) {
	root := t.TempDir()
	dbFile := filepath.Join(root, "mimo.db")
	db, err := sql.Open("sqlite", dbFile)
	if err != nil {
		t.Skip("sqlite unavailable")
	}
	for _, statement := range []string{
		`CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, data TEXT NOT NULL)`,
		`CREATE TABLE session (id TEXT PRIMARY KEY, directory TEXT)`,
		`INSERT INTO session VALUES ('s1', '/tmp/mimo')`,
		// MiMo rows may omit cache entirely: the lenient dialect keeps them.
		`INSERT INTO message VALUES ('r1', 's1', '{"role":"assistant","modelID":"mimo-v2","providerID":"unknown","tokens":{"input":80,"output":9},"time":{"created":1700000000}}')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	entry := mustEmit(t, "micode", collectEntries(t, micodeParser{}, dbFile))
	if entry.Tokens.Input != 80 || entry.Tokens.CacheRead != 0 {
		t.Fatalf("lenient cache zero-fill: %+v", entry.Tokens)
	}
	// Epoch seconds normalize to ms; rowid keys namespace per database.
	if entry.TimestampMS != 1700000000000 || entry.DedupKey != dbFile+":r1" {
		t.Fatalf("dialect rules: %+v", entry)
	}
}

func TestParseZcodeSQLite(t *testing.T) {
	root := t.TempDir()
	dbFile := filepath.Join(root, "db.sqlite")
	db, err := sql.Open("sqlite", dbFile)
	if err != nil {
		t.Skip("sqlite unavailable")
	}
	for _, statement := range []string{
		`CREATE TABLE session (id TEXT PRIMARY KEY, directory TEXT, path TEXT)`,
		`CREATE TABLE model_usage (id TEXT PRIMARY KEY, session_id TEXT, turn_id TEXT, model_id TEXT, started_at INTEGER, completed_at INTEGER, duration_ms INTEGER, input_tokens INTEGER, output_tokens INTEGER, reasoning_tokens INTEGER, cache_read_input_tokens INTEGER, cache_creation_input_tokens INTEGER, computed_total_tokens INTEGER, agent TEXT, mode TEXT)`,
		`INSERT INTO session VALUES ('z-1', '/tmp/zcode-ws', NULL)`,
		`INSERT INTO model_usage VALUES ('u-1', 'z-1', 't1', 'GLM-5.3', 1700000000000, 1700000010000, 10000, 100, 30, 10, 40, 5, 130, 'build', NULL)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	entry := mustEmit(t, "zcode-sqlite", collectEntries(t, zcodeParser{}, dbFile))
	if entry.Tokens.Input != 55 || entry.Tokens.CacheRead != 40 || entry.Tokens.Output != 20 || entry.Tokens.Reasoning != 10 {
		t.Fatalf("inclusive split: %+v", entry.Tokens)
	}
	if entry.DedupKey != "zcode-sqlite:u-1" || entry.Agent != "build" {
		t.Fatalf("identity: %+v", entry)
	}
	if entry.WorkspaceKey == "" {
		t.Fatalf("workspace from session.directory: %+v", entry)
	}
}

func TestParseKiroIDE(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "myproj", "sess_abc")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "session.json"), []byte(`{"id":"kiro-ide-1","model_id":"claude-sonnet-4-kiro","workspace_paths":["/tmp/ide-ws"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	messages := `{"timestamp":"2026-01-01T00:00:01Z","payload":{"type":"user","content":"hello ide"}}
{"timestamp":"2026-01-01T00:00:02Z","payload":{"type":"assistant","content":"answer"}}`
	if err := os.WriteFile(filepath.Join(dir, "messages.jsonl"), []byte(messages), 0o644); err != nil {
		t.Fatal(err)
	}
	entry := mustEmit(t, "kiro-ide", collectEntries(t, kiroParser{}, filepath.Join(dir, "session.json")))
	if entry.SessionID != "kiro-ide-1" || entry.ModelID != "claude-sonnet-4-kiro" {
		t.Fatalf("identity: %+v", entry)
	}
	if entry.Tokens.Input == 0 || entry.Tokens.Output == 0 {
		t.Fatalf("char estimate: %+v", entry.Tokens)
	}
	if entry.WorkspaceKey == "" {
		t.Fatalf("workspace from session.json: %+v", entry)
	}
}

func TestParseOctofriendSQLite(t *testing.T) {
	root := t.TempDir()
	dbFile := filepath.Join(root, "sqlite.db")
	db, err := sql.Open("sqlite", dbFile)
	if err != nil {
		t.Skip("sqlite unavailable")
	}
	for _, statement := range []string{
		`CREATE TABLE messages (id TEXT PRIMARY KEY, model TEXT, input_tokens INTEGER, output_tokens INTEGER, cache_read_tokens INTEGER, cache_write_tokens INTEGER, reasoning_tokens INTEGER, cost REAL, timestamp REAL, session_id TEXT, provider TEXT)`,
		`INSERT INTO messages VALUES ('o-1', 'accounts/fireworks/accounts/deepseek/models/deepseek-v4', 10, 2, 0, 0, 0, 0.01, 1700000000, 'oc-1', 'octofriend')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	entry := mustEmit(t, "synthetic", collectEntries(t, octofriendParser{}, dbFile))
	if entry.Client != "synthetic" || entry.ModelID != "deepseek-v4" {
		t.Fatalf("gateway normalization: %+v", entry)
	}
	if entry.DedupKey != "o-1" || entry.TimestampMS != 1700000000000 {
		t.Fatalf("meta: %+v", entry)
	}
}

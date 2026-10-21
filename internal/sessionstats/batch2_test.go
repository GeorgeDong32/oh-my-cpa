package sessionstats

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// writeFixture writes body under root/rel and returns the path.
func writeFixture(t *testing.T, root, rel, body string) string {
	t.Helper()
	file := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return file
}

func collectEntries(t *testing.T, parser Parser, file string) []UnifiedEntry {
	t.Helper()
	var entries []UnifiedEntry
	if err := parser.ParseFile(file, 1_700_000_000_000, func(entry UnifiedEntry) { entries = append(entries, entry) }); err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	return entries
}

func mustEmit(t *testing.T, name string, entries []UnifiedEntry) UnifiedEntry {
	t.Helper()
	if len(entries) == 0 {
		t.Fatalf("%s: no entries emitted", name)
	}
	return entries[0]
}

// The Batch 2 golden fixtures: one minimal but faithful record per client
// family, asserting the extraction each parser exists to do — model routing,
// additive bucket split, dedup key shape, timestamp anchoring.
func TestParsePiFamilyBatch2(t *testing.T) {
	root := t.TempDir()
	piBody := `{"type":"session","id":"s1","cwd":"/tmp/p"}
{"type":"message","id":"m1","timestamp":"2026-01-01T00:00:01Z","message":{"role":"assistant","model":"glm-5.3","provider":"zhipu","responseId":"resp-1","usage":{"input":10,"output":2,"totalTokens":12}}}`
	cases := []struct {
		name   string
		parser Parser
	}{
		{"omp", ompParser{}},
		{"senpi", senpiParser{}},
		{"craft-agent", craftAgentParser{}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			file := writeFixture(t, root, testCase.name+"/sess.jsonl", piBody)
			entry := mustEmit(t, testCase.name, collectEntries(t, testCase.parser, file))
			if entry.Client != testCase.name || entry.ModelID != "glm-5.3" || entry.Tokens.Input != 10 {
				t.Fatalf("entry: %+v", entry)
			}
		})
	}
	t.Run("kimchi-scoped-key", func(t *testing.T) {
		file := writeFixture(t, root, "kimchi/sess.jsonl", piBody)
		entry := mustEmit(t, "kimchi", collectEntries(t, kimchiParser{}, file))
		// The scoped namespace keys on the ENTRY id (upstream branch), not
		// the response id.
		if entry.DedupKey != "kimchi:s1:m1" {
			t.Fatalf("kimchi key must be session-scoped entry id: %s", entry.DedupKey)
		}
	})
	t.Run("craft-agent-custom-endpoint", func(t *testing.T) {
		body := `{"type":"session","id":"s2"}
{"type":"message","id":"m1","timestamp":"2026-01-01T00:00:01Z","message":{"role":"assistant","model":"claude-opus-5","provider":"custom-endpoint","responseId":"r9","usage":{"input":5,"output":1,"totalTokens":6}}}`
		file := writeFixture(t, root, "craft/.pi-sessions/s2.jsonl", body)
		entry := mustEmit(t, "craft-agent", collectEntries(t, craftAgentParser{}, file))
		if entry.ProviderID != "anthropic" {
			t.Fatalf("custom-endpoint must resolve to the model's provider: %s", entry.ProviderID)
		}
	})
}

func TestParseTencentBuddyFamily(t *testing.T) {
	root := t.TempDir()
	body := `{"type":"message","role":"assistant","status":"completed","sessionId":"s1","timestamp":1700000000000,"cwd":"/tmp/w","providerData":{"model":"hunyuan-pro","messageId":"pm1","usage":{"inputTokens":100,"outputTokens":20,"cacheReadInputTokens":40,"totalTokens":120}}}`
	file := writeFixture(t, root, "codebuddy/s1.jsonl", body)
	entry := mustEmit(t, "codebuddy", collectEntries(t, codebuddyParser{}, file))
	// total==input+output proves input is cache-inclusive → split the cache out.
	if entry.Tokens.Input != 60 || entry.Tokens.CacheRead != 40 || entry.Tokens.Output != 20 {
		t.Fatalf("additive split: %+v", entry.Tokens)
	}
	if entry.ProviderID != "tencent" || entry.DedupKey != "codebuddy:s1:pm1" {
		t.Fatalf("routing: %+v", entry)
	}
	logFile := writeFixture(t, root, "logs/ws/CodeBuddy.log",
		"2026-01-01T00:00:01.000Z [CraftInvokableAgent] [agent-7] Model prepared: hunyuan-pro (provider)\n"+
			"2026-01-01T00:00:02.000Z [AgentReporter] [agent-7] Agent execution successful with usage: {\"inputTokens\":10,\"outputTokens\":5}")
	logEntry := mustEmit(t, "extension-log", collectEntries(t, codebuddyParser{}, logFile))
	if logEntry.ModelID != "hunyuan-pro" || logEntry.SessionID != "agent-7" || logEntry.Tokens.Input != 10 {
		t.Fatalf("extension log: %+v", logEntry)
	}
	// Mirrored reporter lines a few ms apart carry the SAME second-granularity
	// dedup key; the store's key merge collapses them (a ms key would not).
	writeFixture(t, root, "logs/ws/CodeBuddy.log",
		"2026-01-01T00:00:02.005Z [AgentReporter] [agent-7] Agent execution successful with usage: {\"inputTokens\":10,\"outputTokens\":5}\n"+
			"2026-01-01T00:00:02.900Z [AgentReporter] [agent-7] Agent execution successful with usage: {\"inputTokens\":10,\"outputTokens\":5}")
	mirrors := collectEntries(t, codebuddyParser{}, logFile)
	if len(mirrors) != 2 || mirrors[0].DedupKey != mirrors[1].DedupKey {
		t.Fatalf("mirrors must share one second-granularity key: %s vs %s", mirrors[0].DedupKey, mirrors[1].DedupKey)
	}
}

func TestParseCherryStudio(t *testing.T) {
	root := t.TempDir()
	// A partial row (uuid only) followed by the complete row: both must emit,
	// and the partial row must ADOPT the complete row's key so the pair
	// collapses in the key merge; timestamps come from each record.
	body := `{"type":"assistant","uuid":"u1","timestamp":"2026-01-01T00:00:01Z","message":{"model":"claude-sonnet-4.6","usage":{"input_tokens":80,"output_tokens":10,"cache_read_input_tokens":20,"cache_creation_input_tokens":5}}}
{"type":"assistant","uuid":"u1","timestamp":"2026-01-01T00:00:02Z","message":{"id":"msg_1","model":"claude-sonnet-4.6","requestId":"req_1","usage":{"input_tokens":80,"output_tokens":10,"cache_read_input_tokens":20,"cache_creation_input_tokens":5}}}`
	file := writeFixture(t, root, "proj/sess.jsonl", body)
	entries := collectEntries(t, cherrystudioParser{}, file)
	if len(entries) != 2 {
		t.Fatalf("want partial + complete rows, got %d", len(entries))
	}
	// First emitted is the complete row (partials flush after EOF).
	complete := entries[0]
	partial := entries[1]
	if complete.DedupKey != "cherrystudio:msg_1:req_1" {
		t.Fatalf("complete key: %s", complete.DedupKey)
	}
	if partial.DedupKey != complete.DedupKey {
		t.Fatalf("partial row must adopt the complete row key: %s vs %s", partial.DedupKey, complete.DedupKey)
	}
	if partial.TimestampMS != 1767225601000 {
		t.Fatalf("per-record timestamp: %d", partial.TimestampMS)
	}
}

func TestParseGrok(t *testing.T) {
	root := t.TempDir()
	body := `{"timestamp":1700000000000,"params":{"_meta":{"eventId":"ev1"},"update":{"model":"grok-4.1-fast","usage":{"inputTokens":100,"outputTokens":30,"cachedReadTokens":40,"reasoningTokens":10,"totalTokens":130}}}}
{"timestamp":1700000060000,"params":{"_meta":{"eventId":"ev2"},"update":{"usage":{"inputTokens":50,"outputTokens":20,"totalTokens":70}}}}`
	file := writeFixture(t, root, "sess/updates.jsonl", body)
	entries := collectEntries(t, grokParser{}, file)
	if len(entries) != 2 {
		t.Fatalf("want 2 usage rows, got %d", len(entries))
	}
	first := entries[0]
	// total==130==100+30 → inclusive: input 100−40=60, output 30−10=20.
	if first.Tokens.Input != 60 || first.Tokens.CacheRead != 40 || first.Tokens.Output != 20 || first.Tokens.Reasoning != 10 {
		t.Fatalf("additive split: %+v", first.Tokens)
	}
	if first.DedupKey != "grok:sess:usage:0:ev1" {
		t.Fatalf("key: %s", first.DedupKey)
	}
}

func TestParseLMStudio(t *testing.T) {
	root := t.TempDir()
	body := "2026-01-01T00:00:01.000Z [CLK] Recv POST /v1/chat/completions {\n" +
		"  \"id\": \"chatcmpl-1\",\n" +
		"  \"model\": \"qwen/qwen3-coder-480b\",\n" +
		"  \"usage\": {\n" +
		"    \"prompt_tokens\": 90,\n" +
		"    \"completion_tokens\": 10,\n" +
		"    \"total_tokens\": 100,\n" +
		"    \"input_tokens_details\": { \"cached_tokens\": 30 }\n" +
		"  }\n" +
		"}"
	file := writeFixture(t, root, "2026-01/server.log", body)
	entry := mustEmit(t, "lmstudio", collectEntries(t, lmstudioParser{}, file))
	// OpenAI-style prompt_tokens is cache-inclusive: 90 − 30 = 60 uncached.
	if entry.Tokens.Input != 60 || entry.Tokens.CacheRead != 30 || entry.Tokens.Output != 10 {
		t.Fatalf("usage: %+v", entry.Tokens)
	}
	if entry.ModelID != "qwen/qwen3-coder-480b" {
		t.Fatalf("model from metadata: %s", entry.ModelID)
	}
}

func TestParseCodebuffFamily(t *testing.T) {
	root := t.TempDir()
	body := `[{"id":"cm1","variant":"ai","timestamp":"2026-01-01T00:00:01Z","metadata":{"model":"claude-sonnet-4.6","usage":{"input_tokens":30,"output_tokens":4,"cache_read_input_tokens":10}}}]`
	file := writeFixture(t, root, "proj/chats/2026-01-01T10-00-00.000Z/chat-messages.json", body)
	entry := mustEmit(t, "codebuff", collectEntries(t, codebuffParser{}, file))
	if entry.Tokens.Input != 30 || entry.Tokens.CacheRead != 10 || entry.DedupKey != "codebuff:cm1" {
		t.Fatalf("entry: %+v key=%s", entry, entry.DedupKey)
	}
}

func TestParseDSH(t *testing.T) {
	root := t.TempDir()
	body := `{"type":"session","id":"dsh-1","cwd":"/tmp/d","seedLength":2}
{"type":"assistant/message","seq":1,"time":1700000000000,"data":{"message":{"id":"seed"},"usage":{"inputTokens":10,"outputTokens":2}}}
{"type":"assistant/message","seq":5,"time":1700000100000,"data":{"message":{"id":"own"},"usage":{"inputTokens":20,"outputTokens":15,"reasoningTokens":5,"cacheReadTokens":8}}}`
	file := writeFixture(t, root, "dsh-1/transcript.jsonl", body)
	entries := collectEntries(t, dshParser{}, file)
	if len(entries) != 1 {
		t.Fatalf("seed rows before seedLength must not count, got %d", len(entries))
	}
	entry := entries[0]
	if entry.Tokens.Input != 20 || entry.Tokens.CacheRead != 8 || entry.Tokens.Output != 10 || entry.Tokens.Reasoning != 5 {
		t.Fatalf("reasoning splits out of output: %+v", entry.Tokens)
	}
	if entry.SessionID != "dsh-1" {
		t.Fatalf("session: %s", entry.SessionID)
	}
}

func TestParseMux(t *testing.T) {
	root := t.TempDir()
	body := `{"version":1,"byModel":{"anthropic:claude-sonnet-4.6":{"input":{"tokens":100,"cost_usd":0.1},"cached":{"tokens":50},"output":{"tokens":20}},"minimax/MiniMax-M3":{"input":{"tokens":5},"output":{"tokens":2}}},"lastRequest":{"timestamp":1700000000000}}`
	file := writeFixture(t, root, "ws1/session-usage.json", body)
	entries := collectEntries(t, muxParser{}, file)
	if len(entries) != 2 {
		t.Fatalf("want 2 model rows, got %d", len(entries))
	}
	if entries[0].ModelID != "claude-sonnet-4.6" || entries[0].ProviderID != "anthropic" {
		t.Fatalf("provider prefix split: %+v", entries[0])
	}
	if entries[0].Tokens.Input != 100 || entries[0].Tokens.CacheRead != 50 {
		t.Fatalf("tokens: %+v", entries[0].Tokens)
	}
}

func TestParseReasonix(t *testing.T) {
	root := t.TempDir()
	body := `{"ts":"2026-01-01T00:00:01Z","model":"deepseek/chat","prompt":100,"completion":30,"reasoning":10,"cache_hit":30,"cache_miss":70,"total":130,"requests":2}
{"ts":"2026-01-01T00:00:02Z","model":"deepseek/chat","prompt":1,"completion":1,"total":2,"requests":1,"turn":true}`
	file := writeFixture(t, root, "2026-01-01.jsonl", body)
	entries := collectEntries(t, reasonixParser{}, file)
	if len(entries) != 1 {
		t.Fatalf("turn records are excluded, got %d", len(entries))
	}
	entry := entries[0]
	if entry.Tokens.Input != 70 || entry.Tokens.CacheRead != 30 || entry.Tokens.Reasoning != 10 || entry.Tokens.Output != 20 {
		t.Fatalf("buckets: %+v", entry.Tokens)
	}
	if entry.MessageCount != 2 || entry.ProviderID != "deepseek" {
		t.Fatalf("meta: %+v", entry)
	}
}

func TestParseMcode(t *testing.T) {
	root := t.TempDir()
	body := `{"type":"message","turnId":"t1","message":{"role":"assistant","timestamp":1700000000000,"usage":{"inputTokens":50,"outputTokens":10,"cacheReadTokens":20}}}
{"type":"exec.result","turnId":"t1","sessionId":"mcode-s1","model":{"providerId":"minimax","modelId":"MiniMax-M3"}}`
	file := writeFixture(t, root, "stream.jsonl", body)
	entry := mustEmit(t, "mcode", collectEntries(t, mcodeParser{}, file))
	if entry.ModelID != "MiniMax-M3" || entry.ProviderID != "minimax" {
		t.Fatalf("model from exec.result only: %+v", entry)
	}
	if entry.Tokens.Input != 50 || entry.Tokens.CacheRead != 20 {
		t.Fatalf("usage: %+v", entry.Tokens)
	}
	// A stream without the model-bearing result row must not guess.
	firstLine := body
	if idx := strings.Index(firstLine, "\n"); idx >= 0 {
		firstLine = firstLine[:idx]
	}
	orphan := writeFixture(t, root, "partial.jsonl", firstLine)
	if entries := collectEntries(t, mcodeParser{}, orphan); len(entries) != 0 {
		t.Fatalf("partial stream must stay unpriced, got %d", len(entries))
	}
}

func TestParseGjc(t *testing.T) {
	root := t.TempDir()
	body := `{"type":"session","id":"gjc-1","cwd":"/tmp/g"}
{"type":"message","id":"gm1","message":{"role":"assistant","model":"glm-5.3","provider":"zhipu","timestamp":1700000000000,"usage":{"input_tokens":10,"output_tokens":2,"cache_read_input_tokens":3}}}`
	file := writeFixture(t, root, "proj/sess.jsonl", body)
	entry := mustEmit(t, "gjc", collectEntries(t, gjcParser{}, file))
	if entry.DedupKey != "gjc-1:gm1" || entry.Tokens.Input != 10 {
		t.Fatalf("entry: %+v", entry)
	}
}

func TestParseJunie(t *testing.T) {
	root := t.TempDir()
	body := `{"timestampMs":1700000000000,"event":{"agentEvent":{"kind":"LlmResponseMetadataEvent","modelUsage":[{"model":"claude-sonnet-4.6","inputTokens":100,"outputTokens":20,"cacheInputTokens":40,"time":1.5}]}}}`
	file := writeFixture(t, root, "session-260101-000000-abc/events.jsonl", body)
	entry := mustEmit(t, "junie", collectEntries(t, junieParser{}, file))
	if entry.Tokens.Input != 100 || entry.Tokens.CacheRead != 40 {
		t.Fatalf("buckets: %+v", entry.Tokens)
	}
	// End timestamp 1.7e12 minus 1500ms latency anchors the start.
	if entry.TimestampMS != 1700000000000-1500 {
		t.Fatalf("back-anchor: %d", entry.TimestampMS)
	}
}

func TestParseOpenCodeReview(t *testing.T) {
	root := t.TempDir()
	body := `{"type":"llm_response","model":"gpt-6-astra","timestamp":"2026-01-01T00:00:01Z","duration_ms":2000,"usage":{"input_tokens":50,"output_tokens":10}}`
	file := writeFixture(t, root, "sess.jsonl", body)
	entry := mustEmit(t, "opencodereview", collectEntries(t, opencodereviewParser{}, file))
	// 2026-01-01T00:00:01Z = 1767225601000; duration back-anchors the start.
	if entry.ProviderID != "openai" || entry.TimestampMS != 1767225601000-2000 {
		t.Fatalf("entry: %+v", entry)
	}
}

func TestParseJcode(t *testing.T) {
	root := t.TempDir()
	body := `{"id":"jcode-1","provider_key":"anthropic","model":"claude-sonnet-4.6","working_dir":"/tmp/j","messages":[{"id":"jm1","role":"assistant","timestamp":"2026-01-01T00:00:01Z","token_usage":{"input_tokens":20,"output_tokens":4,"cache_read_input_tokens":5,"reasoning_output_tokens":2}}]}`
	file := writeFixture(t, root, "session_1.json", body)
	entry := mustEmit(t, "jcode", collectEntries(t, jcodeParser{}, file))
	if entry.DedupKey != "jcode:jcode-1:jm1" || entry.Tokens.Input != 15 || entry.Tokens.Reasoning != 2 {
		t.Fatalf("ambiguous input splits the cache out: %+v", entry.Tokens)
	}
}

func TestParseAugment(t *testing.T) {
	root := t.TempDir()
	body := `{"sessionId":"aug-1","agent_state":{"modelId":"aug-default"},"turns":[{"finishedAt":"2026-01-01T00:00:01Z","completed":true,"sequenceId":"sq-9","exchange":{"model_id":"claude-sonnet-4.6","request_id":"req-1","response_nodes":[{"token_usage":{"input_tokens":40,"cache_read_input_tokens":10}},{"token_usage":{"input_tokens":60,"output_tokens":8,"cache_read_input_tokens":15}}]}}]}`
	file := writeFixture(t, root, "aug-1.json", body)
	entry := mustEmit(t, "augment", collectEntries(t, augmentParser{}, file))
	// Only the LAST response node's usage counts.
	if entry.Tokens.Input != 60 || entry.Tokens.CacheRead != 15 || entry.Tokens.Output != 8 {
		t.Fatalf("last-node usage: %+v", entry.Tokens)
	}
	if entry.DedupKey != "augment:aug-1:req:req-1" {
		t.Fatalf("key: %s", entry.DedupKey)
	}
}

func TestParseFx(t *testing.T) {
	root := t.TempDir()
	body := `{"session_id":"fx-1","snapshot":{"models":[{"model":"zai/glm-5.3","input_tokens":70,"output_tokens":10,"cache_read_tokens":30,"request_count":3}]}}`
	dir := filepath.Join(root, "fx-1")
	file := writeFixture(t, dir, "usage-v2.json", body)
	writeFixture(t, dir, "session.json", `{"workspace_root":"/tmp/fx","updated_at_ms":1700000000000}`)
	entry := mustEmit(t, "fx", collectEntries(t, fxParser{}, file))
	if entry.Tokens.Input != 70 || entry.Tokens.CacheRead != 30 || entry.MessageCount != 3 {
		t.Fatalf("entry: %+v", entry)
	}
	if entry.ProviderID != "zai" && entry.ProviderID != "zhipu" {
		t.Fatalf("provider: %s", entry.ProviderID)
	}
	if entry.WorkspaceKey == "" || entry.TimestampMS != 1700000000000 {
		t.Fatalf("meta: %+v", entry)
	}
}

func TestParseCommandCode(t *testing.T) {
	root := t.TempDir()
	body := `{"type":"session","id":"cc-1"}
{"type":"message","id":"u1","message":{"role":"user","content":"hello world please respond kindly"}}
{"type":"message","id":"a1","timestamp":"2026-01-01T00:00:01Z","model":"glm-5.3","message":{"role":"assistant","content":"hi"},"usage":{"inputTokens":100,"outputTokens":30,"cacheReadTokens":40}}`
	dir := filepath.Join(root, "myproj")
	file := writeFixture(t, dir, "cc-1.jsonl", body)
	entry := mustEmit(t, "commandcode", collectEntries(t, commandcodeParser{}, file))
	if entry.Tokens.Input != 60 || entry.Tokens.CacheRead != 40 || !entry.IsTurnStart {
		t.Fatalf("input splits the cache out: %+v", entry.Tokens)
	}
	if entry.DedupKey != "a1:1767225601000" {
		t.Fatalf("key: %s", entry.DedupKey)
	}
}

func TestParseOpenClawJSONL(t *testing.T) {
	root := t.TempDir()
	// Wire format is camelCase ids with `message.model` (upstream OpenClawEntry).
	body := `{"type":"model_change","modelId":"claude-sonnet-4.6","provider":"anthropic"}
{"type":"message","id":"oc1","message":{"role":"assistant","model":"claude-sonnet-4.6","timestamp":1700000000000,"usage":{"input":50,"output":12,"reasoningTokens":3,"cacheRead":10}}}`
	file := writeFixture(t, root, "sess-1.jsonl", body)
	entry := mustEmit(t, "openclaw", collectEntries(t, openclawParser{}, file))
	if entry.ModelID != "claude-sonnet-4.6" || entry.ProviderID != "anthropic" {
		t.Fatalf("model_change routing: %+v", entry)
	}
	if entry.Tokens.Input != 50 || entry.Tokens.Reasoning != 3 || entry.Tokens.Output != 9 {
		t.Fatalf("usage: %+v", entry.Tokens)
	}
	if entry.DedupKey != "openclaw:oc1" {
		t.Fatalf("key: %s", entry.DedupKey)
	}
}

func TestGooseSQLite(t *testing.T) {
	root := t.TempDir()
	gooseDB := filepath.Join(root, "sessions.db")
	db, err := sql.Open("sqlite", gooseDB)
	if err != nil {
		t.Skip("sqlite unavailable")
	}
	schema := `CREATE TABLE sessions (id TEXT PRIMARY KEY, model_config_json TEXT, provider_name TEXT, created_at TEXT, total_tokens INTEGER, input_tokens INTEGER, output_tokens INTEGER, accumulated_total_tokens INTEGER, accumulated_input_tokens INTEGER, accumulated_output_tokens INTEGER);`
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sessions VALUES ('g1', '{"model_name":"claude-sonnet-4.6"}', 'anthropic', '2026-01-01T00:00:00Z', 160, 100, 20, 160, 100, 20)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	entry := mustEmit(t, "goose", collectEntries(t, gooseParser{}, gooseDB))
	if entry.Tokens.Input != 100 || entry.Tokens.Output != 20 || entry.Tokens.Reasoning != 40 {
		t.Fatalf("gap→reasoning: %+v", entry.Tokens)
	}
	if entry.DedupKey != "goose:g1" {
		t.Fatalf("key: %s", entry.DedupKey)
	}
}

func TestParseGrokUnifiedLog(t *testing.T) {
	root := t.TempDir()
	body := `{"ts":"2026-01-01T00:00:01Z","msg":"model changed","sid":"sess-9","ctx":{"model":"grok-4.1-fast"},"pid":10}
{"ts":"2026-01-01T00:00:02Z","msg":"shell.turn.inference_done","sid":"sess-9","pid":10,"event_id":"eu1","ctx":{"prompt_tokens":100,"completion_tokens":30,"cached_prompt_tokens":40,"reasoning_tokens":10}}`
	file := writeFixture(t, root, "logs/unified.jsonl", body)
	entries := collectEntries(t, grokParser{}, file)
	entry := mustEmit(t, "grok-unified", entries)
	if entry.SessionID != "sess-9" || entry.ModelID != "grok-4.1-fast" {
		t.Fatalf("sid + model-change routing: %+v", entry)
	}
	// prompt is cache-inclusive, completion reasoning-inclusive: split both.
	if entry.Tokens.Input != 60 || entry.Tokens.CacheRead != 40 || entry.Tokens.Output != 20 || entry.Tokens.Reasoning != 10 {
		t.Fatalf("additive split: %+v", entry.Tokens)
	}
	if entry.DedupKey != "grok-unified:sess-9:id:eu1" {
		t.Fatalf("key: %s", entry.DedupKey)
	}
	// Discover reaches the unified log through the logs root.
	found, err := grokParser{}.Discover(filepath.Join(root, "logs"))
	if err != nil || len(found) != 1 || found[0] != file {
		t.Fatalf("dual-source discovery: %v %v", found, err)
	}
}

func TestParseOpenClawSQLite(t *testing.T) {
	root := t.TempDir()
	dbFile := filepath.Join(root, "agent", "openclaw-agent.sqlite")
	if err := os.MkdirAll(filepath.Dir(dbFile), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbFile)
	if err != nil {
		t.Skip("sqlite unavailable")
	}
	if _, err := db.Exec(`CREATE TABLE transcript_events (session_id TEXT, seq INTEGER, event_json TEXT, created_at INTEGER)`); err != nil {
		t.Fatal(err)
	}
	rows := [][]string{
		{`{"type":"model_change","modelId":"claude-sonnet-4.6","provider":"anthropic"}`, "1700000000000"},
		{`{"type":"message","id":"e1","message":{"role":"assistant","timestamp":1700000001000,"usage":{"input":50,"output":12,"reasoningTokens":3,"cacheRead":10}}}`, "1700000001000"},
	}
	for index, row := range rows {
		if _, err := db.Exec(`INSERT INTO transcript_events VALUES (?, ?, ?, ?)`, "oc-sess", index, row[0], row[1]); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	entries := collectEntries(t, openclawParser{}, dbFile)
	entry := mustEmit(t, "openclaw-sqlite", entries)
	if entry.SessionID != "oc-sess" || entry.ModelID != "claude-sonnet-4.6" || entry.ProviderID != "anthropic" {
		t.Fatalf("camel lane routing: %+v", entry)
	}
	if entry.Tokens.Input != 50 || entry.Tokens.Output != 9 || entry.Tokens.Reasoning != 3 {
		t.Fatalf("usage: %+v", entry.Tokens)
	}
}

func TestParseWorkbuddy(t *testing.T) {
	root := t.TempDir()
	body := `{"type":"function_call","status":"completed","sessionId":"wb-1","timestamp":1700000000000,"providerData":{"requestModelId":"hunyuan-code","traceId":"pt1","rawUsage":{"cachedMissTokens":40,"outputTokens":10,"totalTokens":50}}}`
	file := writeFixture(t, root, "wb/s1.jsonl", body)
	entry := mustEmit(t, "workbuddy", collectEntries(t, workbuddyParser{}, file))
	if entry.Client != "workbuddy" || entry.Tokens.Input != 40 || entry.Tokens.Output != 10 {
		t.Fatalf("extension-style miss input is exclusive: %+v", entry.Tokens)
	}
	if entry.DedupKey != "workbuddy:wb-1:pt1" {
		t.Fatalf("key: %s", entry.DedupKey)
	}
}

func TestParseFreebuff(t *testing.T) {
	root := t.TempDir()
	body := `[{"id":"fb1","variant":"ai","timestamp":"2026-01-01T00:00:01Z","content":"` + strings.Repeat("x", 400) + `","metadata":{"runState":{"sessionState":{"mainAgentState":{"agentType":"base2-free"}}}}}]`
	file := writeFixture(t, root, "proj/chats/2026-01-01T10-00-00.000Z/chat-messages.json", body)
	entry := mustEmit(t, "freebuff", collectEntries(t, freebuffParser{}, file))
	if entry.Tokens.Input != 100 {
		t.Fatalf("char estimate (~4 chars/token): %+v", entry.Tokens)
	}
}

func TestParseCrushSQLite(t *testing.T) {
	root := t.TempDir()
	registry := filepath.Join(root, "projects.json")
	projectDir := filepath.Join(root, "site")
	dataDir := filepath.Join(projectDir, "custom-data")
	dbFile := filepath.Join(dataDir, "crush.db")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(registry, []byte(`{"projects":[{"path":"`+projectDir+`","data_dir":"custom-data"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbFile)
	if err != nil {
		t.Skip("sqlite unavailable")
	}
	for _, statement := range []string{
		`CREATE TABLE sessions (id TEXT PRIMARY KEY, parent_session_id TEXT, cost REAL, created_at INTEGER, updated_at INTEGER, message_count INTEGER)`,
		`CREATE TABLE messages (session_id TEXT, role TEXT, created_at INTEGER)`,
		`INSERT INTO sessions VALUES ('c1', NULL, 0.5, 1700000000000, 1700000500000, 2)`,
		`INSERT INTO messages VALUES ('c1', 'assistant', 1700000100000)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	discovered, err := crushParser{}.Discover(registry)
	if err != nil || len(discovered) != 1 {
		t.Fatalf("registry discovery (data_dir relative to path): %v %v", discovered, err)
	}
	// The discovery path carries the registry-annotated workspace prefix.
	entry := mustEmit(t, "crush", collectEntries(t, crushParser{}, discovered[0]))
	if entry.ModelID != "session-total" || entry.MessageCount != 1 || entry.WorkspaceKey == "" {
		t.Fatalf("cost-only day bucket with workspace: %+v", entry)
	}
}

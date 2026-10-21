package sessionstats

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return file
}

func collect(parse func(string, int64, func(UnifiedEntry))) []UnifiedEntry {
	var entries []UnifiedEntry
	parse("/tmp/nonexistent-for-test.jsonl", 1_700_000_000_000, func(entry UnifiedEntry) {
		entries = append(entries, entry)
	})
	return entries
}

// Qwen: assistant lines with usageMetadata; cache-inclusive prompt counts
// stay as reported (the upstream keeps promptTokenCount as-is and reports
// cachedContentTokenCount separately — additive buckets).
func TestParseQwen(t *testing.T) {
	body := `{"type":"user","message":{}}
{"type":"assistant","model":"qwen3-max","timestamp":"2026-01-10T15:40:45.212Z","sessionId":"s-1","usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":50,"thoughtsTokenCount":10,"cachedContentTokenCount":20}}
{"type":"assistant","model":"qwen3-max","timestamp":"2026-01-10T15:41:45.212Z","sessionId":"s-1","usageMetadata":{"promptTokenCount":0,"candidatesTokenCount":0}}
`
	file := writeTemp(t, "chats/s1.jsonl", body)
	var entries []UnifiedEntry
	qwenParser{}.ParseFile(file, 9, func(entry UnifiedEntry) { entries = append(entries, entry) })
	if len(entries) != 1 {
		t.Fatalf("entries: %+v", entries)
	}
	entry := entries[0]
	if entry.Tokens.Input != 100 || entry.Tokens.Output != 50 || entry.Tokens.Reasoning != 10 || entry.Tokens.CacheRead != 20 {
		t.Fatalf("tokens: %+v", entry.Tokens)
	}
	if entry.SessionID != "s-1" || entry.ModelID != "qwen3-max" || entry.ProviderID != "qwen" {
		t.Fatalf("identity: %+v", entry)
	}
	if entry.DedupKey != "qwen:s-1:0" {
		t.Fatalf("dedup: %q", entry.DedupKey)
	}
	if entry.TimestampMS != 1_768_059_645_212 {
		t.Fatalf("timestamp: %d", entry.TimestampMS)
	}
}

// Codex: the state machine's arms — first event takes last (not the resumed
// total), the total-only defensive delta, and equal-total skips.
func TestParseCodexStateMachine(t *testing.T) {
	body := `{"timestamp":"2026-01-01T00:00:01Z","type":"session_meta","payload":{"id":"th-1","cwd":"/tmp/proj"}}
{"timestamp":"2026-01-01T00:00:02Z","type":"turn_context","payload":{"model_info":{"slug":"gpt-5.6"}}}
{"timestamp":"2026-01-01T00:00:03Z","type":"token_count","payload":{"info":{"total_token_usage":{"input_tokens":1000,"cached_input_tokens":900,"output_tokens":10,"total_tokens":1010},"last_token_usage":{"input_tokens":100,"cached_input_tokens":90,"output_tokens":10,"total_tokens":110}}}}
{"timestamp":"2026-01-01T00:00:04Z","type":"token_count","payload":{"info":{"total_token_usage":{"input_tokens":1000,"cached_input_tokens":900,"output_tokens":10,"total_tokens":1010},"last_token_usage":{"input_tokens":100,"cached_input_tokens":90,"output_tokens":10,"total_tokens":110}}}}
{"timestamp":"2026-01-01T00:00:05Z","type":"token_count","payload":{"info":{"total_token_usage":{"input_tokens":1100,"cached_input_tokens":990,"output_tokens":15,"total_tokens":1115},"last_token_usage":{"input_tokens":50,"cached_input_tokens":40,"output_tokens":5,"total_tokens":55}}}}
`
	file := writeTemp(t, "rollout-2026-01-01T00-00-00-th-1.jsonl", body)
	var entries []UnifiedEntry
	codexParser{}.ParseFile(file, 9, func(entry UnifiedEntry) { entries = append(entries, entry) })
	if len(entries) != 2 {
		t.Fatalf("entries: %+v", entries)
	}
	first, second := entries[0], entries[1]
	// First event: last only — the resumed history (900 cached) is not imported.
	if first.Tokens.Input != 100 || first.Tokens.Output != 10 || first.Tokens.CacheRead != 90 {
		t.Fatalf("first: %+v", first.Tokens)
	}
	// Second identical total is skipped; third takes its last increment.
	if second.Tokens.Input != 50 || second.Tokens.Output != 5 || second.Tokens.CacheRead != 40 {
		t.Fatalf("second: %+v", second.Tokens)
	}
	if first.ModelID != "gpt-5.6" || second.ModelID != "gpt-5.6" {
		t.Fatalf("model chain: %+v %+v", first, second)
	}
	if second.DedupKey == first.DedupKey {
		t.Fatalf("total-scoped keys must differ: %q", first.DedupKey)
	}
}

// Codex defensive path: total-only events delta against the baseline.
func TestParseCodexTotalOnlyDelta(t *testing.T) {
	body := `{"timestamp":"2026-01-01T00:00:01Z","type":"token_count","payload":{"info":{"total_token_usage":{"input_tokens":100,"output_tokens":10,"total_tokens":110},"last_token_usage":{"input_tokens":100,"output_tokens":10,"total_tokens":110}}}}
{"timestamp":"2026-01-01T00:00:02Z","type":"token_count","payload":{"info":{"total_token_usage":{"input_tokens":150,"output_tokens":20,"total_tokens":170}}}}
`
	file := writeTemp(t, "rollout-x.jsonl", body)
	var entries []UnifiedEntry
	codexParser{}.ParseFile(file, 9, func(entry UnifiedEntry) { entries = append(entries, entry) })
	if len(entries) != 2 {
		t.Fatalf("entries: %+v", entries)
	}
	if entries[1].Tokens.Input != 50 || entries[1].Tokens.Output != 10 {
		t.Fatalf("total-only delta: %+v", entries[1].Tokens)
	}
}

// Claude Code: streaming duplicates merge per-field max; synthetic models
// are suppressed; sidechain re-attribution to the parent session id.
func TestParseClaudeCode(t *testing.T) {
	body := `{"type":"assistant","sessionId":"parent-1","timestamp":"2026-01-01T00:00:01Z","message":{"id":"msg_1","model":"claude-opus-5.5","usage":{"input_tokens":100,"output_tokens":10,"cache_read_input_tokens":50,"cache_creation_input_tokens":5}}}
{"type":"assistant","sessionId":"parent-1","timestamp":"2026-01-01T00:00:01Z","message":{"id":"msg_1","model":"claude-opus-5.5","usage":{"input_tokens":120,"output_tokens":18,"cache_read_input_tokens":50,"cache_creation_input_tokens":5}}}
{"type":"assistant","sessionId":"parent-1","timestamp":"2026-01-01T00:00:02Z","message":{"id":"msg_2","model":"<synthetic>","usage":{"input_tokens":99,"output_tokens":9}}}
{"type":"assistant","isSidechain":true,"sessionId":"parent-1","agentId":"worker-7","timestamp":"2026-01-01T00:00:03Z","message":{"id":"msg_3","model":"claude-haiku-4.5","usage":{"input_tokens":40,"output_tokens":4}}}
`
	file := writeTemp(t, "parent-1.jsonl", body)
	var entries []UnifiedEntry
	claudeParser{}.ParseFile(file, 9, func(entry UnifiedEntry) { entries = append(entries, entry) })
	if len(entries) != 2 {
		t.Fatalf("entries: %+v", entries)
	}
	first := entries[0]
	if first.Tokens.Input != 120 || first.Tokens.Output != 18 || first.Tokens.CacheRead != 50 {
		t.Fatalf("streaming merge: %+v", first.Tokens)
	}
	sidechain := entries[1]
	if sidechain.SessionID != "parent-1" || sidechain.Agent != "worker-7" {
		t.Fatalf("sidechain: %+v", sidechain)
	}
	if sidechain.DedupKey != "claude:message:msg_3" {
		t.Fatalf("dedup: %q", sidechain.DedupKey)
	}
}

// Pi: responseModel (served) wins over the requested alias; the session
// header fixes workspace and session; responseId keys are cross-session.
func TestParsePi(t *testing.T) {
	body := `{"type":"session","id":"s-9","cwd":"/Users/x/proj"}
{"type":"message","id":"m-1","timestamp":"2026-01-01T00:00:01Z","message":{"role":"assistant","provider":"9router","model":"forge","responseId":"resp_1","responseModel":"glm-5.3","usage":{"input":100,"output":50,"cacheRead":10,"cacheWrite":5,"totalTokens":165}}}
{"type":"message","id":"m-2","timestamp":"2026-01-01T00:00:02Z","message":{"role":"assistant","provider":"9router","model":"glm-5.3","usage":{"input":7,"output":3,"cacheRead":0,"cacheWrite":0,"totalTokens":10}}}
`
	file := writeTemp(t, "s-9.jsonl", body)
	var entries []UnifiedEntry
	piParser{client: "pi"}.ParseFile(file, 9, func(entry UnifiedEntry) { entries = append(entries, entry) })
	if len(entries) != 2 {
		t.Fatalf("entries: %+v", entries)
	}
	if entries[0].ModelID != "glm-5.3" || entries[0].DedupKey != "pi:response:resp_1" {
		t.Fatalf("served model / dedup: %+v", entries[0])
	}
	if entries[1].ModelID != "glm-5.3" || entries[1].WorkspaceKey != "Users--x--proj" {
		t.Fatalf("requested model / workspace: %+v", entries[1])
	}
}

// Kimi legacy generation: StatusUpdate payloads with token_usage; the model
// comes from the sibling config.json; message_id keys the row.
func TestParseKimiLegacy(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "kimi", "sessions") // the sessions root; config sits beside it
	sessionDir := filepath.Join(dir, "g", "s")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "kimi", "config.json"), []byte(`{"model":"kimi-k2.5"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	body := `{"timestamp": 1768739496.5, "message": {"type": "TurnBegin", "payload": {}}}
{"timestamp": 1768739497.5, "message": {"type": "StatusUpdate", "payload": {"message_id": "mu-1", "token_usage": {"input_other": 200, "output": 40, "input_cache_read": 60, "input_cache_creation": 10}}}}
{"timestamp": 1768739498.5, "message": {"type": "StatusUpdate", "payload": {"message_id": "mu-1", "token_usage": {"input_other": 200, "output": 40, "input_cache_read": 60, "input_cache_creation": 10}}}}
`
	file := filepath.Join(sessionDir, "wire.jsonl")
	if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var entries []UnifiedEntry
	kimiParser{}.ParseFile(file, 9, func(entry UnifiedEntry) { entries = append(entries, entry) })
	if len(entries) != 2 {
		t.Fatalf("entries: %+v", entries)
	}
	entry := entries[0]
	if entry.Tokens.Input != 200 || entry.Tokens.Output != 40 || entry.Tokens.CacheRead != 60 || entry.Tokens.CacheWrite != 10 {
		t.Fatalf("tokens: %+v", entry.Tokens)
	}
	if entry.ModelID != "kimi-k2.5" || entry.SessionID != "s" {
		t.Fatalf("identity: %+v", entry)
	}
	if entry.TimestampMS != 1768739497500 {
		t.Fatalf("timestamp: %d", entry.TimestampMS)
	}
	if entries[0].DedupKey != entries[1].DedupKey {
		t.Fatalf("message_id keys must match: %q vs %q", entries[0].DedupKey, entries[1].DedupKey)
	}
}

package sessionstats

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The privacy contract (spec §1): parsers read usage fields only; message
// content never reaches any stored row or log line. A canary string planted
// in every content-bearing field of every parser family's fixture must not
// appear in any emitted entry — the repository stores exactly these entries,
// so the guarantee carries to the tables.
func TestContentCanaryNeverCrossesIntoEntries(t *testing.T) {
	const canary = "CANARY-CONTENT-8f3a"
	fixtures := map[string]struct {
		file   string
		body   string
		parser Parser
	}{
		"claude-code": {
			file: "proj/s1.jsonl",
			body: `{"type":"user","sessionId":"s1","message":{"content":"` + canary + `"}}
{"type":"assistant","sessionId":"s1","timestamp":"2026-01-01T00:00:01Z","message":{"id":"m1","model":"claude-opus-5.5","content":"` + canary + `","usage":{"input_tokens":10,"output_tokens":2}}}`,
			parser: claudeParser{},
		},
		"codex": {
			file: "rollout-s1.jsonl",
			body: `{"type":"event_msg","payload":{"type":"user_message","text":"` + canary + `"}}
{"timestamp":"2026-01-01T00:00:01Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":10,"output_tokens":2,"total_tokens":12},"last_token_usage":{"input_tokens":10,"output_tokens":2,"total_tokens":12}}}}`,
			parser: codexParser{},
		},
		"pi": {
			file: "s1.jsonl",
			body: `{"type":"session","id":"s1","cwd":"/tmp/p"}
{"type":"message","id":"m1","timestamp":"2026-01-01T00:00:01Z","message":{"role":"assistant","model":"glm-5.3","content":"` + canary + `","usage":{"input":10,"output":2,"totalTokens":12}}}`,
			parser: piParser{client: "pi"},
		},
		"qwen": {
			file:   "chats/s1.jsonl",
			body:   `{"type":"assistant","model":"qwen3-max","timestamp":"2026-01-01T00:00:01Z","sessionId":"s1","message":"` + canary + `","usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":2}}`,
			parser: qwenParser{},
		},
	}
	for name, fixture := range fixtures {
		t.Run(name, func(t *testing.T) {
			file := writeTemp(t, fixture.file, fixture.body)
			var entries []UnifiedEntry
			fixture.parser.ParseFile(file, 9, func(entry UnifiedEntry) { entries = append(entries, entry) })
			if len(entries) == 0 {
				t.Fatalf("fixture must emit entries: %s", name)
			}
			for _, entry := range entries {
				encoded := strings.Join([]string{entry.Client, entry.ModelID, entry.ProviderID, entry.SessionID,
					entry.WorkspaceKey, entry.WorkspaceLabel, entry.Agent, entry.DedupKey}, "\x00")
				if strings.Contains(encoded, canary) {
					t.Fatalf("content canary leaked into an entry field: %q", encoded)
				}
			}
		})
	}
}

// Fixture files land under testdata as sanitized snapshots for future golden
// tests; this guards that nothing written there carries content either.
func TestTestdataCarriesNoCanaries(t *testing.T) {
	const canary = "CANARY-CONTENT-8f3a"
	_ = filepath.WalkDir("testdata", func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr == nil && strings.Contains(string(body), canary) {
			t.Fatalf("canary in testdata: %s", path)
		}
		return nil
	})
}

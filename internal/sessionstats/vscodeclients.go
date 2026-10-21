package sessionstats

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// vscodeclients.go: the VS Code globalStorage task-log family. Cline is the
// upstream project Roo Code and Kilo Code forked from, so all three share the
// ui_messages.json "say/api_req_started" transcript format; Cline
// additionally has a CLI *.messages.json variant with its own manifest.

// taskLogParser parses the shared Cline/Roo/Kilo task-log format.
type taskLogParser struct {
	client string
	roots  func(home string) []string
}

func (p taskLogParser) Client() string { return p.client }

func (p taskLogParser) Roots(home string) []string { return p.roots(home) }

func (p taskLogParser) Discover(root string) ([]string, error) {
	return WalkFilesWithSuffix(root, "ui_messages.json")
}

// rooModelInfo is the per-message identity Cline 4.x writes on every entry;
// it is authoritative over the file-level <model> tag heuristic (#1321).
type rooModelInfo struct {
	ProviderID string `json:"providerId"`
	ModelID    string `json:"modelId"`
}

type rooUiEntry struct {
	Type      string          `json:"type"`
	Say       string          `json:"say"`
	Text      string          `json:"text"`
	TS        json.RawMessage `json:"ts"`
	ModelInfo *rooModelInfo   `json:"modelInfo"`
}

// parseRooKiloFile walks ui_messages.json: each "say/api_req_started" entry
// carries a JSON payload (in text) with cost and token buckets; model comes
// from the per-entry modelInfo, falling back to the LAST <model> tag in the
// sibling api_conversation_history.json.
func (p taskLogParser) ParseFile(file string, mtimeMS int64, emit func(UnifiedEntry)) error {
	body, ok := readFileLimited(file, maxFileBytes)
	if !ok {
		return nil
	}
	var entries []rooUiEntry
	if json.Unmarshal(body, &entries) != nil {
		return nil
	}
	session := dirBase(fileDir(file))
	fallbackModel, agent := readRooTaskMetadata(file)
	for _, entry := range entries {
		if entry.Type != "say" || entry.Say != "api_req_started" {
			continue
		}
		ts, ok := parseRooTimestamp(entry.TS)
		if !ok {
			continue
		}
		var payload struct {
			Cost        float64 `json:"cost"`
			TokensIn    int64   `json:"tokensIn"`
			TokensOut   int64   `json:"tokensOut"`
			CacheReads  int64   `json:"cacheReads"`
			CacheWrite  int64   `json:"cacheWrites"`
			APIProtocol string  `json:"apiProtocol"`
		}
		if json.Unmarshal([]byte(entry.Text), &payload) != nil {
			continue
		}
		model := fallbackModel
		if entry.ModelInfo != nil && strings.TrimSpace(entry.ModelInfo.ModelID) != "" {
			model = strings.TrimSpace(entry.ModelInfo.ModelID)
		}
		// A nested apiProtocol ("bedrock/anthropic") carries reseller routing
		// the bare providerId would flatten to its last segment.
		provider := strings.TrimSpace(payload.APIProtocol)
		if provider == "" && entry.ModelInfo != nil {
			provider = strings.TrimSpace(entry.ModelInfo.ProviderID)
		}
		if provider == "" {
			provider = "unknown"
		}
		emit(UnifiedEntry{
			Client: p.client, ModelID: model, ProviderID: provider,
			SessionID: session, TimestampMS: ts,
			Tokens: TokenBreakdown{
				Input: clampNonNeg(payload.TokensIn), Output: clampNonNeg(payload.TokensOut),
				CacheRead: clampNonNeg(payload.CacheReads), CacheWrite: clampNonNeg(payload.CacheWrite),
			},
			Agent: agent,
		})
	}
	return nil
}

func parseRooTimestamp(raw json.RawMessage) (int64, bool) {
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
	if json.Unmarshal(raw, &number) != nil || number <= 0 {
		return 0, false
	}
	// A recent epoch is ~1.7e12 in ms; at or below 1e12 it is seconds.
	if number < 1e12 {
		return int64(number * 1000), true
	}
	return int64(number), true
}

// readRooTaskMetadata extracts the LAST <environment_details> block's model
// and agent slug from the sibling api_conversation_history.json.
func readRooTaskMetadata(uiMessages string) (model string, agent string) {
	model = "unknown"
	history, ok := readFileLimited(fileDir(uiMessages)+"/api_conversation_history.json", maxFileBytes)
	if !ok {
		return model, ""
	}
	content := string(history)
	const envStart, envEnd = "<environment_details>", "</environment_details>"
	offset, lastSlug, lastName := 0, "", ""
	for {
		start := strings.Index(content[offset:], envStart)
		if start < 0 {
			break
		}
		start += offset + len(envStart)
		end := strings.Index(content[start:], envEnd)
		if end < 0 {
			break
		}
		block := content[start : start+end]
		if value := extractXMLEnvironmentTag(block, "model"); value != "" {
			model = value
		}
		if value := extractXMLEnvironmentTag(block, "slug"); value != "" {
			lastSlug = value
		}
		if value := extractXMLEnvironmentTag(block, "name"); value != "" {
			lastName = value
		}
		offset = start + end
	}
	// Upstream keeps the LAST slug across the file, falling back to the last
	// name only when no slug ever appeared.
	if lastSlug != "" {
		return model, lastSlug
	}
	return model, lastName
}

func extractXMLEnvironmentTag(block, tag string) string {
	open, close_ := "<"+tag+">", "</"+tag+">"
	start := strings.Index(block, open)
	if start < 0 {
		return ""
	}
	start += len(open)
	end := strings.Index(block[start:], close_)
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(block[start : start+end])
}

// clineParser: the VS Code task logs plus the CLI *.messages.json variant.
type clineParser struct {
	taskLogParser
}

func (clineParser) Client() string { return "cline" }

func (clineParser) Roots(home string) []string {
	return []string{
		home + "/.config/Code/User/globalStorage/saoudrizwan.claude-dev/tasks",
		home + "/.config/Code - OSS/User/globalStorage/saoudrizwan.claude-dev/tasks",
		home + "/Library/Application Support/Code/User/globalStorage/saoudrizwan.claude-dev/tasks",
		// CLI sessions: the home fallback is unconditional upstream; env
		// overrides only replace it when set (not ported, single-user local).
		home + "/.cline/data/sessions",
		home + "/.vscode-server/data/User/globalStorage/saoudrizwan.claude-dev/tasks",
	}
}

func (clineParser) Discover(root string) ([]string, error) {
	tasks, err := WalkFilesWithSuffix(root, "ui_messages.json")
	if err != nil {
		return nil, err
	}
	messages, err := WalkFilesWithSuffix(root, ".messages.json")
	if err == nil {
		tasks = append(tasks, messages...)
	}
	return tasks, nil
}

func (p clineParser) ParseFile(file string, mtimeMS int64, emit func(UnifiedEntry)) error {
	if strings.HasSuffix(filepath.Base(file), ".messages.json") {
		parseClineCLIFile(file, mtimeMS, emit)
		return nil
	}
	return taskLogParser{client: "cline", roots: clineParser{}.Roots}.ParseFile(file, mtimeMS, emit)
}

// parseClineCLIFile reads the CLI's *.messages.json with its sibling manifest
// (*.json) carrying session identity, model routing and workspace root.
func parseClineCLIFile(file string, mtimeMS int64, emit func(UnifiedEntry)) error {
	body, ok := readFileLimited(file, maxFileBytes)
	if !ok {
		return nil
	}
	var messages struct {
		SessionID string `json:"sessionId"`
		Agent     string `json:"agent"`
		Messages  []struct {
			ID      *string         `json:"id"`
			Role    string          `json:"role"`
			TS      json.RawMessage `json:"ts"`
			Content []struct {
				Type string `json:"type"`
			} `json:"content"`
			ModelInfo *struct {
				ID       string `json:"id"`
				Provider string `json:"provider"`
			} `json:"modelInfo"`
			Metrics *struct {
				InputTokens      json.RawMessage `json:"inputTokens"`
				OutputTokens     json.RawMessage `json:"outputTokens"`
				CacheReadTokens  json.RawMessage `json:"cacheReadTokens"`
				CacheWriteTokens json.RawMessage `json:"cacheWriteTokens"`
				Cost             json.RawMessage `json:"cost"`
			} `json:"metrics"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &messages) != nil {
		return nil
	}
	manifest := readClineCLIManifest(file)
	session := strings.TrimSpace(messages.SessionID)
	if session == "" {
		session = manifest.SessionID
	}
	if session == "" {
		session = strings.TrimSuffix(filepath.Base(file), ".messages.json")
	}
	wsKey, wsLabel := "", ""
	wsRoot := manifest.WorkspaceRoot
	if wsRoot == "" {
		wsRoot = manifest.CWD
	}
	if key := NormalizeWorkspaceKey(wsRoot); key != "" {
		wsKey, wsLabel = key, WorkspaceLabelFromKey(key)
	}
	currentModel := strings.TrimSpace(manifest.Model)
	if currentModel == "" {
		currentModel = "unknown"
	}
	currentProvider := strings.TrimSpace(manifest.Provider)
	if currentProvider == "" {
		currentProvider = "unknown"
	}
	pendingTurnStart := false
	assistantIndex := 0
	for _, entry := range messages.Messages {
		if entry.Role == "user" {
			if isHumanUserPrompt(entry.Content) {
				pendingTurnStart = true
			}
			continue
		}
		if entry.Role != "assistant" {
			continue
		}
		if entry.ModelInfo != nil {
			if strings.TrimSpace(entry.ModelInfo.ID) != "" {
				currentModel = strings.TrimSpace(entry.ModelInfo.ID)
			}
			if strings.TrimSpace(entry.ModelInfo.Provider) != "" {
				currentProvider = strings.TrimSpace(entry.ModelInfo.Provider)
			}
		}
		if entry.Metrics == nil {
			continue
		}
		rawInput := rawI64(entry.Metrics.InputTokens)
		output := rawI64(entry.Metrics.OutputTokens)
		cacheRead := rawI64(entry.Metrics.CacheReadTokens)
		cacheWrite := rawI64(entry.Metrics.CacheWriteTokens)
		input := clampNonNeg(rawInput - cacheRead - cacheWrite)
		cost := rawF64(entry.Metrics.Cost)
		hasCost := cost >= 0 && len(strings.TrimSpace(string(entry.Metrics.Cost))) > 0
		tokens := TokenBreakdown{Input: input, Output: output, CacheRead: cacheRead, CacheWrite: cacheWrite}
		if tokens.Total() == 0 && !hasCost {
			continue
		}
		ts := mtimeMS
		if parsed, ok := parseRooTimestamp(entry.TS); ok {
			ts = parsed
		}
		key := "cline-cli:" + session + ":" + strconv.Itoa(assistantIndex)
		if entry.ID != nil && strings.TrimSpace(*entry.ID) != "" {
			key = "cline-cli:" + session + ":" + strings.TrimSpace(*entry.ID)
		}
		emit(UnifiedEntry{
			Client: "cline", ModelID: currentModel, ProviderID: currentProvider,
			SessionID: session, WorkspaceKey: wsKey, WorkspaceLabel: wsLabel,
			TimestampMS: ts, Tokens: tokens, IsTurnStart: pendingTurnStart,
			Agent: strings.TrimSpace(messages.Agent), DedupKey: key,
		})
		assistantIndex++
		pendingTurnStart = false
	}
	return nil
}

// rawI64 accepts a JSON number or numeric string (the CLI writes both).
func rawI64(raw json.RawMessage) int64 {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return 0
	}
	if trimmed[0] == '"' {
		var text string
		if json.Unmarshal(raw, &text) == nil {
			var parsed int64
			if json.Unmarshal([]byte(text), &parsed) == nil {
				return clampNonNeg(parsed)
			}
			return 0
		}
		return 0
	}
	var parsed int64
	if json.Unmarshal(raw, &parsed) == nil {
		return clampNonNeg(parsed)
	}
	return 0
}

func rawF64(raw json.RawMessage) float64 {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return -1
	}
	var parsed float64
	if json.Unmarshal(raw, &parsed) == nil && parsed >= 0 {
		return parsed
	}
	return -1
}

func isHumanUserPrompt(content []struct {
	Type string `json:"type"`
}) bool {
	hasText := false
	for _, block := range content {
		switch block.Type {
		case "tool_result":
			return false
		case "text":
			hasText = true
		}
	}
	return hasText
}

type clineCLIManifest struct {
	SessionID     string `json:"session_id"`
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	CWD           string `json:"cwd"`
	WorkspaceRoot string `json:"workspace_root"`
}

func readClineCLIManifest(messagesFile string) clineCLIManifest {
	stem := strings.TrimSuffix(filepath.Base(messagesFile), ".messages.json")
	manifestPath := fileDir(messagesFile) + "/" + stem + ".json"
	body, ok := readFileLimited(manifestPath, maxFileBytes)
	if !ok {
		return clineCLIManifest{}
	}
	var manifest clineCLIManifest
	json.Unmarshal(body, &manifest)
	return manifest
}

// roocodeParser / kilocodeParser: the same task-log format under their own
// globalStorage directories.
func roocodeRoots(home string) []string {
	return []string{
		home + "/.config/Code/User/globalStorage/rooveterinaryinc.roo-cline/tasks",
		home + "/Library/Application Support/Code/User/globalStorage/rooveterinaryinc.roo-cline/tasks",
	}
}

func kilocodeRoots(home string) []string {
	return []string{
		home + "/.config/Code/User/globalStorage/kilocode.kilo-code/tasks",
		home + "/Library/Application Support/Code/User/globalStorage/kilocode.kilo-code/tasks",
	}
}

type roocodeParser struct{}

func (roocodeParser) Client() string             { return "roocode" }
func (roocodeParser) Roots(home string) []string { return roocodeRoots(home) }
func (roocodeParser) Discover(root string) ([]string, error) {
	return WalkFilesWithSuffix(root, "ui_messages.json")
}
func (roocodeParser) ParseFile(file string, mtimeMS int64, emit func(UnifiedEntry)) error {
	return taskLogParser{client: "roocode", roots: roocodeRoots}.ParseFile(file, mtimeMS, emit)
}

type kilocodeParser struct{}

func (kilocodeParser) Client() string             { return "kilocode" }
func (kilocodeParser) Roots(home string) []string { return kilocodeRoots(home) }
func (kilocodeParser) Discover(root string) ([]string, error) {
	return WalkFilesWithSuffix(root, "ui_messages.json")
}
func (kilocodeParser) ParseFile(file string, mtimeMS int64, emit func(UnifiedEntry)) error {
	return taskLogParser{client: "kilocode", roots: kilocodeRoots}.ParseFile(file, mtimeMS, emit)
}

// copilotVSCodeParser reads Copilot Chat's projected session logs under
// VS Code workspaceStorage/<hash>/chatSessions/<session>.jsonl. The log is a
// row-log projection: kind-0 rows carry the requests array, kind-1 rows
// patch a path inside one request, kind-2 rows delete entries — the requests
// are reconstructed before any usage is read.
type copilotVSCodeParser struct{}

func (copilotVSCodeParser) Client() string { return "copilot-vscode" }

func copilotVSCodeWorkspaceStorage(home string) []string {
	return []string{
		home + "/Library/Application Support/Code/User/workspaceStorage",
		home + "/.config/Code/User/workspaceStorage",
		home + "/AppData/Roaming/Code/User/workspaceStorage",
	}
}

func (copilotVSCodeParser) Roots(home string) []string {
	return copilotVSCodeWorkspaceStorage(home)
}

func (copilotVSCodeParser) Discover(root string) ([]string, error) {
	// <root>/<hash>/chatSessions/*.jsonl; workspace.json sits beside the
	// chatSessions dir.
	var files []string
	hashes, err := os.ReadDir(root)
	if err != nil {
		return nil, nil
	}
	for _, hash := range hashes {
		chatDir := filepath.Join(root, hash.Name(), "chatSessions")
		entries, err := os.ReadDir(chatDir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".jsonl") {
				files = append(files, filepath.Join(chatDir, entry.Name()))
			}
		}
	}
	return files, nil
}

func (copilotVSCodeParser) ParseFile(file string, mtimeMS int64, emit func(UnifiedEntry)) error {
	session := strings.TrimSuffix(filepath.Base(file), ".jsonl")
	wsKey, wsLabel := copilotWorkspaceFor(file)
	requests := []map[string]any{}
	forEachJSONLine(file, mtimeMS, func(raw []byte) {
		var row struct {
			Kind int               `json:"kind"`
			V    json.RawMessage   `json:"v"`
			K    []json.RawMessage `json:"k"`
		}
		if json.Unmarshal(raw, &row) != nil {
			return
		}
		switch row.Kind {
		case 0:
			// Full state: v.requests APPENDS to the working set (upstream
			// extend) — streaming sessions open with a stub and grow.
			var value struct {
				Requests []map[string]any `json:"requests"`
			}
			if json.Unmarshal(row.V, &value) == nil {
				requests = append(requests, value.Requests...)
			}
		case 1:
			// Patch: k = ["requests", <index>, ...path], v = new value.
			if len(row.K) < 2 {
				return
			}
			if key, _ := parseJSONString(row.K[0]); key != "requests" {
				return
			}
			index, ok := parseJSONIndex(row.K[1])
			if !ok || index >= len(requests) || index > 4096 {
				return
			}
			applyCopilotPatch(&requests[index], row.K[2:], row.V)
		case 2:
			// Top-level ["requests"] appends grow the set (upstream extend);
			// deeper delete paths are not projected onto usage fields.
			if len(row.K) == 1 {
				if key, _ := parseJSONString(row.K[0]); key == "requests" {
					var appended []map[string]any
					if err := json.Unmarshal(row.V, &appended); err == nil {
						requests = append(requests, appended...)
					}
				}
			}
		}
	})
	for _, request := range requests {
		emitCopilotRequest(request, session, wsKey, wsLabel, emit)
	}
	return nil
}

func copilotWorkspaceFor(file string) (string, string) {
	hashDir := fileDir(fileDir(file))
	body, ok := readFileLimited(hashDir+"/workspace.json", maxFileBytes)
	if !ok {
		return "", ""
	}
	var value struct {
		Folder    string `json:"folder"`
		Workspace string `json:"workspace"`
	}
	if json.Unmarshal(body, &value) != nil {
		return "", ""
	}
	folder := value.Folder
	if folder == "" {
		folder = value.Workspace
	}
	folder = strings.TrimPrefix(folder, "file://")
	if key := NormalizeWorkspaceKey(folder); key != "" {
		return key, WorkspaceLabelFromKey(key)
	}
	return "", ""
}

func parseJSONString(raw json.RawMessage) (string, bool) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, true
	}
	return "", false
}

func parseJSONIndex(raw json.RawMessage) (int, bool) {
	var index int
	if json.Unmarshal(raw, &index) == nil && index >= 0 {
		return index, true
	}
	return 0, false
}

// applyCopilotPatch walks a path of string/number keys into a request map,
// creating intermediate containers, and writes the value.
func applyCopilotPatch(target *map[string]any, path []json.RawMessage, value json.RawMessage) {
	if len(path) == 0 {
		var decoded any
		if json.Unmarshal(value, &decoded) == nil {
			// A non-object root replacement cannot carry usage fields; skip
			// rather than panic on the assertion.
			if mapping, ok := decoded.(map[string]any); ok {
				*target = mapping
			}
		}
		return
	}
	current := *target
	if current == nil {
		current = map[string]any{}
		*target = current
	}
	// Resolve every parent; the final key writes.
	container := current
	for i := 0; i < len(path)-1; i++ {
		if key, ok := parseJSONString(path[i]); ok {
			next, exists := container[key]
			if !exists {
				child := map[string]any{}
				container[key] = child
				container = child
				continue
			}
			if child, ok := next.(map[string]any); ok {
				container = child
			} else {
				child := map[string]any{}
				container[key] = child
				container = child
			}
		}
	}
	var decoded any
	if json.Unmarshal(value, &decoded) != nil {
		return
	}
	if key, ok := parseJSONString(path[len(path)-1]); ok {
		container[key] = decoded
	}
}

func emitCopilotRequest(request map[string]any, session, wsKey, wsLabel string, emit func(UnifiedEntry)) {
	number := func(path ...string) int64 {
		current := any(request)
		for _, segment := range path {
			mapping, ok := current.(map[string]any)
			if !ok {
				return 0
			}
			current, ok = mapping[segment]
			if !ok {
				return 0
			}
		}
		if floated, ok := current.(float64); ok {
			return clampNonNeg(int64(floated))
		}
		return 0
	}
	text := func(path ...string) string {
		current := any(request)
		for _, segment := range path {
			mapping, ok := current.(map[string]any)
			if !ok {
				return ""
			}
			current, ok = mapping[segment]
			if !ok {
				return ""
			}
		}
		if value, ok := current.(string); ok {
			return value
		}
		return ""
	}
	prompt := number("promptTokens")
	if prompt == 0 {
		prompt = number("result", "metadata", "promptTokens")
	}
	completion := number("completionTokens")
	if completion == 0 {
		completion = number("result", "metadata", "outputTokens")
	}
	timestamp := number("timestamp")
	if timestamp == 0 {
		return
	}
	resolvedModel := strings.TrimSpace(text("result", "metadata", "resolvedModel"))
	modelIDRaw := strings.TrimSpace(text("modelId"))
	model := resolvedModel
	isCopilot := resolvedModel != "" || strings.HasPrefix(modelIDRaw, "copilot/")
	if model == "" {
		model = strings.TrimPrefix(modelIDRaw, "copilot/")
	}
	if model == "" {
		model = "auto"
	}
	if !isCopilot {
		return
	}
	// toolCallRounds[*].thinking.tokens sums into reasoning.
	reasoning := int64(0)
	if rounds, ok := lookupPath(request, "result", "metadata", "toolCallRounds").([]any); ok {
		for _, round := range rounds {
			if mapping, ok := round.(map[string]any); ok {
				if thinking, ok := mapping["thinking"].(map[string]any); ok {
					if floated, ok := thinking["tokens"].(float64); ok && floated > 0 {
						reasoning += int64(floated)
					}
				}
			}
		}
	}
	tokens := TokenBreakdown{Input: prompt, Output: completion, Reasoning: clampNonNeg(reasoning)}
	if tokens.Total() == 0 {
		return
	}
	emit(UnifiedEntry{
		Client: "copilot-vscode", ModelID: model,
		ProviderID: providerForModelOr(model, "github-copilot"),
		SessionID:  session, WorkspaceKey: wsKey, WorkspaceLabel: wsLabel,
		TimestampMS: timestamp, Tokens: tokens,
		DedupKey: "copilot-vscode:" + session + ":" + strconv.FormatInt(timestamp, 10),
	})
}

func lookupPath(value map[string]any, path ...string) any {
	current := any(value)
	for _, segment := range path {
		mapping, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current, ok = mapping[segment]
		if !ok {
			return nil
		}
	}
	return current
}

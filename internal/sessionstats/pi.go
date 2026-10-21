package sessionstats

// Pi (badlogic/pi-mono) and descendants: ~/.pi/agent/sessions/<ws>/*.jsonl
// (sessions/pi.rs). Assistant message records carry usage; responseModel is
// the served model (preferred over the requested alias); fork copies dedup
// cross-session via responseId, with an id+content-hash fallback key.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

type piParser struct {
	client string
	roots  func(home string) []string
	// scopedKeys keeps Kimchi's historical session-scoped dedup namespace
	// (`client:session:message`), which predates the cross-session key the
	// standard pi lane adopted (upstream #1306).
	scopedKeys bool
	// rewriteCustomEndpoint moves Craft Agent's user-defined endpoint
	// provider ("custom-endpoint") to the provider the model name implies.
	rewriteCustomEndpoint bool
}

func (p piParser) Client() string { return p.client }

func (p piParser) Roots(home string) []string { return p.roots(home) }

func (p piParser) Discover(root string) ([]string, error) { return WalkJSONLFiles(root) }

type piSessionHeader struct {
	Type     string `json:"type"`
	ID       string `json:"id"`
	CWD      string `json:"cwd"`
	RLMDepth *int   `json:"rlmDepth"`
}

type piSessionEntry struct {
	Type      string     `json:"type"`
	ID        *string    `json:"id"`
	Timestamp string     `json:"timestamp"`
	Message   *piMessage `json:"message"`
	Name      *string    `json:"name"` // session_info records
}

type piMessage struct {
	Role          string   `json:"role"`
	Provider      string   `json:"provider"`
	Model         string   `json:"model"`
	ResponseID    *string  `json:"responseId"`
	ResponseModel string   `json:"responseModel"`
	Usage         *piUsage `json:"usage"`
}

type piUsage struct {
	Input      *int64 `json:"input"`
	Output     *int64 `json:"output"`
	CacheRead  *int64 `json:"cacheRead"`
	CacheWrite *int64 `json:"cacheWrite"`
	Total      *int64 `json:"totalTokens"`
}

func (u *piUsage) breakdown() (TokenBreakdown, bool) {
	if u == nil {
		return TokenBreakdown{}, false
	}
	tokens := TokenBreakdown{
		Input: nonNeg(u.Input), Output: nonNeg(u.Output),
		CacheRead: nonNeg(u.CacheRead), CacheWrite: nonNeg(u.CacheWrite),
	}
	if tokens.Total() == 0 {
		return TokenBreakdown{}, false
	}
	return tokens, true
}

func (p piParser) ParseFile(file string, mtimeMS int64, emit func(UnifiedEntry)) error {
	body, ok := readFileLimited(file, maxFileBytes)
	if !ok {
		return nil
	}
	_, wsLabel := WorkspaceAfterMarker(file, "sessions", "")
	var sessionID string
	var wsKey, agent string
	isSubagent := false
	dec := json.NewDecoder(strings.NewReader(string(body)))
	for dec.More() {
		start := dec.InputOffset()
		var probe struct {
			Type string `json:"type"`
		}
		if dec.Decode(&probe) != nil {
			continue
		}
		raw := body[start:dec.InputOffset()]
		if sessionID == "" {
			if probe.Type != "session" {
				continue // pre-header metadata or a foreign line
			}
			var header piSessionHeader
			if json.Unmarshal(raw, &header) != nil || header.ID == "" {
				continue
			}
			sessionID = header.ID
			wsKey = NormalizeWorkspaceKey(header.CWD)
			isSubagent = header.RLMDepth != nil && *header.RLMDepth > 0
			continue
		}
		if probe.Type == "session_info" {
			var info piSessionEntry
			if json.Unmarshal(raw, &info) == nil && info.Name != nil && isSubagent {
				// A subagent session names its worker; requester sessions keep
				// the main thread's empty agent.
				name := strings.TrimPrefix(strings.TrimSpace(*info.Name), "subagent-")
				if name != "" {
					agent = name
				}
			}
			continue
		}
		if probe.Type != "message" {
			continue
		}
		var entry piSessionEntry
		if json.Unmarshal(raw, &entry) != nil || entry.Message == nil || entry.Message.Role != "assistant" {
			continue
		}
		message := entry.Message
		tokens, hasTokens := message.Usage.breakdown()
		if !hasTokens {
			continue
		}
		// responseModel served the request; model is the (possibly proxied)
		// alias the client asked for.
		model := strings.TrimSpace(message.ResponseModel)
		if model == "" {
			model = strings.TrimSpace(message.Model)
		}
		if model == "" {
			continue
		}
		provider := strings.TrimSpace(message.Provider)
		if provider == "" {
			provider = "pi"
		} else if p.rewriteCustomEndpoint && provider == "custom-endpoint" {
			// A user-pointed endpoint is not a vendor any pricing source knows;
			// attribute to the model's own provider when inferable (tokens#48).
			if inferred := providerForModel(model); inferred != "" {
				provider = inferred
			}
		}
		ts := mtimeMS
		if parsed, valid := ParseRFC3339MS(entry.Timestamp); valid {
			ts = parsed
		}
		key := p.client + ":response:"
		if message.ResponseID != nil && strings.TrimSpace(*message.ResponseID) != "" {
			key += strings.TrimSpace(*message.ResponseID)
		} else if entry.ID != nil && strings.TrimSpace(*entry.ID) != "" {
			digest := sha256.Sum256(raw)
			key = p.client + ":message:" + strings.TrimSpace(*entry.ID) + ":" + hex.EncodeToString(digest[:8])
		} else {
			digest := sha256.Sum256(raw)
			key = p.client + ":record:" + hex.EncodeToString(digest[:12])
		}
		if p.scopedKeys {
			// Kimchi's historical namespace keys on the ENTRY id (the
			// upstream scoped branch), not the response id.
			if entry.ID != nil && strings.TrimSpace(*entry.ID) != "" {
				key = p.client + ":" + sessionID + ":" + strings.TrimSpace(*entry.ID)
			}
		}
		emit(UnifiedEntry{
			Client: p.client, ModelID: model, ProviderID: provider, SessionID: sessionID,
			WorkspaceKey: wsKey, WorkspaceLabel: wsLabel, TimestampMS: ts, Tokens: tokens,
			Agent: agent, DedupKey: key,
		})
	}
	return nil
}

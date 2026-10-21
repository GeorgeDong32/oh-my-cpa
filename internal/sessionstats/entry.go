package sessionstats

// Package-level helpers for the parsers: time, workspace identity, and the
// 15-minute bucket grid shared with storage (migration 045).

import (
	"net/url"
	"path"
	"strings"
	"time"
)

// TokenBreakdown mirrors the upstream five mutually exclusive, additive
// buckets. Reasoning is NOT inside output.
type TokenBreakdown struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
	Reasoning  int64 `json:"reasoning"`
}

// Total is the sum of every bucket.
func (t TokenBreakdown) Total() int64 {
	return t.Input + t.Output + t.CacheRead + t.CacheWrite + t.Reasoning
}

// UnifiedEntry is the port of the upstream UnifiedMessage with its cost
// fields dropped: costs are computed at read time from the price book.
type UnifiedEntry struct {
	Client     string
	ModelID    string
	ProviderID string
	SessionID  string
	// WorkspaceKey is the normalized workspace identity (escaped path or
	// directory segment); WorkspaceLabel is its display segment.
	WorkspaceKey   string
	WorkspaceLabel string
	TimestampMS    int64
	Tokens         TokenBreakdown
	// CacheWrite1h is the share of CacheWrite written with Anthropic's 1-hour
	// cache TTL; carried for display, never surcharged in estimates.
	CacheWrite1h int64
	DurationMS   int64
	MessageCount int32
	Agent        string
	DedupKey     string
	IsTurnStart  bool
	// ModelAttributionConflicted marks rows with conflicting authoritative
	// model evidence; they stay unpriced rather than accepting a guess.
	ModelAttributionConflicted bool
}

// Bucket15MS is the UTC 15-minute grid index of the timestamp in epoch
// milliseconds (migration 035 rationale: every civil UTC offset in use is a
// multiple of fifteen minutes).
func Bucket15MS(tsMS int64) int64 {
	const bucket = int64(15 * time.Minute / time.Millisecond)
	return tsMS / bucket * bucket
}

// ParseRFC3339MS parses an RFC 3339 timestamp to epoch milliseconds; ok is
// false for blank or malformed values.
func ParseRFC3339MS(value string) (int64, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return 0, false
	}
	return t.UnixMilli(), true
}

// NormalizeWorkspaceKey turns a recorded working directory into a stable
// workspace identity: escaped separators, trimmed, empty for none.
func NormalizeWorkspaceKey(cwd string) string {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return ""
	}
	if u, err := url.Parse(cwd); err == nil && u.Scheme != "" && u.Host != "" {
		// file:// URLs appear in some clients; normalize to the path.
		cwd = u.Path
	}
	cwd = strings.Trim(cwd, "/")
	if cwd == "" {
		return ""
	}
	return strings.ReplaceAll(cwd, "/", "--")
}

// WorkspaceLabelFromKey is the display segment of a workspace key: the last
// path component, unescaped.
func WorkspaceLabelFromKey(key string) string {
	if key == "" {
		return ""
	}
	if idx := strings.LastIndex(key, "--"); idx >= 0 && idx+2 < len(key) {
		return key[idx+2:]
	}
	return key
}

// WorkspaceAfterMarker returns the single directory segment directly after
// the marker segment ("projects" for directory-structured clients), with its
// display label. followedBy, when non-empty, requires that segment next
// (qwen nests a "chats" level; claude does not). Empty when absent.
func WorkspaceAfterMarker(file, marker, followedBy string) (key, label string) {
	dir := path.Dir(file)
	segments := strings.Split(dir, "/")
	for i := 0; i+2 < len(segments); i++ {
		if segments[i] != marker {
			continue
		}
		ws := segments[i+1]
		if ws == "" {
			return "", ""
		}
		if followedBy != "" && segments[i+2] != followedBy {
			return "", ""
		}
		return ws, WorkspaceLabelFromKey(ws)
	}
	return "", ""
}

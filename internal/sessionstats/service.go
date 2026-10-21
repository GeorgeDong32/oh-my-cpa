package sessionstats

import (
	"context"
	"fmt"
	"time"
)

// FileWatermark is the persisted scan state of one source file.
type FileWatermark struct {
	Client     string
	Path       string
	MtimeMS    int64
	SizeBytes  int64
	ByteOffset int64
	PrefixHash string
	EntryRows  int64
	Status     string
}

// Store is the persistence boundary for session statistics; the repository
// implements it. Every scan write shares the repository write gate.
type Store interface {
	// ListFileWatermarks returns the known watermarks for a client.
	ListFileWatermarks(ctx context.Context, client string) (map[string]FileWatermark, error)
	// ReplaceFileEntries swaps one file's message rows, advances its
	// watermark and marks every touched session dirty — one transaction.
	ReplaceFileEntries(ctx context.Context, watermark FileWatermark, entries []UnifiedEntry, touchedSessions map[string]struct{}) error
	// MarkFileStatus records an unreadable/oversized file with no rows.
	MarkFileStatus(ctx context.Context, watermark FileWatermark) error
	// DropFile removes a vanished file's rows and marks its sessions dirty.
	DropFile(ctx context.Context, client, path string) (touchedSessions map[string]struct{}, err error)
	// DirtySessions lists the sessions awaiting an activity rebuild.
	DirtySessions(ctx context.Context) (map[string]map[string]struct{}, error)
	// ReplaceSessionActivity rebuilds one session's activity rows and clears
	// its dirty mark — one transaction.
	ReplaceSessionActivity(ctx context.Context, client, sessionID string, buckets []ActivityBucket) error
	// SessionTimeline loads a session's deduped points for sessionization.
	SessionTimeline(ctx context.Context, client, sessionID string) ([]SessionPoint, error)
	// SummaryRows answer the read APIs: deduped aggregates grouped to the
	// requested grain, still on the UTC bucket grid.
	SummaryRows(ctx context.Context, fromBucket int64) ([]AggregatedRow, error)
	// ClientStatusRows report per-client counts and scan state.
	ClientStatusRows(ctx context.Context) ([]ClientStatusRow, error)
}

// AggregatedRow is a deduped, grouped usage row on the 15-minute grid.
type AggregatedRow struct {
	Bucket15MS int64
	Client     string
	Model      string
	Provider   string
	Workspace  string
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
	Reasoning  int64
	Messages   int64
	Sessions   int64
}

// ClientStatusRow summarizes one client's stored coverage.
type ClientStatusRow struct {
	Client     string
	Files      int64
	Entries    int64
	Unreadable int64
	Oversized  int64
	OldestTSMS int64
	NewestTSMS int64
	LastScanMS int64
}

// Service owns the scan loop and the read path.
type Service struct {
	store    Store
	parsers  []Parser
	home     string
	interval time.Duration
	logger   Logger
	clock    func() time.Time

	wake chan struct{}
}

// Logger matches the pricing service's minimal surface.
type Logger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
}

// NewService wires the parsers; home is the filesystem root the client
// directories resolve under.
func NewService(store Store, home string, logger Logger) *Service {
	service := &Service{
		store:    store,
		home:     home,
		interval: 10 * time.Minute,
		logger:   logger,
		clock:    time.Now,
		wake:     make(chan struct{}, 1),
		parsers: []Parser{
			claudeParser{},
			codexParser{},
			piParser{client: "pi", roots: piRoots},
			qwenParser{},
			kimiParser{},
			geminiParser{},
			opencodeParser{},
			ampParser{},
			droidParser{},
			zedParser{},
			// Batch 2: the remaining file-class clients (spec §2).
			ompParser{},
			kimchiParser{},
			senpiParser{},
			craftAgentParser{},
			codebuddyParser{},
			workbuddyParser{},
			cherrystudioParser{},
			grokParser{},
			lmstudioParser{},
			codebuffParser{},
			freebuffParser{},
			dshParser{},
			muxParser{},
			reasonixParser{},
			mcodeParser{},
			gjcParser{},
			junieParser{},
			opencodereviewParser{},
			jcodeParser{},
			augmentParser{},
			fxParser{},
			commandcodeParser{},
			openclawParser{},
			gooseParser{},
			crushParser{},
		},
	}
	return service
}

func piRoots(home string) []string {
	return []string{home + "/.pi/agent/sessions"}
}

// SetInterval overrides the scan cadence (tests).
func (s *Service) SetInterval(interval time.Duration) {
	if interval > 0 {
		s.interval = interval
	}
}

// Run drives the scan loop until the context ends.
func (s *Service) Run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	s.ScanOnce(ctx) // on start
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.wake:
		}
		s.ScanOnce(ctx)
	}
}

// TriggerScan wakes the loop; false when a scan request is already pending.
func (s *Service) TriggerScan() bool {
	select {
	case s.wake <- struct{}{}:
		return true
	default:
		return false
	}
}

// ScanOnce walks every parser's roots once. A parser failure marks its
// client unreadable for this pass and never fails the scan.
func (s *Service) ScanOnce(ctx context.Context) {
	for _, parser := range s.parsers {
		s.scanClient(ctx, parser)
	}
	s.rebuildActivity(ctx)
}

// parserVersion prefixes every watermark's prefix_hash: fixing a parser's
// extraction logic must invalidate stored rows even though the source files
// did not move — the upstream message_cache solves this with per-parser
// cache versions. Bump a client's version when its parser changes.
var parserVersions = map[string]string{
	"claude-code": "v1", "codex": "v2", "pi": "v1", "qwen": "v2", "kimi": "v1",
	"gemini": "v1", "opencode": "v2", "amp": "v2", "droid": "v1", "zed": "v1",
	"omp": "v1", "kimchi": "v1", "senpi": "v1", "craft-agent": "v1",
	"codebuddy": "v1", "workbuddy": "v1", "cherrystudio": "v1", "grok": "v1",
	"lmstudio": "v1", "codebuff": "v1", "freebuff": "v1", "dsh": "v1",
	"mux": "v1", "reasonix": "v1", "mcode": "v1", "gjc": "v1", "junie": "v1",
	"opencodereview": "v1", "jcode": "v1", "augment": "v1", "fx": "v1",
	"commandcode": "v1", "openclaw": "v1", "goose": "v1", "crush": "v1",
}

func parserVersion(client string) string {
	if version, ok := parserVersions[client]; ok {
		return version
	}
	return "v1"
}

func (s *Service) scanClient(ctx context.Context, parser Parser) {
	// A panicking parser marks its client unreadable and never takes the
	// process down: the scan loop is a guest on files another program owns.
	defer func() {
		if recovered := recover(); recovered != nil {
			s.logger.Warn("session parser panicked", "client", parser.Client(), "panic", recovered)
		}
	}()
	client := parser.Client()
	watermarks, err := s.store.ListFileWatermarks(ctx, client)
	if err != nil {
		s.logger.Warn("session scan state unreadable", "client", client, "error", err)
		return
	}
	// Seen spans every root, so a root whose directory vanished still drops
	// its files' rows (spec: rows follow the source files).
	seen := map[string]struct{}{}
	for _, root := range parser.Roots(s.home) {
		files, err := parser.Discover(root)
		if err != nil {
			s.logger.Warn("session scan discovery failed", "client", client, "error", err)
			continue
		}
		for _, file := range files {
			seen[file] = struct{}{}
			info, statErr := statFile(file)
			if statErr != nil {
				continue
			}
			mark := watermarks[file]
			version := parserVersion(client)
			if mark.MtimeMS == info.mtimeMS && mark.SizeBytes == info.size && mark.Status == "ok" && mark.PrefixHash == version {
				continue // unchanged and parsed by this parser version
			}
			if info.size > maxFileBytes {
				_ = s.store.MarkFileStatus(ctx, FileWatermark{
					Client: client, Path: file, MtimeMS: info.mtimeMS, SizeBytes: info.size,
					Status: "oversized", EntryRows: mark.EntryRows,
				})
				continue
			}
			// In-file duplicates (streaming repeats) merge here per-field max,
			// making unique (client, path, dedup_key) rows a framework
			// guarantee whatever a parser emits.
			byKey := map[string]*UnifiedEntry{}
			var order []string
			parseErr := parser.ParseFile(file, info.mtimeMS, func(entry UnifiedEntry) {
				if entry.MessageCount == 0 {
					entry.MessageCount = 1
				}
				if entry.DedupKey == "" {
					// An empty key would otherwise merge every keyless entry in
					// the file into one row; give each its own stable identity
					// (the repository layer uses the same rule).
					entry.DedupKey = fmt.Sprintf("%s:%s:%d", entry.Client, entry.SessionID, entry.TimestampMS)
				}
				if existing, ok := byKey[entry.DedupKey]; ok {
					existing.Tokens.Input = maxInt64(existing.Tokens.Input, entry.Tokens.Input)
					existing.Tokens.Output = maxInt64(existing.Tokens.Output, entry.Tokens.Output)
					existing.Tokens.CacheRead = maxInt64(existing.Tokens.CacheRead, entry.Tokens.CacheRead)
					existing.Tokens.CacheWrite = maxInt64(existing.Tokens.CacheWrite, entry.Tokens.CacheWrite)
					existing.Tokens.Reasoning = maxInt64(existing.Tokens.Reasoning, entry.Tokens.Reasoning)
					return
				}
				captured := entry
				byKey[entry.DedupKey] = &captured
				order = append(order, entry.DedupKey)
			})
			var entries []UnifiedEntry
			for _, key := range order {
				entries = append(entries, *byKey[key])
			}
			watermark := FileWatermark{
				Client: client, Path: file, MtimeMS: info.mtimeMS, SizeBytes: info.size,
				ByteOffset: info.size, EntryRows: int64(len(entries)), PrefixHash: version,
			}
			if parseErr != nil {
				watermark.Status = "unreadable"
				_ = s.store.MarkFileStatus(ctx, watermark)
				continue
			}
			watermark.Status = "ok"
			touched := touchedSessionsOf(mark.EntryRows, entries)
			if err := s.store.ReplaceFileEntries(ctx, watermark, entries, touched); err != nil {
				s.logger.Warn("session scan write failed", "client", client, "file", file, "error", err)
			}
		}
	}
	for path := range watermarks {
		if _, ok := seen[path]; !ok {
			if _, err := s.store.DropFile(ctx, client, path); err != nil {
				s.logger.Warn("session scan drop failed", "client", client, "file", path, "error", err)
			}
		}
	}
}

// touchedSessionsOf merges the sessions a file's rows referenced before
// (delivered via a store read in ReplaceFileEntries) with the new set; the
// repository fills the before-set from the rows being replaced.
func touchedSessionsOf(_ int64, entries []UnifiedEntry) map[string]struct{} {
	sessions := map[string]struct{}{}
	for _, entry := range entries {
		sessions[entry.SessionID] = struct{}{}
	}
	return sessions
}

func (s *Service) rebuildActivity(ctx context.Context) {
	dirty, err := s.store.DirtySessions(ctx)
	if err != nil {
		s.logger.Warn("session activity dirty set unreadable", "error", err)
		return
	}
	for client, sessions := range dirty {
		for sessionID := range sessions {
			points, err := s.store.SessionTimeline(ctx, client, sessionID)
			if err != nil {
				s.logger.Warn("session timeline unreadable", "client", client, "session", sessionID, "error", err)
				continue
			}
			buckets := Sessionize(points, DefaultIdleGapMS)
			if err := s.store.ReplaceSessionActivity(ctx, client, sessionID, buckets); err != nil {
				s.logger.Warn("session activity rebuild failed", "client", client, "session", sessionID, "error", err)
			}
		}
	}
}

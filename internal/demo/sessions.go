package demo

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"github.com/oh-my-cpa/oh-my-cpa/internal/repository"
	"github.com/oh-my-cpa/oh-my-cpa/internal/sessionstats"
)

// seedSessions fills the session statistics tables with a small synthetic
// history: three clients, four models, ten days (docs/plans/session-stats.md
// §6 — synthesized workspace keys, never real path shapes).
func seedSessions(ctx context.Context, repo *repository.Repository, now time.Time) error {
	type seedClient struct {
		client string
		models []string
	}
	clients := []seedClient{
		{client: "claude-code", models: []string{"claude-opus-5.5", "claude-haiku-4.5"}},
		{client: "codex", models: []string{"gpt-5.6-sol"}},
		{client: "pi", models: []string{"glm-5.3"}},
	}
	// Deterministic layout: the same fixture on every seed.
	source := rand.New(rand.NewSource(451))
	day := now.UTC().Truncate(24 * time.Hour)
	for dayOffset := 9; dayOffset >= 0; dayOffset-- {
		base := day.AddDate(0, 0, -dayOffset)
		for index, seed := range clients {
			file := fmt.Sprintf("/home/demo/%s/session-%d.jsonl", seed.client, dayOffset)
			var entries []sessionstats.UnifiedEntry
			messages := 3 + source.Intn(6)
			for i := 0; i < messages; i++ {
				model := seed.models[source.Intn(len(seed.models))]
				ts := base.Add(time.Duration(9+i)*time.Hour + time.Duration(source.Intn(45))*time.Minute)
				entries = append(entries, sessionstats.UnifiedEntry{
					Client: seed.client, ModelID: model, ProviderID: "demo",
					SessionID: fmt.Sprintf("demo-%s-%d", seed.client, dayOffset),
					WorkspaceKey: fmt.Sprintf("demo-workspace-%d", index),
					WorkspaceLabel: fmt.Sprintf("workspace-%d", index),
					TimestampMS:   ts.UnixMilli(),
					Tokens: sessionstats.TokenBreakdown{
						Input: int64(400 + source.Intn(2400)), Output: int64(80 + source.Intn(500)),
						CacheRead: int64(100 + source.Intn(900)), CacheWrite: int64(source.Intn(60)),
						Reasoning: int64(source.Intn(120)),
					},
					DurationMS: int64(4 + source.Intn(40)) * 1000, MessageCount: 1,
					DedupKey: fmt.Sprintf("demo:%s:%d:%d", seed.client, dayOffset, i),
				})
			}
			watermark := sessionstats.FileWatermark{
				Client: seed.client, Path: file, MtimeMS: now.UnixMilli(), SizeBytes: 4096,
				ByteOffset: 4096, EntryRows: int64(len(entries)), Status: "ok",
			}
			touched := map[string]struct{}{}
			for _, entry := range entries {
				touched[entry.SessionID] = struct{}{}
			}
			if err := repo.ReplaceFileEntries(ctx, watermark, entries, touched); err != nil {
				return fmt.Errorf("seed session file: %w", err)
			}
			for sessionID := range touched {
				points, err := repo.SessionTimeline(ctx, seed.client, sessionID)
				if err != nil {
					return err
				}
				buckets := sessionstats.Sessionize(points, sessionstats.DefaultIdleGapMS)
				if err := repo.ReplaceSessionActivity(ctx, seed.client, sessionID, buckets); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

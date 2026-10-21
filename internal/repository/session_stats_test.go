package repository

import (
	"context"
	"testing"

	"github.com/oh-my-cpa/oh-my-cpa/internal/sessionstats"
)

func sessionStoreTestRepository(t *testing.T) *Repository {
	t.Helper()
	return usageTestRepository(t)
}

func entryFor(path, dedup string, ts int64, tokens sessionstats.TokenBreakdown) sessionstats.UnifiedEntry {
	return sessionstats.UnifiedEntry{
		Client: "pi", ModelID: "glm-5.3", ProviderID: "pi", SessionID: "s-1",
		WorkspaceKey: "w", TimestampMS: ts, Tokens: tokens, DedupKey: dedup,
	}
}

// A file's rows replace atomically, the touched sessions land in the dirty
// set, the activity rebuild folds the timeline, and the read-time dedup
// merges cross-file copies of a stable key with per-field max.
func TestSessionStatsStoreScanAndRead(t *testing.T) {
	r := sessionStoreTestRepository(t)
	ctx := context.Background()

	// File A: the parser already merged its streaming copies in-file (the
	// contract parsers own); one row lands per stable key.
	markA := sessionstats.FileWatermark{Client: "pi", Path: "/a.jsonl", MtimeMS: 1, SizeBytes: 10, ByteOffset: 10, Status: "ok", EntryRows: 1}
	if err := r.ReplaceFileEntries(ctx, markA, []sessionstats.UnifiedEntry{
		entryFor("/a.jsonl", "pi:response:r1", 1_700_000_000_000, sessionstats.TokenBreakdown{Input: 100, Output: 20}),
	}, nil); err != nil {
		t.Fatal(err)
	}
	dirty, err := r.DirtySessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(dirty["pi"]) != 1 {
		t.Fatalf("dirty set: %+v", dirty)
	}
	points, err := r.SessionTimeline(ctx, "pi", "s-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 1 {
		t.Fatalf("stable-key copies must fold in the timeline: %+v", points)
	}
	if err := r.ReplaceSessionActivity(ctx, "pi", "s-1", sessionstats.Sessionize(points, sessionstats.DefaultIdleGapMS)); err != nil {
		t.Fatal(err)
	}
	dirty, err = r.DirtySessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(dirty["pi"]) != 0 {
		t.Fatalf("dirty set not cleared: %+v", dirty)
	}

	// File B: the same stable key from a fork copy, with a fuller count and a
	// second session-scoped key that must not merge across files.
	markB := sessionstats.FileWatermark{Client: "pi", Path: "/b.jsonl", MtimeMS: 1, SizeBytes: 10, ByteOffset: 10, Status: "ok", EntryRows: 2}
	if err := r.ReplaceFileEntries(ctx, markB, []sessionstats.UnifiedEntry{
		entryFor("/b.jsonl", "pi:response:r1", 1_700_000_000_000, sessionstats.TokenBreakdown{Input: 100, Output: 25}),
		func() sessionstats.UnifiedEntry {
			entry := entryFor("/b.jsonl", "claude:s-9:tool_result:t1", 1_700_000_090_000, sessionstats.TokenBreakdown{Input: 5})
			entry.SessionID = "s-2"
			entry.ModelID = "claude-3.7-sonnet"
			return entry
		}(),
	}, nil); err != nil {
		t.Fatal(err)
	}

	rows, err := r.SummaryRows(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	var r1Input, r1Output, toolResultInput int64
	for _, row := range rows {
		if row.Model == "glm-5.3" {
			r1Input += row.Input
			r1Output += row.Output
		}
	}
	_ = toolResultInput
	if r1Input != 100 || r1Output != 25 {
		t.Fatalf("stable key merged with per-field max: input=%d output=%d (want 100/25)", r1Input, r1Output)
	}

	// A vanished file drops its rows; the tool_result row from file B survives.
	if _, err := r.DropFile(ctx, "pi", "/a.jsonl"); err != nil {
		t.Fatal(err)
	}
	rows, err = r.SummaryRows(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("fork copy + session-scoped row remain: %+v", rows)
	}

	// Watermarks advance and are read back per client.
	watermarks, err := r.ListFileWatermarks(ctx, "pi")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := watermarks["/b.jsonl"]; !ok {
		t.Fatalf("watermarks: %+v", watermarks)
	}
	if _, ok := watermarks["/a.jsonl"]; ok {
		t.Fatal("dropped file kept its watermark")
	}

	statuses, err := r.ClientStatusRows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || statuses[0].Client != "pi" || statuses[0].Entries != 2 {
		t.Fatalf("client status: %+v", statuses)
	}
}

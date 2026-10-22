package api

import (
	"strings"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/oh-my-cpa/oh-my-cpa/internal/repository"
	"github.com/oh-my-cpa/oh-my-cpa/internal/sessionstats"
)

// seedSessionRows lands entries through the same repository path the scan
// writes (ReplaceFileEntries marks the bucket dirty), so the cache folding
// under test is the production one.
func seedSessionRows(t *testing.T, repo *repository.Repository, client, session, model string, ageHours int) {
	t.Helper()
	now := time.Now().Add(-time.Duration(ageHours) * time.Hour)
	entries := []sessionstats.UnifiedEntry{{
		Client: client, ModelID: model, ProviderID: "test", SessionID: session,
		TimestampMS:  now.UnixMilli(),
		Tokens:       sessionstats.TokenBreakdown{Input: 100, Output: 20},
		DedupKey:     client + ":" + session + ":" + model,
		MessageCount: 1,
	}}
	watermark := sessionstats.FileWatermark{
		Client: client, Path: "/tmp/" + client + "-" + session + ".jsonl",
		MtimeMS: now.UnixMilli(), SizeBytes: 128, EntryRows: int64(len(entries)),
	}
	if err := repo.ReplaceFileEntries(context.Background(), watermark, entries, nil); err != nil {
		t.Fatalf("seed session entries: %v", err)
	}
}

func decodeBody(t *testing.T, payload []byte) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return body
}

func expectTokens(t *testing.T, body map[string]any, floor float64) {
	t.Helper()
	tokens, _ := body["total_tokens"].(float64)
	if tokens < floor {
		t.Fatalf("expected total_tokens >= %v, got %v", floor, body["total_tokens"])
	}
}

// The range parameter accepts the calendar-today and all forms beside rolling
// day counts: today anchors on the viewer's calendar day, all is unbounded,
// and both read the same folded cache the rolling windows do.
func TestSessionsRangeForms(t *testing.T) {
	client, base, repo := startDashboardTestServer(t, nil)
	seedSessionRows(t, repo, "pi", "sess-1", "glm-5.3", 2)
	seedSessionRows(t, repo, "pi", "sess-2", "glm-5.3", 26)
	// The scan's tail folds the cache; the service is not running in this
	// harness, so fold directly through the repository.
	buckets, err := repo.DirtyBuckets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) == 0 {
		t.Fatal("seeded rows must mark their buckets dirty")
	}
	if err := repo.FoldBucketCache(context.Background(), buckets); err != nil {
		t.Fatal(err)
	}
	for _, days := range []string{"today", "7", "30", "all"} {
		response, payload := getJSON(t, client, base+"/omc/api/v1/sessions/summary?days="+days+"&tz=UTC")
		if response.StatusCode != 200 {
			t.Fatalf("days=%s: status %d body %s", days, response.StatusCode, payload)
		}
		expectTokens(t, decodeBody(t, payload), 120)
	}
}

// Folding is idempotent and deletion-aware: a refold after a file vanishes
// drops its contribution instead of doubling it.
func TestSessionsFoldIdempotent(t *testing.T) {
	client, base, repo := startDashboardTestServer(t, nil)
	seedSessionRows(t, repo, "codex", "sess-1", "gpt-5.6", 3)
	buckets, _ := repo.DirtyBuckets(context.Background())
	if err := repo.FoldBucketCache(context.Background(), buckets); err != nil {
		t.Fatal(err)
	}
	if err := repo.FoldBucketCache(context.Background(), buckets); err != nil {
		t.Fatal(err)
	}
	response, payload := getJSON(t, client, base+"/omc/api/v1/sessions/summary?days=all&tz=UTC")
	if response.StatusCode != 200 {
		t.Fatalf("status %d", response.StatusCode)
	}
	body := decodeBody(t, payload)
	tokens, _ := body["total_tokens"].(float64)
	if tokens != 120 {
		t.Fatalf("refold must not double rows: total_tokens=%v", tokens)
	}
	if _, err := repo.DropFile(context.Background(), "codex", "/tmp/codex-sess-1.jsonl"); err != nil {
		t.Fatal(err)
	}
	buckets, _ = repo.DirtyBuckets(context.Background())
	if err := repo.FoldBucketCache(context.Background(), buckets); err != nil {
		t.Fatal(err)
	}
	_, payload = getJSON(t, client, base+"/omc/api/v1/sessions/summary?days=all&tz=UTC")
	body = decodeBody(t, payload)
	tokens, _ = body["total_tokens"].(float64)
	if tokens != 0 {
		t.Fatalf("deleted rows must leave the cache: total_tokens=%v", tokens)
	}
}

// The series endpoint splits the folded buckets by client or model.
func TestSessionsSeriesSplit(t *testing.T) {
	client, base, repo := startDashboardTestServer(t, nil)
	seedSessionRows(t, repo, "pi", "sess-1", "glm-5.3", 2)
	seedSessionRows(t, repo, "codex", "sess-2", "gpt-5.6", 3)
	buckets, _ := repo.DirtyBuckets(context.Background())
	if err := repo.FoldBucketCache(context.Background(), buckets); err != nil {
		t.Fatal(err)
	}
	for _, split := range []string{"client", "model"} {
		response, payload := getJSON(t, client, base+"/omc/api/v1/sessions/series?days=30&split="+split+"&tz=UTC")
		if response.StatusCode != 200 {
			t.Fatalf("split=%s: status %d body %s", split, response.StatusCode, payload)
		}
		body := decodeBody(t, payload)
		series, _ := body["series"].([]any)
		if len(series) < 2 {
			t.Fatalf("split=%s: expected one row per key, got %v", split, body)
		}
		first, _ := series[0].(map[string]any)
		if name, _ := first["name"].(string); name == "" {
			t.Fatalf("split=%s: series row missing name: %v", split, first)
		}
	}
	// The invalid split answers 400.
	response, _ := getJSON(t, client, base+"/omc/api/v1/sessions/series?days=30&split=nope&tz=UTC")
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid split: status %d", response.StatusCode)
	}
}

// The today range can answer in hourly buckets: the 15-minute cache folds
// into wall-clock hours of the viewer's zone, and every other range rejects
// the granularity instead of ignoring it.
func TestSessionsHourlyBuckets(t *testing.T) {
	client, base, repo := startDashboardTestServer(t, nil)
	// Two entries three hours apart today: the hourly read must separate them.
	seedSessionRows(t, repo, "pi", "sess-1", "glm-5.3", 1)
	seedSessionRows(t, repo, "pi", "sess-2", "glm-5.3", 4)
	buckets, _ := repo.DirtyBuckets(context.Background())
	if err := repo.FoldBucketCache(context.Background(), buckets); err != nil {
		t.Fatal(err)
	}
	response, payload := getJSON(t, client, base+"/omc/api/v1/sessions/daily?days=today&bucket=hour&tz=UTC")
	if response.StatusCode != 200 {
		t.Fatalf("hourly daily: status %d body %s", response.StatusCode, payload)
	}
	body := decodeBody(t, payload)
	days, _ := body["days"].([]any)
	if len(days) < 2 {
		t.Fatalf("hourly daily must split the day into hours, got %v", body)
	}
	first, _ := days[0].(map[string]any)
	if date, _ := first["date"].(string); !strings.Contains(date, ":00") {
		t.Fatalf("hourly dates must carry the wall-clock hour, got %q", date)
	}
	response, payload = getJSON(t, client, base+"/omc/api/v1/sessions/series?days=today&bucket=hour&split=client&tz=UTC")
	if response.StatusCode != 200 {
		t.Fatalf("hourly series: status %d body %s", response.StatusCode, payload)
	}
	// Any non-today range must refuse the granularity outright.
	response, _ = getJSON(t, client, base+"/omc/api/v1/sessions/daily?days=30&bucket=hour&tz=UTC")
	if response.StatusCode != 400 {
		t.Fatalf("hourly outside today must be rejected, got status %d", response.StatusCode)
	}
	response, _ = getJSON(t, client, base+"/omc/api/v1/sessions/daily?days=today&bucket=week&tz=UTC")
	if response.StatusCode != 400 {
		t.Fatalf("unknown bucket must be rejected, got status %d", response.StatusCode)
	}
}

package repository

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/oh-my-cpa/oh-my-cpa/internal/sessionstats"
)

// Compile-time contract: the repository is the session stats store.
var _ sessionstats.Store = (*Repository)(nil)

// dedupedSessionMessages is the read-time dedup view (spec §4): numeric
// fields take the per-field MAX across copies of a globally stable key — the
// port of merge_message_completeness; identity fields come from the
// lexicographically smallest path. Session-scoped keys (the tool_result
// marker) merge per file only and never collapse across files.
const dedupedSessionMessages = `WITH scoped AS (
  SELECT m.*, CASE WHEN m.dedup_key LIKE '%:tool_result:%' THEN m.path ELSE '' END AS merge_scope
  FROM session_messages m
), ranked AS (
  SELECT s.*, ROW_NUMBER() OVER (PARTITION BY s.client, s.dedup_key, s.merge_scope ORDER BY s.path) AS rn
  FROM scoped s
), winner AS (
  SELECT r.* FROM ranked r WHERE r.rn = 1
), nums AS (
  SELECT s.client, s.dedup_key, s.merge_scope,
    MAX(s.input) AS input, MAX(s.output) AS output, MAX(s.cache_read) AS cache_read,
    MAX(s.cache_write) AS cache_write, MAX(s.reasoning) AS reasoning,
    MAX(s.cache_write_1h) AS cache_write_1h, MAX(s.message_count) AS message_count,
    MAX(s.duration_ms) AS duration_ms
  FROM scoped s GROUP BY s.client, s.dedup_key, s.merge_scope
)
SELECT w.client, w.path, w.dedup_key, w.ts_ms, w.bucket_15m, w.session_id, w.model, w.provider,
 w.ws_key, w.agent, w.is_turn_start,
 n.input, n.output, n.cache_read, n.cache_write, n.reasoning, n.cache_write_1h, n.message_count, n.duration_ms
FROM winner w JOIN nums n
 ON n.client = w.client AND n.dedup_key = w.dedup_key AND n.merge_scope = w.merge_scope`

func (r *Repository) ListFileWatermarks(ctx context.Context, client string) (map[string]sessionstats.FileWatermark, error) {
	rows, err := r.SQL().QueryContext(ctx, `SELECT client, path, mtime_ms, size_bytes, byte_offset, prefix_hash, entry_rows, status
	 FROM session_files WHERE client = ?`, client)
	if err != nil {
		return nil, fmt.Errorf("read session file watermarks: %w", err)
	}
	defer rows.Close()
	result := map[string]sessionstats.FileWatermark{}
	for rows.Next() {
		var mark sessionstats.FileWatermark
		if err := rows.Scan(&mark.Client, &mark.Path, &mark.MtimeMS, &mark.SizeBytes, &mark.ByteOffset, &mark.PrefixHash, &mark.EntryRows, &mark.Status); err != nil {
			return nil, fmt.Errorf("scan session file watermark: %w", err)
		}
		result[mark.Path] = mark
	}
	return result, rows.Err()
}

// ReplaceFileEntries swaps one file's message rows, advances its watermark
// and marks every touched session dirty — one transaction, so an interrupted
// scan resumes at a file boundary and the dirty set never loses a session.
func (r *Repository) ReplaceFileEntries(ctx context.Context, watermark sessionstats.FileWatermark, entries []sessionstats.UnifiedEntry, _ map[string]struct{}) error {
	tx, err := r.SQL().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// The sessions the outgoing rows referenced must be rebuilt too.
	oldSessions, err := deleteSessionFileRows(ctx, tx, watermark.Client, watermark.Path)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if _, err := tx.ExecContext(ctx, `INSERT INTO session_messages
		 (client, path, dedup_key, ts_ms, bucket_15m, session_id, model, provider, ws_key, agent, is_turn_start,
		  input, output, cache_read, cache_write, reasoning, cache_write_1h, message_count, duration_ms)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			entry.Client, watermark.Path, dedupKeyOrFallback(entry), entry.TimestampMS,
			sessionstats.Bucket15MS(entry.TimestampMS), entry.SessionID, entry.ModelID, entry.ProviderID,
			entry.WorkspaceKey, entry.Agent, boolInt(entry.IsTurnStart),
			entry.Tokens.Input, entry.Tokens.Output, entry.Tokens.CacheRead, entry.Tokens.CacheWrite,
			entry.Tokens.Reasoning, entry.CacheWrite1h, entry.MessageCount, entry.DurationMS); err != nil {
			return fmt.Errorf("insert session message: %w", err)
		}
		oldSessions[entry.SessionID] = struct{}{}
	}
	for sessionID := range oldSessions {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO session_rebuild (client, session_id) VALUES (?,?)`,
			watermark.Client, sessionID); err != nil {
			return fmt.Errorf("mark session dirty: %w", err)
		}
	}
	if err := upsertSessionFile(ctx, tx, watermark); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *Repository) MarkFileStatus(ctx context.Context, watermark sessionstats.FileWatermark) error {
	tx, err := r.SQL().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := upsertSessionFile(ctx, tx, watermark); err != nil {
		return err
	}
	return tx.Commit()
}

func upsertSessionFile(ctx context.Context, tx *sql.Tx, watermark sessionstats.FileWatermark) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO session_files (client, path, mtime_ms, size_bytes, byte_offset, prefix_hash, entry_rows, status, updated_at_ms)
	 VALUES (?,?,?,?,?,?,?,?,?)
	 ON CONFLICT(client, path) DO UPDATE SET mtime_ms=excluded.mtime_ms, size_bytes=excluded.size_bytes,
	  byte_offset=excluded.byte_offset, prefix_hash=excluded.prefix_hash, entry_rows=excluded.entry_rows,
	  status=excluded.status, updated_at_ms=excluded.updated_at_ms`,
		watermark.Client, watermark.Path, watermark.MtimeMS, watermark.SizeBytes, watermark.ByteOffset,
		watermark.PrefixHash, watermark.EntryRows, statusOrOK(watermark.Status), time.Now().UnixMilli())
	return err
}

func (r *Repository) DropFile(ctx context.Context, client, path string) (map[string]struct{}, error) {
	tx, err := r.SQL().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	touched, err := deleteSessionFileRows(ctx, tx, client, path)
	if err != nil {
		return nil, err
	}
	for sessionID := range touched {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO session_rebuild (client, session_id) VALUES (?,?)`, client, sessionID); err != nil {
			return nil, err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM session_files WHERE client = ? AND path = ?`, client, path); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return touched, nil
}

func deleteSessionFileRows(ctx context.Context, tx *sql.Tx, client, path string) (map[string]struct{}, error) {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT session_id FROM session_messages WHERE client = ? AND path = ?`, client, path)
	if err != nil {
		return nil, err
	}
	touched := map[string]struct{}{}
	for rows.Next() {
		var sessionID string
		if err := rows.Scan(&sessionID); err != nil {
			rows.Close()
			return nil, err
		}
		touched[sessionID] = struct{}{}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM session_messages WHERE client = ? AND path = ?`, client, path); err != nil {
		return nil, err
	}
	return touched, nil
}

func (r *Repository) DirtySessions(ctx context.Context) (map[string]map[string]struct{}, error) {
	rows, err := r.SQL().QueryContext(ctx, `SELECT client, session_id FROM session_rebuild`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]map[string]struct{}{}
	for rows.Next() {
		var client, sessionID string
		if err := rows.Scan(&client, &sessionID); err != nil {
			return nil, err
		}
		if result[client] == nil {
			result[client] = map[string]struct{}{}
		}
		result[client][sessionID] = struct{}{}
	}
	return result, rows.Err()
}

func (r *Repository) ReplaceSessionActivity(ctx context.Context, client, sessionID string, buckets []sessionstats.ActivityBucket) error {
	tx, err := r.SQL().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM session_activity WHERE client = ? AND session_id = ?`, client, sessionID); err != nil {
		return err
	}
	for _, bucket := range buckets {
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO session_activity (bucket_15m, client, session_id, ws_key, active_ms, messages)
		 VALUES (?,?,?,?,?,?)`, bucket.Bucket15MS, client, sessionID, bucket.WorkspaceKey, bucket.ActiveMS, bucket.Messages); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM session_rebuild WHERE client = ? AND session_id = ?`, client, sessionID); err != nil {
		return err
	}
	return tx.Commit()
}

// SessionTimeline loads one session's deduped points for sessionization.
func (r *Repository) SessionTimeline(ctx context.Context, client, sessionID string) ([]sessionstats.SessionPoint, error) {
	rows, err := r.SQL().QueryContext(ctx, `SELECT ts_ms, duration_ms, ws_key FROM (`+dedupedSessionMessages+`)
	 WHERE client = ? AND session_id = ? ORDER BY ts_ms`, client, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var points []sessionstats.SessionPoint
	for rows.Next() {
		var point sessionstats.SessionPoint
		if err := rows.Scan(&point.TimestampMS, &point.DurationMS, &point.WorkspaceKey); err != nil {
			return nil, err
		}
		points = append(points, point)
	}
	return points, rows.Err()
}

// SummaryRows answers the read APIs: deduped aggregates grouped to the UTC
// 15-minute grid — calendar days fold in Go against the console timezone.
func (r *Repository) SummaryRows(ctx context.Context, fromBucket int64) ([]sessionstats.AggregatedRow, error) {
	rows, err := r.SQL().QueryContext(ctx, `SELECT bucket_15m, client, model, provider, ws_key,
	  sum(input), sum(output), sum(cache_read), sum(cache_write), sum(reasoning), sum(message_count),
	  count(DISTINCT session_id)
	 FROM (`+dedupedSessionMessages+`)
	 WHERE bucket_15m >= ?
	 GROUP BY bucket_15m, client, model, provider, ws_key`, fromBucket)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []sessionstats.AggregatedRow
	for rows.Next() {
		var row sessionstats.AggregatedRow
		if err := rows.Scan(&row.Bucket15MS, &row.Client, &row.Model, &row.Provider, &row.Workspace,
			&row.Input, &row.Output, &row.CacheRead, &row.CacheWrite, &row.Reasoning, &row.Messages, &row.Sessions); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

func (r *Repository) ClientStatusRows(ctx context.Context) ([]sessionstats.ClientStatusRow, error) {
	rows, err := r.SQL().QueryContext(ctx, `SELECT f.client,
	 count(*), coalesce(sum(CASE WHEN f.status = 'ok' THEN f.entry_rows ELSE 0 END), 0),
	 coalesce(sum(CASE WHEN f.status = 'unreadable' THEN 1 ELSE 0 END), 0),
	 coalesce(sum(CASE WHEN f.status = 'oversized' THEN 1 ELSE 0 END), 0),
	 coalesce(max(f.updated_at_ms), 0)
	 FROM session_files f GROUP BY f.client`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []sessionstats.ClientStatusRow
	for rows.Next() {
		var row sessionstats.ClientStatusRow
		if err := rows.Scan(&row.Client, &row.Files, &row.Entries, &row.Unreadable, &row.Oversized, &row.LastScanMS); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

func dedupKeyOrFallback(entry sessionstats.UnifiedEntry) string {
	if entry.DedupKey != "" {
		return entry.DedupKey
	}
	return fmt.Sprintf("%s:%s:%d", entry.Client, entry.SessionID, entry.TimestampMS)
}

func statusOrOK(status string) string {
	if status == "" {
		return "ok"
	}
	return status
}

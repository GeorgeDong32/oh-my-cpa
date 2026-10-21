-- Local session statistics (docs/plans/session-stats.md): client-side coding
-- session usage, ported from tokens.ci's parsers (MIT, missuo/tokens).
-- Message-level usage facts only — content never crosses this boundary.

-- Scan watermark per source file: a file is re-parsed when mtime or size
-- changed; status marks unreadable/oversized skips so the console can report
-- them without failing the scan.
CREATE TABLE session_files (
    client TEXT NOT NULL,
    path TEXT NOT NULL,
    mtime_ms INTEGER NOT NULL DEFAULT 0,
    size_bytes INTEGER NOT NULL DEFAULT 0,
    byte_offset INTEGER NOT NULL DEFAULT 0,
    prefix_hash TEXT NOT NULL DEFAULT '',
    entry_rows INTEGER NOT NULL DEFAULT 0,
    status TEXT NOT NULL DEFAULT 'ok' CHECK (status IN ('ok', 'unreadable', 'oversized')),
    updated_at_ms INTEGER NOT NULL,
    PRIMARY KEY (client, path)
);
CREATE INDEX idx_session_files_client ON session_files(client);

-- One usage row per (file, dedup key): parsers merge streaming duplicates
-- within a file at parse time (per-field max); cross-file duplicates of
-- globally stable keys merge at read time (docs/plans/session-stats.md §4).
-- bucket_15m is the UTC 15-minute grid of ts_ms — every civil UTC offset in
-- use is a multiple of fifteen minutes, so a local calendar day is a whole
-- number of buckets in any deployment timezone (migration 035 rationale).
CREATE TABLE session_messages (
    client TEXT NOT NULL,
    path TEXT NOT NULL,
    dedup_key TEXT NOT NULL,
    ts_ms INTEGER NOT NULL,
    bucket_15m INTEGER NOT NULL,
    session_id TEXT NOT NULL,
    model TEXT NOT NULL DEFAULT 'unknown',
    provider TEXT NOT NULL DEFAULT '',
    ws_key TEXT NOT NULL DEFAULT '',
    agent TEXT NOT NULL DEFAULT '',
    is_turn_start INTEGER NOT NULL DEFAULT 0 CHECK (is_turn_start IN (0, 1)),
    input INTEGER NOT NULL DEFAULT 0 CHECK (input >= 0),
    output INTEGER NOT NULL DEFAULT 0 CHECK (output >= 0),
    cache_read INTEGER NOT NULL DEFAULT 0 CHECK (cache_read >= 0),
    cache_write INTEGER NOT NULL DEFAULT 0 CHECK (cache_write >= 0),
    reasoning INTEGER NOT NULL DEFAULT 0 CHECK (reasoning >= 0),
    cache_write_1h INTEGER NOT NULL DEFAULT 0 CHECK (cache_write_1h >= 0),
    message_count INTEGER NOT NULL DEFAULT 1,
    duration_ms INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (client, path, dedup_key)
);
CREATE INDEX idx_session_messages_client_time ON session_messages(client, ts_ms);
CREATE INDEX idx_session_messages_client_session ON session_messages(client, session_id);

-- Dirty set for crash-safe activity rebuilds: the transaction that replaces a
-- file's rows marks every touched session; the rebuild pass folds activity
-- for exactly these and clears them in the same transaction as the result.
-- A crash between the two leaves the rows in place, so the next scan rebuilds
-- regardless of file watermarks.
CREATE TABLE session_rebuild (
    client TEXT NOT NULL,
    session_id TEXT NOT NULL,
    PRIMARY KEY (client, session_id)
);

-- Sessionized activity on the UTC 15-minute grid: idle-gap spans (3-minute
-- default) split proportionally across bucket boundaries. messages is the
-- deduped message count in that bucket.
CREATE TABLE session_activity (
    bucket_15m INTEGER NOT NULL,
    client TEXT NOT NULL,
    session_id TEXT NOT NULL,
    ws_key TEXT NOT NULL DEFAULT '',
    active_ms INTEGER NOT NULL DEFAULT 0 CHECK (active_ms >= 0),
    messages INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (bucket_15m, client, session_id, ws_key)
);
CREATE INDEX idx_session_activity_client_time ON session_activity(client, bucket_15m);

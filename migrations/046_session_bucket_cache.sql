-- Session statistics: pre-aggregated bucket cache (read-path optimization).
--
-- The reads used to re-run the dedup CTE over session_messages on every
-- request — a double full scan that grows with history regardless of the
-- requested window. This cache is folded from the same dedup view at scan
-- time (usage_facts pattern): the scan marks affected buckets dirty and
-- re-folds them idempotently, so a read becomes one indexed range scan.
--
-- Buckets, not tokens: costs stay read-time against the live price book.
CREATE TABLE session_bucket_cache (
    bucket_15m     INTEGER NOT NULL,
    client         TEXT NOT NULL,
    model          TEXT NOT NULL,
    provider       TEXT NOT NULL DEFAULT '',
    ws_key         TEXT NOT NULL DEFAULT '',
    input          INTEGER NOT NULL DEFAULT 0,
    output         INTEGER NOT NULL DEFAULT 0,
    cache_read     INTEGER NOT NULL DEFAULT 0,
    cache_write    INTEGER NOT NULL DEFAULT 0,
    reasoning      INTEGER NOT NULL DEFAULT 0,
    message_count  INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (bucket_15m, client, model, provider, ws_key)
);
CREATE INDEX idx_session_bucket_cache_bucket ON session_bucket_cache(bucket_15m);

-- The bucket-level dirty set, written inside the same transactions that
-- change session_messages (outgoing rows before delete, incoming rows after
-- insert), so a crash between write and fold re-folds on the next scan.
CREATE TABLE session_bucket_rebuild (
    bucket_15m INTEGER PRIMARY KEY
);

-- Window filters hit this index directly; the dedup CTE's own full scan was
-- the read path's dominant cost.
CREATE INDEX idx_session_messages_bucket ON session_messages(bucket_15m);

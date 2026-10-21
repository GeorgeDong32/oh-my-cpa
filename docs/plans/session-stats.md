# Local session statistics

Status: Batch 1 + Batch 2 delivered (35 clients registered, golden fixtures green);
Batch 3 delivered (46 clients registered: 10 + 25 + 11); feature complete.
Source of truth for the session-stats feature. Decisions here are final unless
the review overturns them; later batches update this document, never a side
conversation.

## 1. What this feature is

An observability view of **client-side AI coding sessions**: what Claude Code,
Codex, pi and the other local coding agents did on this machine, read from the
session files they already write. It complements the gateway view: usage events
count what CPA proxied; session stats count what the clients recorded —
including traffic that never went through the gateway. The two views are kept
on separate pages with explicit captions, and never merged into one number.

Hard rules:

- **Usage fields only.** Parsers read token counts, model names, timestamps and
  session identifiers. Message content is never stored, logged or shipped.
  A fixture test asserts no content field reaches any table or log line.
- **Fully local.** No network, no upload. tokens.ci's Rust parsers (MIT,
  `missuo/tokens`) are the format specification; the Go port credits them in
  the package doc.
- **Costs are read-time estimates** against the current price book; an
  unpriced model shows as unpriced, never $0, and never locks a version.

## 2. Client inventory

Ported from `missuo/tokens` (57 client parsers by file count — the ownership
rows below name 56 clients; the 57th, `copilot_desktop`, is an attached
parser that rides with Copilot CLI's deferral). Classes:

- **A — plain files** (JSONL/JSON): incremental watermarks.
- **B — third-party SQLite, read-only**: `modernc.org/sqlite`, read-only URI;
  never written.
- **C — VS Code extension globalStorage** task/workspace files.
- **D — data not locally derivable** (Cursor, Warp, Trae, Antigravity + CLI,
  Devin, Hindsight, Prime Agent: require tokens' own `sync` login and cache;
  Copilot CLI: its otel files exist only if tokens' exporter sidecar ran —
  availability, not auth). **Deferred, recorded only.**

| Batch | Clients | Class mix |
| --- | --- | --- |
| 1 (present on this machine; live-verified) | claudecode, codex, pi, qwen, kimi, gemini, opencode, amp, droid (Factory), zed | A ×8, B ×2 |
| 2 (golden fixtures from upstream formats) | cherrystudio, omp, openclaw, crush, goose, junie, fx, grok, lmstudio, codebuff, codebuddy, dsh, mux, tencent_buddy, commandcode, freebuff, gjc, jcode, kimchi, mcode, reasonix, senpi, craft_agent, opencodereview, augment | A ×20, B ×5 |
| 3 | cline, roocode, kilocode, copilot_vscode (C); kilo, kiro, hermes, micode, unsloth, workbuddy, zcode (B); synthetic (re-attribution filter) | C ×4, B ×7, filter |
| deferred (D) | cursor, warp, trae, antigravity, antigravity_cli, devin, hindsight, prime_agent, copilot (CLI; its otel files exist only if tokens' own exporter sidecar ran — a data-availability gap, not an account API one; copilot_desktop rides with it) | — |

Per-client paths and field maps live with the parser code (ported from the
Rust source, cited by file); this table is the ownership map, not a path copy.
A client whose data directory does not exist contributes nothing — zero
configuration, and a missing root is not an error.

## 3. Go package and data flow

```
internal/sessionstats/
  entry.go        UnifiedEntry, TokenBreakdown
  scanner.go      client-root discovery + mtime/size/offset watermarks
  <client>.go     one parser per client (port of sessions/<client>.rs)
  sessionize.go   idle-gap spans (port of sessionize.rs, 3-minute default gap)
  service.go      scan loop, scan state, read APIs
```

Flow per scan: for each client, list files → for each changed file, parse to a
stream of `UnifiedEntry` (never materialized whole-file; folded into inserts)
→ replace that file's rows in `session_messages` and mark touched sessions
dirty in one transaction with the watermark update → after the file pass,
rebuild `session_activity` for the dirty set and clear it, transactionally
(sidechain folding needs parent and child files in the same pass).

### UnifiedEntry (port of UnifiedMessage; cost fields dropped)

```go
type TokenBreakdown struct{ Input, Output, CacheRead, CacheWrite, Reasoning int64 }
// Buckets are mutually exclusive and additive; reasoning is NOT inside output.

type UnifiedEntry struct {
    Client, ModelID, ProviderID, SessionID string
    WorkspaceKey, WorkspaceLabel           string
    TimestampMS                            int64
    Tokens                                 TokenBreakdown
    CacheWrite1h                           int64  // Anthropic 1h cache-TTL share
    DurationMS                             int64
    MessageCount                           int32
    Agent                                  string // normalized subagent name
    DedupKey                               string // may be session-scoped (see below)
    IsTurnStart                            bool
    ModelAttributionConflicted             bool   // unpriced, never guessed
}
```

### Parser rules worth naming (the traps the Rust code solved; citations are upstream file:line-level, kept in code comments)

- **claudecode** (`claudecode.rs`): assistant entries dedup on
  `messageId:requestId`; without requestId the key is `message:{id}`; streaming
  duplicates merge with per-field max. tool_result keys carry a
  `:tool_result:` marker and are **session-scoped, not globally stable** —
  they die with their file. Sidechain entries re-attribute to the parent
  session id and a resolved subagent name. `<synthetic>` placeholder models
  suppressed; tool_result usage only from explicit structured metadata (char
  estimates double-count); workflow journals skipped; headless JSON variant;
  cc-mirror variants get their own client id.
  *Ported in Batch 1:* the assistant path, streaming max-merge, sidechain
  re-attribution (stem/agent-id names), synthetic suppression, tool_result
  structured-metadata extraction, journal skip. *Not ported:* headless JSON
  variant, cc-mirror variants, parent-file subagent name resolution — the
  local corpus carries none of them.
- **codex** (`codex.rs`): the incremental usage source is `last_token_usage`;
  `total_token_usage` drives the dedup key and the monotonicity check, with
  the stale-snapshot tolerance (2% or two-last coverage). The state machine
  keeps every upstream arm, including the defensive total-only delta and the
  first-event-takes-last rule; cached is split out of input and reasoning out
  of output so the buckets stay additive (`total() == total_tokens`). Model
  priority `model_info.slug → model → model_name`; timestamps anchor at the
  request window and durations span to the response.
- **pi** (`pi.rs`): requester vs subagent sessions separate on
  `session_info.name` (`subagent-` prefix) and the `rlmDepth` lineage;
  **omp** separates on directory shape.
- **B-class SQLite**: port the upstream SELECT verbatim; a schema mismatch or
  a mid-iteration error marks the client unreadable for this scan and does
  **not** advance its watermark (upstream `SqliteScan::Incomplete`).

## 4. Storage (migration `045_session_stats.sql`)

Message-level usage facts — the same philosophy as `usage_events` (per-request
facts, no payloads). Content never crosses this boundary; sizes stay bounded
(order 10⁵ rows machine-wide, a few hundred MB worst case).

```sql
session_files   (client, path PK, mtime_ms, size_bytes, byte_offset, prefix_hash,
                 entry_rows, status, updated_at_ms)
                 -- status: ok | unreadable | oversized
session_messages(client, path, dedup_key, ts_ms, bucket_15m, session_id, model,
                 provider, ws_key, agent, is_turn_start,
                 input, output, cache_read, cache_write, reasoning, cache_write_1h,
                 message_count, duration_ms,
                 PRIMARY KEY (client, path, dedup_key))
session_rebuild (client, session_id PK)   -- dirty set, see below
session_activity(bucket_15m, client, session_id, ws_key, active_ms, messages,
                 PRIMARY KEY (bucket_15m, client, session_id, ws_key))
```

- **Dedup and merging happen at read time, in SQL, under fully deterministic
  rules.** Rows group by `dedup_key` (globally stable keys may appear in
  several files — fork/`--resume` replays; session-scoped tool_result keys
  embed their session id and never match across files). Within a key group:
  numeric fields (the five token buckets, `cache_write_1h`, `message_count`,
  `duration_ms`) take per-field `MAX` — the port of
  `merge_message_completeness`; identity fields (`ts_ms`, `session_id`,
  `model`, `provider`, `ws_key`, `agent`, `is_turn_start`) come from the
  deterministic winner row, the lexicographically smallest `path` — upstream
  merges these fill-if-missing or not at all, and a fork replay legitimately
  carries a different `session_id` per copy, so one deterministic owner beats
  order-dependent merging. (Recorded deviation: fill-if-missing corner cases
  where only a duplicate holds a workspace name stay empty.)
- **Fifteen-minute buckets, matching migration 035's rationale.** `bucket_15m`
  is the UTC 15-minute grid of `ts_ms`, because every civil UTC offset in use
  is a multiple of fifteen minutes and a local calendar day is then a whole
  number of buckets in any deployment timezone — an hourly bucket cannot
  offer that (+05:30, +05:45). Calendar days, weekday patterns and "today"
  resolve at the API layer against the OMC time-zone preference (heatmap
  precedent: absolute buckets in storage, local calendars at read; switching
  zones never rewrites a row and triggers no rescan).
- **Sessions and active time** come from `session_activity`: idle-gap
  sessionization (3-minute gap, upstream default) over each
  `(client, session_id)` message timeline **consumed from the deduped view**
  (stable-key duplicates never double-count activity), spans split across
  15-minute bucket boundaries proportionally to wall overlap. A span carries
  the `ws_key` of the entry that opened it and never splits on workspace
  changes. Daily session count = `COUNT(DISTINCT session_id)` over the day's
  buckets — the source-session count, not the span count; a session crossing
  midnight counts in each civil day it touched, while the window tile counts
  it once per window (daily values may sum above the window tile; the page
  copy says so). Active time = `SUM(active_ms)`; active days derive per zone
  at read.
- **Crash-safe activity rebuild via the dirty set.** The transaction that
  replaces a file's `session_messages` rows also inserts every
  `(client, session_id)` the change touched — from the rows being deleted and
  the rows being inserted — into `session_rebuild`. A file deletion (rows
  removed, watermark retired) marks its sessions dirty the same way. The scan
  pass then rebuilds `session_activity` for exactly the dirty set and deletes
  those dirty rows in the same transaction as the rebuilt activity. A crash
  between the two transactions leaves the dirty rows in place, so the next
  scan rebuilds them regardless of file watermarks — no silently stale
  activity, ever.
- **Retention: rows follow the source files.** When a client prunes its own
  transcripts (Claude Code keeps ~30 days), those days age out of the view.
  The page states this horizon explicitly rather than silently showing a
  shorter tail; no synthetic retention is invented. (Recorded deviation from
  tokens.ci, which snapshots into its own cache.)
- Scan-loop runtime contract:
  - Cadence: on start, then every 10 minutes, plus the manual trigger.
  - The scan participates in the repository write gate like every writer;
    each file's row replacement + watermark advance + dirty marking is one
    transaction, so an interrupted scan resumes at a file boundary and is
    idempotent.
  - A file is re-parsed when mtime or size changed, or when the parser's
    version stamp (stored in the watermark's prefix_hash) differs — a parser
    fix re-parses unchanged files, the upstream message-cache invalidation
    rule. Byte-offset incremental parsing is not implemented in Batch 1: the
    watermark records the consumed size and a full re-parse happens on any
    change (documented simplification; the guard columns exist for a later
    incremental reader). Truncation (size < offset) resets to a full re-parse.
  - An unreadable file (I/O error, malformed) is retried with capped backoff
    and marked `unreadable` in scan status; it never fails the scan or the
    loop. Per-file size cap 512 MiB (upstream caps zed blobs at 32 MiB);
    oversized files are marked `oversized`, skipped and reported. A panicking
    parser is recovered per client and marked, never taking the process down.
  - Roots resolve from `$HOME`. Per-client environment overrides and
    container root remapping are **not implemented in Batch 1** (single-user
    local deployment is the only current shape); paths are stored as seen —
    moving a root is a re-scan, documented in operations.

## 5. Cost estimation (read time)

`session_messages` rows are priced by the current price book with deliberate
simplifications, all visible in the response:

- Base rates only: **no channel multiplier** (this traffic never crossed CPA;
  the client-recorded provider is not a CPA channel) and **no price tiers**
  (tier conditions are per-request assertions; a day-aggregated estimate
  cannot match them — stated on the page).
- The adapter maps the console's mutually exclusive buckets onto
  `pricing.Tokens`, whose `Input` **includes** cached and cache-creation
  tokens (quote.go's accounting): `Input = input + cache_read + cache_write`,
  `Output = output + reasoning` (both price at the completion rate;
  `pricing.Tokens` has no reasoning field). The mapping is explicit in code
  and unit-tested against the quote path.
- Matching follows the price book's exact-identity rule; resemblance never
  matches.
- `cache_write_1h` carries a premium upstream that the OMC book does not
  price; it is **not** surcharged, and the unpriced share says so.
- Response: `cost_source: "estimated"`, plus the unpriced token share.

## 6. API (console `/api/v1`, DTO allowlists)

```
GET  /api/v1/sessions/summary?days=N    tiles: totals, today, active days,
                                        session count, composition, est. cost
GET  /api/v1/sessions/daily?days=N      per-day: tokens by client/model, sessions, active_ms
GET  /api/v1/sessions/models?days=N     per-model: tokens, share, est. cost, clients
GET  /api/v1/sessions/clients           detected clients, last scan, entry counts, unreadable notes
POST /api/v1/sessions/rescan            manual trigger; 409 while running;
                                        audited like pricing.sync.start; refused in demo
```

`days` defaults to 30, maximum 365, validated. Reads are not audited (console
precedent). Every response field is a handwritten DTO in `internal/api/`; no
parser output crosses the wire. New routes are classified in the demo policy
in the batch that introduces them, with synthetic session fixtures in the demo
dataset (synthesized workspace keys — no real path shapes in the public
dataset).

## 7. Console page

Nav: Observe → **Sessions** (`/sessions`). Information architecture follows
the tokens.ci profile the operator referenced; **visual design follows
`docs/design.md` tokens and existing panel patterns** — nothing is copied
from the external site's styling.

1. Headline tiles: total tokens with five-part composition bar, estimated
   cost (caption: current book, unpriced share), active days, session count.
2. Today strip: tokens, est. cost, messages, per-model mini-bars.
3. Daily stacked chart (by client, toggle by model), 7/30/90-day ranges; a
   horizon caption states how far back the sources still reach (§4 retention).
4. Weekday distribution; biggest-day callout.
5. Model table: model, client(s), tokens, share, est. cost.
6. Client cards: detected clients with totals, last-scan state, and
   live-verified vs fixture-verified marks.

Copy i18n ×4 from Batch 1 (the page ships then). A one-line caption states
the 口径 difference from the gateway dashboard.

## 8. Testing strategy

- **Batch 1 — real data, sanitized snapshots**: one-time parser runs over
  this machine's real directories produce golden entry snapshots copied into
  `testdata/` (usage fields only); tests assert exact values against the
  snapshots. Live directories get a non-zero smoke check only — no drifty
  bounds. Where the upstream `tokens` CLI installs, a manual cross-check is
  recorded in the batch report (not a CI gate).
- **Batch 2/3 — golden fixtures**: per client, a minimal fixture built from
  the upstream format with golden `UnifiedEntry` expectations; client cards
  mark these fixture-verified.
- Unit tests mirror upstream cases for dedup/merge, codex deltas, sidechain
  folding, sessionize splits (including midnight splits), and the §1 privacy
  assertion (no content field in any table or log).
- Gates per AGENTS.md blast radius: `test:fast` while editing; batch
  completion runs `pnpm verify`, `pnpm check:ui` when the page or its
  interactions change, and `pnpm demo:generate` + `check:demo` whenever a
  console response shape changes (Batch 1 included).

## 9. Privacy and security review points

- Only §3 fields are read; the Batch 1 privacy fixture test is an AC.
- B-class DBs open read-only; A-class files read with process privileges; no
  new permissions requested.
- Routes sit behind the session cookie; demo policy entries ship with the
  routes; `session_*` tables are proposed **readable** under the database
  query classification (non-sensitive aggregates) — the classification lands
  with the migration PR.

## 10. Batch acceptance (per-batch AC, checked in each 验收报告)

- **Batch 1**: every client whose data directory exists on the dev machine
  yields entries (absent roots are no-ops); page renders real numbers; a
  double scan changes nothing (idempotent); read-time dedup verified by a
  crafted duplicate-key fixture; cost estimates tie to the price book ±
  rounding; migration forward-only; demo policy + synthetic session fixtures
  + dataset regenerated; `pnpm verify` and `pnpm check:ui` green; deployed to
  both machines with healthz green.
- **Batch 2**: every listed client parses its fixture; goldens green;
  absent-root no-ops covered by test.
- **Batch 3**: VS Code extension storages parse; synthetic re-attribution
  filter works; docs updated (architecture, operations, agent-capabilities as
  needed); i18n ×4 audited complete; demo dataset current; browser scenarios
  added and green.

## 11. Out of scope (recorded, not done)

- D-class account-API clients (§2) — revisit after Batch 3.
- Cross-machine aggregation — each instance reports its own machine.
- Per-message drill-down or content viewer — usage fields only.
- Importing tokens' own cache files — coupling to another tool's cache is
  not a contract we want.
- Cross-file per-field-max completeness merging beyond the SQL `MAX` rollup
  (§4) — the SQL merge covers stable keys; tool_result session-scoped keys
  never merge across files by design.

## 12. Recorded deviations (Batch 1 review)

- **amp**: the ledger/message reconciliation matcher is simplified to
  ledger-first, unmatched message records counted — the upstream's
  to_message_id pairing (which merges durations) is not ported; token totals
  match, durations may be coarser.
- **claudecode**: parent-file subagent name resolution, headless JSON and
  cc-mirror variants are not ported (absent from the local corpus).
- **codex**: the OpenClaw re-attribution and headless JSONL variant are not
  ported; the fork-scoped dedup key covers the double-count they guard
  against upstream.
- **byte-offset incremental parsing**: full re-parse on change (§4 note).
- The sessions page ships the tokens.ci information architecture minus the
  per-request log and the contribution heatmap styling; weekday bars and
  biggest-day replace the heatmap until a later batch wants it.

### Batch 2 deviations

- **grok**: dual-source scan ported (sessions tree + sibling logs root);
  the unified-log model attribution tracks `model changed` /
  `subagent model resolved` records per sid but not the upstream's pid
  generation and child-evidence machinery, and the subagent
  spawn/terminal evidence rows are not ported. The updates parser keeps
  the authoritative-usage path plus the monotonic cumulative-counter
  fallback (user-message bounded, first observation only records the
  baseline) but not the signals.json reconciliation; summary.json model
  metadata reads current_model_id/model_id/model. The unified log reads
  the capped whole file instead of the upstream's bounded streaming
  window. The prefer-unified-log rule is implemented at SESSION
  granularity in the repository read view (a grok session with unified
  rows drops its updates rows wholesale) rather than the upstream's
  timestamp-span suppression — same guarantee against the ~2×
  double-count, coarser at midnight-straddling boundaries.
- **lmstudio**: whole-file marker scan (see grok); the dedup key addresses
  the marker's byte offset, which stays stable across re-parses of an
  unchanged file. The model comes from the last `"model"`/`"id"` string
  field in the preceding metadata slice rather than the upstream's
  two-phase field search; the timestamp header parser accepts RFC3339 and
  naive local `[YYYY-MM-DD HH:MM:SS.mmm]` stamps.
- **crush**: cost-only rows (no token buckets) carry a `session-total`
  model; OMC estimates nothing for them — message counts and activity
  land, and the model stays unpriced by design. The registry resolves
  `{path, data_dir}` entries (data_dir relative to the project path) from
  the config/share/library candidates; day buckets group by UTC day via
  SQL rather than the upstream's configurable bucket timezone.
- **openclaw**: the sessions.json index dispatcher is not ported —
  discovery walks every agents root for `*.jsonl` and
  `openclaw-agent.sqlite` directly (exact suffix match); the SQLite path
  resolves model/provider from model_change and model-snapshot events
  only (no session_windows join). The codex-mirror turn scoping in dedup
  keys is not ported.
- **workbuddy**: JSONL transcript lane only; the upstream's
  workbuddy.db SQLite fallback is not ported (deviation recorded — the
  JSONL transcripts cover the CLI lane).
- **codebuff/freebuff**: both scan the manicode channel trees
  (manicode, manicode-dev, manicode-staging); freebuff gates on the
  base2-free root agent, skips any chat carrying authoritative provider
  usage (that chat belongs to the codebuff scan), and estimates tokens
  from message text (~4 chars/token). The settings.json model fallback
  and the RunState model hints are carried only when present in the
  row's own metadata.
- **jcode**: journal rows replace snapshot rows through the shared
  max-per-key merge rather than the upstream's explicit known-keys pass;
  the cache split applies when the record omits
  cache_creation_input_tokens or the cache read exceeds the plain input.
- **commandcode**: usage rows split the cache buckets out of inputTokens
  (the upstream's inclusive-input rule with the 1e12 ceiling); all-zero
  rows without a positive reported cost are dropped (an explicit $0 only
  survives with real buckets); the char-based estimator applies only when
  a row has no usage object; the config-file model fallback reads
  `<dir>/.config.json`. The /rewind active-branch filter and the legacy
  flat schema (top-level role/content) are not ported — orphaned-branch
  usage still counts and legacy rows are dropped.
- **mcode**: roots are the Tokens headless capture dirs
  (`~/.config/tokens/headless/mcode`, macOS Application Support sibling);
  the parser ignores a stream whose exec.result never arrives (upstream
  behavior), so streams captured by tools other than Tokens are invisible
  by design.
- **cherrystudio**: partial rows (uuid-only or message-id-only) adopt a
  complete row's key when the same uuid appears with full identity — a
  single-pass simplification of the upstream's connect-any-order
  reconciliation.
- **senpi**: OmO task children roots are not followed (children appear
  under their own root when present).
- **dsh**: session logs discovered under both spellings
  (`session.jsonl`, `session.v<N>.jsonl`, each optionally `.zstd`); the
  served model resolves responseModel → model from
  `data.message.source`, the provider from the same source, and the
  request-header config as fallback — the replayState responseModel
  pointer path is flattened.
- **reasonix**: the dedup key carries the line ordinal alongside
  requests/total (upstream shape).
- **kimchi**: the scoped namespace keys on the entry id (the upstream
  scoped branch), not the response id.

### Batch 3 deviations

- **kiro**: the CLI snapshot lane (header json + sibling <stem>.jsonl,
  message_ids turn join, explicit token counts, context-usage estimate) and
  the IDE sess_<uuid> structured-payload lane are ported; the globalStorage
  chat-artifact lane and the conversations_v2 SQLite lane are not. The IDE
  flat-format aggregation (role/content rows without a payload wrapper) and
  turn_end/contextUsage records are not ported — a flat file yields nothing.
  Token counts are estimates from context-usage × context window and text
  length, as upstream (Kiro writes no per-turn tokens).
- **copilot-vscode**: the row-log projection is ported with APPEND
  semantics for kind-0 and top-level kind-2 (upstream extend); the kind-1
  patch walker handles object keys and the requests index but does not
  materialize array-path parents inside a request (rare in the observed
  shapes). timestamp-0 rows are dropped (upstream emits them).
- **cline**: roots cover the VS Code task storages (Code, Code - OSS,
  macOS, vscode-server) and the CLI home fallback ~/.cline/data/sessions;
  the env-root overrides are not ported. The CLI manifest's metadata.title
  is unused (no session-title surface).
- **roo/kilo family**: per-entry modelInfo wins over the file-level
  <environment_details> LAST-tag heuristic (slug preferred over name),
  exactly as upstream.
- **hermes**: profile discovery walks <root>/profiles/*/state.db directly;
  the upstream's settings-driven data-isolation rule is simplified to "all
  profiles under the root". The per-model ORDER BY keeps the session's
  own model first so it carries the message count.
- **unsloth**: chat and API lanes ported with the total-aware split
  (input = max(total, prompt+completion) − completion − cache shares, with
  upstream clamps); API event status filtering is not applied.
- **zcode**: JSONL and modern model_usage SQLite lanes ported with the
  inclusive-total split and usage field aliases; the LEGACY SQLite schema
  variant is not (legacy databases yield nothing from the db lane).
  resolveZcodeTimestamp clamps the back-anchor at zero instead of falling
  back to completed_at when the duration overshoots.
- **octofriend/synthetic**: the gateway re-attribution filter runs in the
  scan emit path for every client (upstream runs it in the aggregation
  pipeline); the octofriend ledger reads $XDG_DATA_HOME/octofriend/
  sqlite.db. The token_usage fallback table is not ported.
- **workbuddy.db**: aggregate session_usage lane ported (cumulative "used"
  as input, seconds→ms normalization); discovered only under ~/.workbuddy.
- **micode/kilo**: the shared OpenCode-schema engine ports the strict
  (kilo) and lenient (micode) cache dialects; the incremental high-water
  resumption is not ported — OMC's per-file watermark (mtime/size/parser
  version) bounds rescan cost and whole-database reads are idempotent
  through the dedup view.
- **durations**: several Batch 3 lanes do not record durations
  (kilo/micode completed−created, kiro turn spans, zcode sqlite duration)
  — OMC's activity buckets derive from timestamps, and sessionization
  treats a bare point as a zero-length span.


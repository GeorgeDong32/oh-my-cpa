# ADR 0103: models.dev is the fallback price source

- Status: Accepted
- Date: 2026-10-21
- Amends: ADR 0030 (price source), whose decision 1 named OpenRouter the only automatic source

## Context

OpenRouter is the primary price source because it lists a model once under its
maker's namespace and publishes long-context and time-of-day overrides. But a
newly launched model often has no paid OpenRouter listing at all — MiniMax
M3.1 Flash shipped with only a `:free` variant there, while models.dev already
carried the vendor's own rates. Until OpenRouter catches up, such a model is
unpriced and every cost it accrues is invisible; the matcher correctly refuses
to guess, and the operator's only path is hand-entered rates.

models.dev was removed as a source in ADR 0030 for three reasons: relay and
aggregator duplication, flat rates that could not express tiers, and a missing
cache rate that billed cached tokens as free. Two of the three have since
dissolved upstream: the catalogue now publishes context tiers in the same
shape this project evaluates, and the missing-cache-rate rule (an omission
means the prompt rate, never zero) already lives in the OpenRouter decoder and
carries over. The duplication problem remains — but only the maker's own
namespace is admissible, and the matcher already ranks namespaces by an
ordered authority table.

The source also must not disturb what already works: the console's model
picker reads the stored OpenRouter snapshot and works offline, and links are
picks from that picker.

## Decision

1. **models.dev is the fallback price source, consulted only when OpenRouter
   prices nothing for a model.** OpenRouter stays primary: its match wins on
   every sync, and a row it priced is never overwritten by the fallback. The
   fallback fills gaps: an unpriced model, or a `modelsdev` row it can
   refresh. The fetch rides the existing sync loop, is cached in memory for
   fifteen minutes, and is never persisted — the stored upstream snapshot
   remains the OpenRouter list the picker reads.
2. **Only the maker's own namespace is admissible.** `firstPartyModelsDevProviders`
   maps each model family to the models.dev namespaces that publish it
   first-hand (`minimax`, not `minimax-cn`; `alibaba` for Qwen; `volcengine`
   for Doubao; `zai` before `zhipuai`, mirroring `z-ai` before `thudm`).
   Entries under relays, aggregators, regional arms and plan namespaces are
   skipped, as are entries in a first-party namespace hosting another maker's
   model. Families without a trustworthy first-hand namespace (Llama, whose
   models.dev namespace mixes hosting-variant ids) have no fallback.
3. **The fallback carries the same price structure.** Rates are USD per 1M
   tokens already; context tiers map onto price tiers; an omitted cache rate
   resolves to the prompt rate, never zero. Same-slug candidates rank by the
   models.dev namespace order, then slug length, then id.
4. **A fallback row is honest about its origin and temporary by design.** It
   carries source `modelsdev` and derives mode `auto`; the first sync that
   finds an OpenRouter match replaces it with the OpenRouter rate. A pin is
   never reinterpreted: a link names an OpenRouter model, and a failed lookup
   stays a failed lookup. Suggestions and the model picker remain
   OpenRouter-only.
5. **A fallback failure must not fail a sync.** models.dev being unreachable
   prices nothing new and records no primary-source error; the sync that
   OpenRouter answered still succeeds.

## Consequences

- A newly launched model is priced within one sync of appearing on models.dev,
  instead of never — without operator action, and without weakening the
  reseller protections that motivated ADR 0030.
- `modelsdev` rows written before the OpenRouter switch are no longer a
  special legacy case: they are fallback rows, refreshed by the fallback and
  upgraded by the primary, and the console labels the source plainly
  "models.dev".
- The automatic match consults a second external endpoint. The endpoint is a
  compile-time constant like OpenRouter's, needs no key, and its failure is
  non-fatal; offline deployments keep their last good prices exactly as
  before.
- `SyncResult` counts fallback matches for logs; the sync-state row stays
  keyed to OpenRouter, because models.dev has no schedule of its own — it
  rides the primary sync.

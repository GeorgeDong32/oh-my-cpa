# ADR 0104: Manual price backfill for stored unpriced requests

- Status: Accepted
- Date: 2026-10-21
- Revises: migration 019's blanket ban ("Existing requests have no recoverable historical rate. Never backfill them.")

## Context

A request stored as `unpriced` or `legacy_unpriced` has usage telemetry and no
cost. The blanket ban conflated two situations:

- The rate that governed the request is genuinely unknowable — OMC has no
  record, and guessing would manufacture an authoritative-looking number.
  These must stay visibly unpriced.
- The rate was simply not recorded yet: the model had no price row when the
  requests ran, and the operator has since recorded one. Preview models are
  the recurring case — MiniMax billed `MiniMax-M3.1-Flash-Preview` at its
  published flat rate from day one, while OMC could not know it until the
  model appeared upstream. For these, refusing to price history keeps a real
  cost invisible forever.

The distinction is not derivable from the data; only the operator can draw
it. So the decision moves from "never" to "only ever by explicit operator
direction".

## Decision

1. **Backfill is a manual, per-model, audited operator action.** `POST
   /v1/pricing/models/{model}/backfill` fills a model's stored unpriced
   requests from the model's current price version and is recorded in the
   audit log. There is no automatic backfill anywhere: a price being added
   never reprices history on its own.
2. **The fill applies the recorded rate at each request's own timestamp.**
   Service tiers and time-of-day windows evaluate exactly as the ingest lock
   would have, and the channel multiplier effective per request applies. The
   operator's statement is "this rate governed these requests"; OMC does not
   reconstruct rates that changed in between — a request billed under a price
   the operator never recorded stays estimable only outside the ledger.
3. **A backfilled cost is marked for life.** It lands in the terminal
   `backfilled` status and never masquerades as a cost locked at ingestion.
   The breakdown view says the cost was computed after the fact. Migration
   044 rebuilds `usage_events` to widen the status CHECK and reshapes the
   immutability trigger: the one allowed transition is an unpriced row being
   filled, and after that the row is as immutable as every other.
4. **Rows carrying a cost are never touched.** The fill targets rows with no
   cost and no version only, so the operation is idempotent and cannot reprice
   a locked request. A row the Quote cannot price (no effective version at its
   timestamp, invalid rates) stays unpriced and is reported as skipped rather
   than guessed.
5. **Derived aggregates re-fold.** The usage facts are derived caches, so the
   backfill clears them and zeroes the aggregation watermark in the same
   transaction; the rollup worker rebuilds the dashboards from the ledger.

## Consequences

- The Preview-class gap closes: an operator records the vendor rate for a
  newly named model and can then price its entire stored history in one
  audited step, instead of hand-editing or leaving thousands of billable
  requests permanently invisible in cost totals.
- `legacy_unpriced` rows that predate all price knowledge remain visible as
  unknown unless the operator explicitly asserts a rate for them — the status
  still distinguishes "the operator filled this" from "the pipeline locked
  this".
- Backfilled costs are estimates by nature (the rate is today's, applied to
  the past). The console labels them; cost totals blend them with locked
  costs, and the breakdown's recompute check covers them like any other row.

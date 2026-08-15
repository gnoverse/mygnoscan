# Dashboards Batch 2a — Blocks Design

Batch 2a of the chain-analytics dashboards: a `blocks` table and the three charts that need it.
Extends [`2026-08-13-chain-analytics-dashboards-design.md`](2026-08-13-chain-analytics-dashboards-design.md),
whose §5 roadmap row for batch 2 this document replaces and corrects.

---

## 1. What the roadmap got wrong

The parent spec's §5 lists eight charts under "batch 2 — `blocks` table". Only three actually need it:

| Needs the blocks table | Needs no schema change (deferred to batch 2b) |
|---|---|
| Block-time histogram | Activity heatmap (hour × day-of-week) |
| Blocks per bucket | New addresses (first-seen) |
| Proposer distribution | Gas-per-tx histogram |
| | Function-call heatmap |
| | DAU/WAU/MAU |

The right-hand five read `block_time`, already denormalized onto `calls`, `transactions`, `bank_sends`
and `packages`. **Batch 2a is the left column plus the §10.2 config widening; batch 2b is the right column.**

The parent spec also implied this batch carries migration risk. It does not. `AGENTS.md` flags the
`packages_new` table-*rebuild* path (`db.go:133`) used to add columns to existing tables; adding new
tables via `CREATE TABLE IF NOT EXISTS` in `initSchema` never touches it. No existing table is named
`blocks` or `proposers`.

## 2. Measurements this design rests on

Taken against the live `gnoland1` indexer and RPC on 2026-08-13:

| Quantity | Value |
|---|---|
| Chain height | 3,275,126 |
| Median block time | 4.34 s (range 3.69–10.11 s observed) |
| History spanned | ~165 days |
| Blocks containing transactions | **0 of ~5,000 sampled** across four eras (heights ~500k, 1.5M, 2.5M, 3.2M) |
| Fetch cost | 5,000 blocks in 500 ms / 684 KB → full history ≈ 656 queries ≈ 5–6 min |
| Distinct proposers in a 2,000-block sample | 5 |
| Current database size | 16 MB (network `sapphire`, 10,029 txs) |

Mainnet blocks are effectively all empty. The three charts this batch builds are the only reason to
store them.

## 3. Decisions

| Decision | Choice | Rationale |
|---|---|---|
| Storage shape | **Raw rows**, one per block | A rollup is derivable from raw rows with a local `GROUP BY`; raw rows are *not* derivable from a rollup without re-fetching 3.3M blocks. Raw preserves every future option for ~390 MB. |
| Row compaction | Intern the proposer address | Saves ~36 bytes/row ≈ 119 MB for one small table and one join in a single query, and gives the moniker a natural home. `time` stays RFC3339 TEXT so the existing `strftime` bucketing helpers work unchanged. |
| Backfill depth | Full history, automatic | Matches `backfillBlockTimes`/`backfillTransactions`, which already run automatically in `SyncAll`. ~5–6 min of fetching, resumable. |
| Backfill direction | **Backward from tip**, with a separate head sync | Filling oldest-first would leave the default 90d window empty until backfill nearly finished. |
| Batch scope | Split; blocks half first | Keeps each batch ~7–8 tasks and starts the backfill early, so data accumulates while 2b is built. |
| Tx vocabulary | Name each precisely | Batch 1 counts *messages*; `num_txs` counts *transactions*. Both on screen unlabelled would read as a contradiction. |

**Rejected: hourly rollup.** Three tables (~123k rows, 5–15 MB vs ~400 MB) would serve all three charts
at the finest grain anything requests, since batch 1 already maps `24h` to hourly buckets. Rejected on
three costs: aggregate counters (`SET blocks = blocks + ?`) **double-count on re-sync**, and this repo's
sync loop logs-and-continues per item by design, so partial re-runs are expected rather than exceptional;
histogram bin edges would freeze at ingest; and computing deltas at ingest reintroduces a page-boundary
bug that query-time `LAG()` avoids. Revisit as a purely local migration if size becomes a problem —
note the cost is *per network*, so pointing this at five chains (~2 GB) is the case where rollup wins.

## 4. Schema

Both tables are additive, created in `initSchema`.

```sql
CREATE TABLE IF NOT EXISTS proposers (
  id      INTEGER PRIMARY KEY,        -- rowid alias
  network TEXT NOT NULL,
  address TEXT NOT NULL,
  UNIQUE (network, address)
);

CREATE TABLE IF NOT EXISTS blocks (
  network     TEXT NOT NULL DEFAULT 'gnoland1',
  height      INTEGER NOT NULL,
  time        TEXT NOT NULL,          -- RFC3339, matching every other table
  proposer_id INTEGER,
  num_txs     INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (network, height)
);
CREATE INDEX IF NOT EXISTS idx_blocks_time ON blocks(network, time);
```

`proposers` rows carry their own `network` and the unique key is `(network, address)`, so an interned id
can never span chains. Queries still filter `blocks.network` explicitly rather than relying on that — the
network invariant is worth being redundant about.

Estimated size at 3.3M blocks: ~188 MB table + ~66 MB primary key + ~152 MB time index ≈ **406 MB per
network**.

**On comparing `time` as text.** Range filters (`time >= ? AND time < ?`) rely on RFC3339 sorting
lexicographically, which holds for fixed-offset UTC timestamps and is already how every other table in
this schema filters by `block_time`. One known edge: the indexer returns nanosecond precision
(`2026-08-13T13:06:11.884040229Z`), and a timestamp with no fractional part sorts *after* one with a
fractional part in the same second, because `.` < `Z`. Only blocks within the same second could
misorder, and observed block times are ~4.3 s apart, so this cannot affect bucketing or delta ordering
in practice.

## 5. Syncer

A new `syncBlocks(ctx)` in `SyncAll`, doing two things per pass:

1. **Head sync** — `MAX(height)+1` → tip. Small; keeps live data current.
2. **Backward backfill** — `MIN(height)-1` downward in 5,000-block pages until height 1.

Both cursors derive from the table itself rather than stored state, per the existing convention. Because
the table always holds a **contiguous height range**, neither cursor can be fooled by gaps — this
contiguity is the invariant that makes the two-cursor scheme safe, and nothing may insert blocks outside
the range.

On an empty table, seed with a head page at the tip, then backfill downward.

**Bounded per pass.** The sync loop polls every 30 s; running a 5–6 minute backfill to completion inside
one pass would stall package, call and msg-run syncing behind it. `syncBlocks` processes a budget of
20 pages (~100k blocks, ~10 s) then returns, resuming next tick — full backfill over ~33 passes,
about 16 minutes, with everything else syncing normally throughout. The page size (5,000) and per-pass
budget (20) are package-level constants, not configuration.

**Backfill termination.** Backfill is done when it reaches height 1, or when a page returns no blocks —
the indexer may prune early history, and without the second condition the backfill would retry a range
that will never return rows.

Proposer interning uses an in-memory `map[address]int` per network, loaded once at startup, so the
common case costs no query per block.

Page failures log and return, resuming next pass, per the sync loop's existing convention. This is the
one place that convention applies; query paths still return errors up.

## 6. Block-time deltas

Computed **at query time** with a window function, not stored:

```sql
SELECT (julianday(time) - julianday(LAG(time) OVER (ORDER BY height))) * 86400.0 AS delta
FROM blocks WHERE network = ? AND time >= ? AND time < ?
```

Verified against this build (SQLite **3.51.3** via `modernc.org/sqlite`): window functions are available
and `julianday` preserves sub-second precision — a probe returned 4.341 / 3.875 / 10.11 s, matching the
deltas measured on mainnet.

Query-time computation avoids both a stored column and the page-boundary problem that ingest-time
computation would create. The window's first block has a `NULL` delta and is excluded.

## 7. API

Mode-A series keeps batch 1's `/api/timeseries/*` convention. The two mode-B aggregates are not time
series, so they sit under `/api/blocks/*` — distinct route patterns from the existing live-indexer
`GET /api/blocks`.

| Endpoint | Returns | Mode | Default window |
|---|---|---|---|
| `/api/timeseries/blocks` | `[{time, blocks, txs}]` | A | global picker |
| `/api/blocks/time-histogram` | `[{bin, blocks}]` | B | 7d |
| `/api/blocks/proposers` | `[{address, blocks}]` | B | 90d, top-N |
| `/api/blocks/coverage` | `{min_time, max_time, complete}` | — | — |

`complete` is true once the backfill has terminated per §5 — reached height 1, or hit a page with no
blocks. It is stored as a `sync_state` flag set by the syncer, not inferred from `MIN(height) <= 1`,
because a pruned indexer never yields height 1 and the inferred version would report incomplete forever.

Histogram bins, from the measured distribution, in seconds:
`<4.0, 4.0–4.5, 4.5–5.0, 5.0–5.5, 5.5–6.0, 6.0–7.0, 7.0–8.0, 8.0–10.0, ≥10.0`

Binning happens in SQL via `CASE` over the `LAG()` delta with `GROUP BY bin`, so the response is nine
rows regardless of window size.

**Moniker resolution stays in the frontend** (corrected 2026-08-13). An earlier draft of this spec said
the endpoint would reuse "the existing `r/gnops/valopers` resolution from `HandleValidators`". That
resolution does not exist server-side: `HandleValidators` (`api.go:843`) returns raw
`r/gnops/valopers` transactions, and the parsing into an address→moniker map lives in the frontend's
`loadValMonikers()` / `_valMonikers` (`frontend/index.html:325`).

So `/api/blocks/proposers` returns bare addresses and stays pure SQLite — no live indexer call, which
also avoids the `AGENTS.md` gotcha about endpoints that query the indexer on every request. The chart
labels bars via the existing `_valMonikers` map, falling back to a truncated address. Note
`loadValMonikers()` is currently only invoked by the blocks view, so the proposer chart must call it
itself.

### Empty-bucket semantics (parent spec §10.1)

| Endpoint | Empty bucket | Reason |
|---|---|---|
| `/api/timeseries/blocks` | `0` | Counts; zero blocks in a bucket is a true zero |
| `/api/blocks/time-histogram` | n/a | Fixed nine-bin output; no empty-bucket concept |
| `/api/blocks/proposers` | n/a | Top-N list; no empty-bucket concept |

Batch 1's `trimLeadingEmptyRows` covers the pre-backfill leading edge.

## 8. Config widening (parent spec §10.2)

Lands **first**, while there are six chart call sites rather than nine.

- **`opt(rows, ctx)`** — `ctx = { window, granularity }`. The existing six charts ignore the second
  argument, so this is backward compatible. Granularity is **inferred from the returned `time` format**
  (`2026-08` monthly, `2026-08-13` daily, `2026-08-13T13` hourly, `2026-W33` weekly) rather than
  duplicating the §8 window table client-side, so it cannot drift from the server.
- **`controls?`** — `dashCard` renders a control container when the chart declares one, and
  `renderDashChart` calls `chart.controls(container, rerender, chart.state)` after rendering.
  Charts gain an optional `state` object. First user is the proposer top-N selector, so the slot ships
  exercised rather than speculative.

## 9. Charts

Three new cards in the existing Chain Pulse section, taking it to seven.

| Chart | Viz | Mode | Window |
|---|---|---|---|
| Block-time distribution | histogram bar | B | 7d (local override) |
| Blocks and transactions per bucket | line + bar | A | global |
| Block proposers | horizontal bar + top-N control | B | 90d |

The three block cards show "history backfilling — showing X to Y" while `/api/blocks/coverage` reports
incomplete.

**Retitle:** batch 1's headline chart becomes "messages per bucket, by type", and its `ⓘ` tooltip states
that one transaction carries one or more messages, so the message count is always ≥ the transaction
count. Without this, the two numbers on the same page read as a contradiction.

## 10. Error handling

Inherited from batch 1: per-card isolation, empty distinguished from failed, `db.go` query paths
returning errors up rather than swallowing them.

New: backfill progress is **signalled, not silent**. A 90d window showing three days of blocks is
indistinguishable from a chain that was down — the coverage endpoint and card note make the difference
visible.

## 11. Testing

Go, table-driven against a temp SQLite file:

- **Proposer interning** — same address returns the same id; the same address on two networks returns
  different ids; repeated upsert is idempotent
- **Block upsert idempotency** — syncing the same page twice leaves the row count unchanged
- **Cursor derivation** — `MAX`/`MIN` against an empty table, a single row, and a contiguous range
- **Bounded work per pass** — `syncBlocks` stops at its page budget and resumes where it left off
- **Histogram binning** — known block times producing known bin counts, including deltas landing exactly
  on bin edges, and confirming the window's first block (`NULL` delta) is excluded rather than counted
  as zero
- **Network isolation** — two networks holding blocks at the *same heights*, asserting each aggregate
  returns only the requested network's rows. This is the invariant `AGENTS.md` says goes wrong silently
- **Coverage** — empty, partial and complete states

Frontend keeps the repo's no-test-runner constraint, verified by driving the running app. Per parent
spec §10.3 the checks assert on data pulled from `getOption()`, not just a clean console: histogram bins
summing to the block count in the window, proposer bars summing to total blocks, and granularity
inference returning the right value for each window.

## 12. Scope boundary

**In:** config widening, both tables, `syncBlocks`, four endpoints, three charts, the batch 1 retitle.

**Out — batch 2b:** activity heatmap, new addresses (first-seen), gas-per-tx histogram, function-call
heatmap, DAU/WAU/MAU. Two parent-spec open questions belong to 2b and are deliberately not resolved
here: what counts as an "active" address (do bank-send receivers count? do failed txs?), and whether
first-seen derivation needs a materialized table for performance.

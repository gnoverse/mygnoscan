# Dashboards Batch 4 — Network Graphs Design

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this design's plan task-by-task.

**Supersedes:** the batch-4 roadmap row in
[`2026-08-13-chain-analytics-dashboards-design.md`](2026-08-13-chain-analytics-dashboards-design.md) §5, and
resolves its §9 open question on the caller-graph renderer.

**Goal:** Persist day-collapsed `transfer_edges` and `caller_edges` rollup tables from data the syncer already
has locally (`bank_sends`, `calls`), add two scope-aware graph endpoints that do window / top-N / ego / edge
collapse in SQL, and ship a new "network" dashboard section with three charts: a value-transfer force graph
with click-to-focus ego drill-down, a token-flow sankey, and a caller→realm WebGL graph.

**Tech Stack:** Go (stdlib + `modernc.org/sqlite`), vanilla JS with the repo's `el()` helper, ECharts 5 +
echarts-gl from CDN (both already in use / already the committed choice below). No bundler, no build step.

---

## 1. Why this differs from every prior batch's sync pass

Every sync pass so far (`syncCalls`, `syncMsgRuns`, `syncBlocks`, `syncStorageEvents`) walks the tx-indexer and
writes raw per-tx/per-event rows keyed by `(network, tx_hash, ...)`. `syncStorageEvents` in particular re-fetches
transactions from the indexer independently, because a shared walk with `syncCalls` would already be at the
chain tip on a synced DB and never see storage events at all.

Batch 4 does not need to touch the indexer. `bank_sends` (`BankMsgSend`) and `calls` (`MsgCall` caller/pkg_path)
are already fully populated locally by `syncCalls`. The rollup passes read **local SQLite**, not the indexer:

```sql
SELECT from_address, to_address, date(block_time) as day,
       SUM(CAST(REPLACE(REPLACE(amount, 'ugnot', ''), '"', '') AS INTEGER)), COUNT(*), MAX(block_height)
FROM bank_sends
WHERE network = ? AND block_height > ? AND success = 1
GROUP BY from_address, to_address, day
```

This is the first sync pass in the codebase to persist an aggregated result rather than raw rows.

---

## 2. Schema

Both tables are `CREATE TABLE IF NOT EXISTS` — brand new tables, so no `ALTER`/rebuild migration risk (contrast
with the `packages_new` rebuild-and-swap path `AGENTS.md` flags as high-risk for adding columns to existing
tables).

```sql
CREATE TABLE IF NOT EXISTS transfer_edges (
    network      TEXT NOT NULL DEFAULT 'gnoland1',
    from_address TEXT NOT NULL,
    to_address   TEXT NOT NULL,
    day          TEXT NOT NULL,               -- 'YYYY-MM-DD', from date(block_time)
    total_value  INTEGER NOT NULL DEFAULT 0,  -- ugnot, already parsed out of bank_sends' decorated string
    tx_count     INTEGER NOT NULL DEFAULT 0,
    last_height  INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (network, from_address, to_address, day)
);
CREATE INDEX IF NOT EXISTS idx_transfer_edges_day  ON transfer_edges(network, day, total_value);
CREATE INDEX IF NOT EXISTS idx_transfer_edges_from ON transfer_edges(network, from_address);
CREATE INDEX IF NOT EXISTS idx_transfer_edges_to   ON transfer_edges(network, to_address);

CREATE TABLE IF NOT EXISTS caller_edges (
    network     TEXT NOT NULL DEFAULT 'gnoland1',
    caller      TEXT NOT NULL,
    pkg_path    TEXT NOT NULL,
    day         TEXT NOT NULL,
    calls       INTEGER NOT NULL DEFAULT 0,
    last_height INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (network, caller, pkg_path, day)
);
CREATE INDEX IF NOT EXISTS idx_caller_edges_day ON caller_edges(network, day, calls);
CREATE INDEX IF NOT EXISTS idx_caller_edges_pkg ON caller_edges(network, pkg_path);
```

> **Correction (during planning):** the original version of this section omitted `last_height` from both
> tables. §3 commits to a block-height cursor derived from "`MAX(block_height)` already rolled into its own
> table" — but a rollup row spans every source row that landed in that `(pair, day)` bucket, potentially across
> many source block heights, so the table needs a column carrying the highest source height any given rollup
> row has absorbed. `last_height` is that column; the cursor is `MAX(last_height)` across all of a network's
> rows. It is not part of the primary key — it is metadata about how far a row's aggregation has progressed, not
> part of its identity.

`bank_sends.amount` is stored as the raw chain string (e.g. `"1000000ugnot"`), not a bare integer — a naive
`CAST(amount AS INTEGER)` truncates to 0. The rollup pass parses it the same way `amountExpr` does elsewhere in
`db.go` (`CAST(REPLACE(REPLACE(amount, 'ugnot', ''), '"', '') AS INTEGER)`) once, at write time, so
`transfer_edges.total_value` stores a plain integer and every downstream graph read is a bare `SUM`.

Unlike those existing reads, the rollup pass filters source rows to `success = 1`: a failed `BankMsgSend`
moved no value on chain, so including it in a *value-transfer graph* — as opposed to a raw send-count stat —
would misrepresent real fund flow. (`GetBankStats`'s `TotalVolume` does not filter on success; that is a
pre-existing, separate concern, out of scope here.)

---

## 3. Sync pass

`syncTransferEdges` and `syncCallerEdges`, added to `SyncAll` after `syncCalls`/`syncMsgRuns`. Each:

1. Derives its cursor from `MAX(block_height)` already rolled into its own table for that network (new queries
   `TransferEdgesLastHeight` / `CallerEdgesLastHeight`), matching the block-height-cursor convention every other
   pass uses — **not** a day cursor, so the in-progress current day is safely reprocessed every run rather than
   permanently excluded until it fully elapses.
2. Reads `bank_sends` / `calls` locally with `block_height > cursor AND success = 1`, grouped by
   `(from_address, to_address, day)` / `(caller, pkg_path, day)`.
3. Upserts with `INSERT ... ON CONFLICT (...) DO UPDATE SET total_value = total_value + excluded.total_value,
   tx_count = tx_count + excluded.tx_count` (transfer) / `calls = calls + excluded.calls` (caller) — additive,
   so a row for a day that's revisited (because the cursor only admits genuinely new source rows) accumulates
   correctly rather than overwriting.
4. Network-scoped throughout; errors propagate up per query-path convention (this is a local aggregate query,
   not the per-item indexer walk that logs-and-continues).

---

## 4. API endpoints

### `GET /api/graph/transfers`

Backs both the value-transfer force graph and the token-flow sankey — one query, two renderers, since both are
fundamentally "top-N addresses by volume in a window."

| Param | Default | Meaning |
|---|---|---|
| `window` | `30d` | `24h · 7d · 30d · 90d · 1y · all`, reuses the existing window resolver → `day >=` cutoff |
| `topN` | 100 | cap 1000. Ignored when `ego` is set |
| `min_value` | none | optional floor to drop dust edges before ranking |
| `ego` | none | address; switches to neighborhood mode |
| `hops` | 1 | only meaningful with `ego`; batch 4 ships 1-hop only |

**Top-N mode:** rank addresses by total in+out volume within the window (summed across `transfer_edges` days),
keep the top N, return only edges where **both** endpoints are in that set — this is the parallel-edge collapse;
same-pair edges across multiple days in the window are summed at read time.

**Ego mode:** ignore `topN`/ranking; return edges where `from_address = ego OR to_address = ego` within the
window, plus the resulting neighbor node list. Bounded per query regardless of chain size, per the existing
recap's framing of ego drill-down as the safe default for arbitrary scale.

```json
{ "nodes": [{"address": "g1...", "volume": "12345"}],
  "edges": [{"from": "g1...", "to": "g1...", "value": "6789", "tx_count": 3}] }
```

The sankey reshapes this same response into `{source, target, value}` links client-side — no separate endpoint.

### `GET /api/graph/callers`

Same shape over `caller_edges`. `window`, `topN` (default 200 — realm counts run smaller than address counts),
`min_value`. No `ego` in this first pass — the roadmap's headline drill-down interaction is the transfer graph's;
extending ego to callers later is a straightforward addition behind the same endpoint if ever needed, not
required here.

```json
{ "nodes": [{"id": "g1...", "type": "caller", "calls": 12}, {"id": "gno.land/r/...", "type": "realm", "calls": 340}],
  "edges": [{"caller": "g1...", "pkg_path": "gno.land/r/...", "calls": 5}] }
```

Both endpoints are network-scoped throughout and return errors up rather than swallowing zero-results, per
`AGENTS.md`.

---

## 5. Frontend — new "network" dashboard section

A fourth section (`pulse`, `economics`, `realms`, **`network`**) — these are graph/relationship views, distinct
in kind from the existing time-series/treemap/heatmap sections, matching the master spec's framing of batch 4 as
"network graphs."

1. **Value-transfer force graph** — ECharts canvas `graph` series (not GL — top-N/ego views stay ≤1-2k nodes,
   comfortably inside canvas force-layout's realistic ceiling). Node size ~ volume, edge width ~ value. Click a
   node → re-fetch `?ego=<addr>&hops=1&window=<current>`, swap the graph in place, with a back control to
   return to the top-N view.
2. **Token-flow sankey** — ECharts `sankey` series, fed by the same `/api/graph/transfers` top-N response,
   reshaped into sender→receiver links. Shares the section's window picker with the force graph — window is a
   mode-B range filter here (changes which edges are counted, not how many nodes come back), per the existing
   time-window contract.
3. **Caller→realm graph** — ECharts-gl `graphGL` series (**decision, resolving the master spec's §9 open
   question**: echarts-gl over sigma.js+graphology — no new dependency, WebGL force layout covers realistic
   per-network caller-graph sizes well within its ~100k-node ceiling, and the scoped top-N/ego-style views this
   design defaults to keep most requests far below that anyway). Top-N/window scoped; no ego drill-down in this
   pass.

All three follow the existing per-card contract: empty ≠ failure, one bad card doesn't blank the section, guard
`typeof echarts === 'undefined'`, dispose-on-section-leave for the WebGL instance (called out as needed "until
the WebGL graph in batch 4" in the master spec — this is where that lifecycle work lands).

---

## 6. Testing & verification

**Go**, table-driven, real temp SQLite, no mocks:

- Rollup correctness: multiple `bank_sends`/`calls` rows on the same day/pair collapse into one edge with
  correct `SUM`/`COUNT`; rows spanning a day boundary land in separate buckets
- Cursor resume: partial-day upsert accumulates correctly across two sync runs without double-counting
- Network isolation: edges never leak across `network`
- API: `topN` ranking + both-endpoints-in-set filtering, `ego`/`hops=1` neighborhood correctness, `window`
  cutoff, `min_value` floor
- Route precedence for both new endpoints, matching the existing `api_test.go` pattern

**Frontend:** no JS test infra, unchanged. Verify live:

```
gofmt -l .
go vet ./...
go test ./...
```

then run the binary, `curl` both endpoints, and drive the running app to confirm the force graph renders and
click-to-focus works, the sankey renders, the WebGL caller graph renders, and the window picker re-scopes all
three cards.

---

## 7. Doc updates

- Tick batch 4 in the master spec's roadmap table (§5)
- Resolve §9's open renderer question: **echarts-gl graphGL**, decided in §5 above
- Update `docs/api.md` with the two new endpoint shapes

---

## 8. Scope boundaries (explicitly out for this batch)

- No ego drill-down on the caller graph (straightforward future addition, not required now)
- No 2-hop ego (1-hop only; a chain size-safe default, per the recap's ego-drill-down framing)
- No precomputed/server-side layout (both renderers stay within their client-side layout ceilings given the
  top-N/ego scoping this design enforces)
- No community/cluster detection (recap's "aggregated meta-graph" scoping strategy — not needed while top-N and
  ego alone keep every response bounded)

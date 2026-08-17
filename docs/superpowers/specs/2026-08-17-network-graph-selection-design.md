# Network Graph Selection — Design

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this design's plan task-by-task.

**Supersedes:** the interaction model shipped in
[`2026-08-17-dashboards-batch-4-network-design.md`](2026-08-17-dashboards-batch-4-network-design.md) for all
three "network" section charts (`transfer-graph`, `token-flow-sankey`, `caller-graph`). That design shipped a
static top-N/top-300 view per chart with no way to find or pick a specific address, and (for `transfer-graph`)
a single-address ego drill-down as the only interaction. User feedback: the shipped charts are not usable for
"find this specific address/contract and see how it moves value or who it calls."

**Goal:** Replace the static per-chart views with a shared table+search+selection UI: rank addresses/realms by
volume or calls, search for any one even outside the top ranking, select up to 20, and view the induced
subgraph among exactly the selected set — with selections removable via a tag/chip, both from the table and
from the graph view.

**Architecture:** Two new backend query capabilities (ranking with sent/received or caller/realm breakdown,
and bounded substring search) plus a new "selected-set" mode on both existing graph endpoints. One new
frontend component, `renderEntityPicker`, shared by all three charts, replacing their current `fetch`/`opt`
dispatch with a `render`-owned card that toggles between a table view and each chart's existing graph
rendering.

**Tech Stack:** Go (stdlib + `modernc.org/sqlite`), vanilla JS with the repo's `el()` helper, ECharts 5 +
echarts-gl (unchanged from batch 4). No bundler, no build step.

---

## 1. Backend: ranking, search, and selected-set queries

### 1.1 Ranking (table data)

```go
type TransferRankRow struct {
    Address  string `json:"address"`
    Sent     int64  `json:"sent"`
    Received int64  `json:"received"`
    Volume   int64  `json:"volume"` // sent + received
}

// GetTransferRanking ranks addresses by total volume (sent+received) in the
// window, splitting sent/received for the table. search, when non-empty,
// replaces the topN cutoff with a bounded substring match so an address
// outside the top ranking can still be found.
func (d *DB) GetTransferRanking(network string, days, topN int, search string) ([]TransferRankRow, error)
```

```sql
-- topN mode (search == "")
SELECT addr, COALESCE(SUM(sent),0), COALESCE(SUM(received),0)
FROM (
    SELECT from_address AS addr, total_value AS sent, 0 AS received FROM transfer_edges WHERE network=? AND day>=?
    UNION ALL
    SELECT to_address AS addr, 0 AS sent, total_value AS received FROM transfer_edges WHERE network=? AND day>=?
)
GROUP BY addr ORDER BY SUM(sent)+SUM(received) DESC LIMIT ?

-- search mode (search != "")
-- same subquery, but: WHERE addr LIKE '%'||?||'%' ... LIMIT 50 (fixed cap, not topN)
```

`CallerRankRow`/`GetCallerRanking` mirror this, unioning `caller_edges.caller` (type `"caller"`) and
`caller_edges.pkg_path` (type `"realm"`) instead of `from_address`/`to_address` — this is a genuine
capability gap fix, not just a refactor: today's `GetCallerGraph` topN mode ranks callers only, so a
realm that isn't called by a top-100 caller can never appear even though it may itself be heavily called.

```go
type CallerRankRow struct {
    ID    string `json:"id"`
    Type  string `json:"type"` // "caller" | "realm"
    Calls int    `json:"calls"`
}

func (d *DB) GetCallerRanking(network string, days, topN int, search string) ([]CallerRankRow, error)
```

Both search paths are parameterized (`LIKE '%'||?||'%'`) — the search term is bound, never concatenated into
the query text — and both cap results at 50 regardless of match count, so a broad or empty search term can't
return an unbounded payload.

### 1.2 Selected-set graph

New `addresses` param on `GET /api/graph/transfers` (comma-separated, server-side capped at 20 — extras
silently dropped, matching the existing topN-clamping convention rather than a 400 error) bypasses the
ranking step: the given list becomes the "order" set fed directly into the same both-endpoints-in-set
edge-collapse query `topNTransferGraph` already runs, extracted into a shared helper:

```go
// edgesAmongSet is the query topNTransferGraph and selectedTransferGraph both
// run — the only difference between them is how `order` (the address list)
// was obtained: a ranking query, or the caller's explicit selection.
func (d *DB) edgesAmongSet(network, start string, order []string, minValue int64) ([]GraphEdge, error)

func (d *DB) selectedTransferGraph(network, start string, addresses []string, minValue int64) (TransferGraph, error)
```

Node volumes for the selected set reuse `GetTransferRanking`'s subquery, restricted to `addr IN (...)` instead
of `ORDER BY ... LIMIT`.

Same shape for `GET /api/graph/callers?entities=...` (a mixed list of caller addresses and realm paths, also
capped at 20): edges require **both** endpoints in the selected set. Because `caller_edges` is bipartite
(callers only ever connect to realms, never to each other), a selection of callers-only or realms-only
produces zero edges by construction — a useful selection needs at least one of each. This is a direct
consequence of the data shape, not a new restriction, and is called out in the chart's `why` text.

```go
func (d *DB) selectedCallerGraph(network, start string, entities []string, minCalls int) (CallerGraph, error)
```

### 1.3 Routes

No new routes — all of the above are new params on the existing `GET /api/graph/transfers` and
`GET /api/graph/callers`, plus two new endpoints for the ranking table itself (search and rank share one
endpoint, keyed by whether `search` is present):

- `GET /api/graph/transfers/ranking?network=&window=&topN=&search=`
- `GET /api/graph/callers/ranking?network=&window=&topN=&search=`

---

## 2. Frontend: a shared entity-picker component

The existing chart-config shape (`fetch` → `opt` → `renderDashChart`'s `echarts.init`/`setOption`) only knows
how to drive one ECharts instance per card. A table+search+selection UI needs its own DOM and its own view
state (table vs. graph), so rather than bending the ECharts pipeline to fit, chart configs gain a second,
parallel shape:

- Existing non-network charts keep `fetch`/`opt` — completely unchanged, still dispatched through
  `renderDashChart`'s existing path.
- `renderDashChart` gains a branch at its top: if `chart.render` is present, call
  `chart.render(host, controlsBar, chart.state)` instead of running `fetch`/`opt`/`setOption`. The three
  network charts switch to this shape.

One shared helper, `renderEntityPicker(host, opts)`, implements the table+search+selection+"see graph" flow
generically for all three charts:

```js
// opts:
//   fetchRank(topN)         -> Promise<RankRow[]>
//   fetchSearch(query)      -> Promise<RankRow[]>
//   fetchGraph(selectedIds) -> Promise<GraphResponse>
//   columns                 -> [{key, label, format?}] for the table
//   idOf(row)                -> string, the row's selectable identity
//   renderGraph(host, graphData, selectedIds, onDeselect) -> void
//     (reuses each chart's EXISTING opt-building code, called from here
//     instead of from renderDashChart's setOption path)
function renderEntityPicker(host, opts) { /* ... */ }
```

`columns` per chart:
- `transfer-graph` / `token-flow-sankey` (both use the transfer ranking): address, sent, received, volume
- `caller-graph`: id, type (caller/realm badge), calls

`renderGraph` is exactly today's `opt` function for that chart, unchanged in its ECharts-building logic —
only its caller changes, from `renderDashChart`'s `setOption` to `renderEntityPicker`'s graph-view branch.

State per card, held on `chart.state` (same lifetime/reset rules as today's `ego`):

```js
{ view: 'table' | 'graph', selected: Set<string>, searchQuery: string }
```

`view` and `selected` both reset to their defaults on a network switch — the same fix already applied to
`transfer-graph`'s `ego` field after the final batch-4 review, extended to cover the new state shape (a stale
cross-network selection is the identical bug in a new shape).

Table rows toggle selection on click (checkbox-style visual state). A tag row above the table — and,
unchanged in the graph view — lists every selected entity as a chip with a `×`; clicking one deselects
without needing to return to the table. "See graph" is disabled at zero selections; "back to table" returns
to the table view without discarding the selection.

---

## 3. Data flow and request sequencing

Per card, per render:

1. **Table view (default).** `fetchRank(topN=100)` on mount. Typing in the search box debounces (~300ms)
   into `fetchSearch(query)`, replacing the displayed rows with search matches — `selected` is untouched, so
   a match picked via search still shows in the tag row after the search box is cleared, even though the
   unfiltered top-100 view might not include it.
2. **Selection** is pure client-side state until "see graph" is clicked — no request per checkbox click.
3. **"See graph"** fires `fetchGraph(selected)` once, sets `view = 'graph'`, and renders via `renderGraph`.
4. **Removing a tag in graph view** re-fetches immediately with the shrunk selection; if it reaches zero,
   fall back to the table view automatically rather than rendering an empty graph.
5. **Window-picker changes** re-run whichever fetch matches the current view (rank/search in table view,
   graph in graph view) — identical to how every other chart already reacts to the window picker.

---

## 4. Error handling and testing

Error/empty states follow the existing per-card contract (`empty ≠ failure`, per the master spec's §6): a
failed rank/search/graph fetch shows the existing "could not load this chart" message — the graph sub-view
reuses the `setOption`-throw guard added during batch 4's final review; the table's own fetches get a
parallel try/catch. An empty search result shows "no matches" inline in the table rather than an empty table
with no explanation.

**Go tests** (table-driven, real temp SQLite, per convention):
- Ranking: sent/received split correctness; caller/realm combined ranking includes a realm with no top-100
  caller ever calling it (the capability gap this design fixes); search is bounded to 50 results and is
  parameterized (a `search` value containing `%`/`_` must not behave as a SQL wildcard escape bypass beyond
  what `LIKE` already implies — verify literal `%`/`_` in a real address doesn't do something unexpected)
- Selected-set graph: both-endpoints-in-set edge collapse from an explicit list (not a ranking); the 20-item
  server-side cap; network scoping throughout
- Route/param tests for the two new ranking endpoints

**Frontend:** no test infra (unchanged) — manual verification drives search, multi-select, tag removal in
both table and graph views, "see graph"/"back to table" round-trips, and the network-switch reset for both
`view` and `selected`.

---

## 5. Scope boundaries (explicitly out for this pass)

- Search is a raw substring match on the address/realm-path string — no fuzzy matching, no moniker/name
  resolution (monikers exist for validators elsewhere in the app but not for arbitrary addresses here).
- No persistence of selection across a page reload — `chart.state` has the same in-memory lifetime as
  today's `ego` field.
- Caller-graph selection stays "both endpoints in set" (bipartite) rather than a looser "either endpoint"
  mode — if that proves confusing in practice, it's a small, isolated follow-up.
- No change to the batch-4 backend's existing `topN`/`ego`/`min_value`/`min_calls` params or response
  shapes — this design is additive (new params, new endpoints), not a breaking change to what already ships.

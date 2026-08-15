# Chain Data Recap — tx-indexer → gnorigin → mygnoscan

> Goal: understand everything happening on the Gno chain (trends, growth, evolution) and turn it into
> charts. This doc maps **what data exists**, **where it comes from**, **what we already show**, and a
> **prioritized list of charts** to build — including ones whose data isn't indexed yet.

---

## 1. The three systems and how they connect

```
┌─────────────────────┐     JSON-RPC (TM2)      ┌──────────────────────┐    GraphQL / JSON-RPC   ┌──────────────────────┐
│  gnorigin            │  blocks + block_results │  tx-indexer          │  getBlocks /            │  mygnoscan           │
│  (the gno.land chain)│ ───────────────────────▶│  (PebbleDB + GraphQL)│  getTransactions ──────▶│  (Go binary + SQLite │
│  source of truth     │   poll every 1s         │  raw chain mirror    │  subscriptions          │   + vanilla JS UI)   │
└─────────────────────┘                          └──────────────────────┘                         └──────────────────────┘
```

- **gnorigin** = the chain. Produces blocks; each block carries transactions; each tx carries messages,
  gas accounting, and emitted events. This is the *source of truth*.
- **tx-indexer** = a faithful, queryable **mirror of raw chain data**. It does NOT compute analytics — it
  stores blocks and tx-results verbatim (Amino-encoded) in PebbleDB and exposes them via GraphQL with
  range/message/event filters + live subscriptions. Think "indexed firehose," not "data warehouse."
- **mygnoscan** = our explorer. It **pulls from tx-indexer's GraphQL**, decodes messages/events, and
  **derives analytics into its own SQLite** (background syncer, 30s poll). All charts/dashboards are
  computed here. This is where every new visualization lives.

**Key architectural consequence:** anything the indexer already stores (blocks, txs, messages, events,
storage events) is chartable *today* — it's just an aggregation in mygnoscan's `db.go`. Anything that is
**chain state rather than tx history** (account balances, current validator set + voting power, live
proposal tallies, total supply) is **NOT in the indexer** and needs either (a) a direct chain RPC/ABCI
query, or (b) replaying the relevant `GnoEvent`s to reconstruct state.

---

## 2. What tx-indexer actually indexes (the available data surface)

### Blocks (`FilterBlock`, ordered by height)
`hash, height, version, chain_id, time, num_txs, total_txs, app_version,
proposer_address_raw, last_block_hash, last_commit_hash, validators_hash,
next_validators_hash, consensus_hash, app_hash, last_results_hash, txs[]`

### Transactions (`FilterTransaction`, ordered by heightAndIndex)
`index, hash, success, block_height, gas_wanted, gas_used, gas_fee{amount,denom},
content_raw, memo, messages[], response{log,info,error,data,events[]}`

Filters available: height range, index range, **gas_wanted/gas_used range**, hash, success bool,
per-message filters (route/type/params), and **event filters**.

### Message types (union `MessageValue`)
| Message | Route/Type | Key fields |
|---|---|---|
| `BankMsgSend` | bank/send | `from_address, to_address, amount` |
| `MsgCall` | vm/exec | `caller, pkg_path, func, args[], send, max_deposit` |
| `MsgAddPackage` | vm/add_package | `creator, package{name,path,files[]}, send, max_deposit` |
| `MsgRun` | vm/run | `caller, package, send, max_deposit` |

### Events (union `Event`)
| Event | Key fields | Source on chain |
|---|---|---|
| `GnoEvent` | `type, pkg_path, attrs[{key,value}]` | `chain.Emit(type, k, v...)` in any realm |
| `StorageDepositEvent` | `bytes_delta, fee_delta{Coin}, pkg_path` | VM storage lock at msg end |
| `StorageUnlockEvent` | `bytes_delta, fee_refund{Coin}, pkg_path` | VM storage unlock/refund |

**Storage economics constants (from chain `vm/params.go`):**
`StoragePrice = 100 ugnot/byte` (1 GNOT per 10 KB), `DefaultDeposit = 600 GNOT`, `1 GNOT = 1,000,000 ugnot`.

---

## 3. What mygnoscan already derives & shows

Stack: Go binary + embedded vanilla-JS SPA (`frontend/index.html`, single file), local **SQLite** cache
(`modernc.org/sqlite v1.48.1`), syncer that pulls tx-indexer GraphQL into SQLite, REST `/api/*` layer.
Two charting libraries are loaded from CDN: **D3.js v7** (dependency graph only) and **Chart.js 4**
(time-series line/bar charts on the `analytics`, `gas` and `sanity` views).

SQLite tables: `packages, package_files, dependencies, calls, msg_runs, bank_sends, transactions,
sync_state`.

**Already built** (this section previously described an earlier state — corrected 2026-08-13):
- `transactions` carries `gas_used`, `gas_wanted`, `gas_fee`, `success`, `block_time` — gas/fee persistence
  is done.
- `block_time` is denormalized onto `calls`, `packages`, `msg_runs`, `bank_sends` and `transactions`, with
  supporting indexes, so rows are bucketable by chain time without a separate `blocks` table.
- Eight `/api/timeseries/*` endpoints exist (`transactions`, `packages`, `callers`, `gas`, `storage`,
  `storage/realms`, `health`, `active-addresses`), taking `?days=&granularity=` rather than the `?window=`
  contract in §8.

Views/pages present: `home, realms, packages, txs, blocks, accounts, tokens, validators, govdao, gas,
analytics, events` + detail views `block/{h}, realm/{path} (7 tabs), tx/{hash}, address/{addr}`.

What's actually visualized:
- **D3 v7 force-directed dependency graph** — the *only* real chart, on `realm/{path}?tab=graph`
  (import/dependent network, pan/zoom/drag, circles=packages, diamonds=realms).
- **Everything else is tables + stat cards**, not charts:
  - **Analytics** = 6 sortable *tables* (top realms by calls, top packages by dependents, top callers,
    top deployers, most-imported paths, recently-deployed). No time-series, no bars.
  - **Gas** = stat cards + 2 tables (top realms by gas, top txs by gas). No time-series chart.
  - **Storage economics** = a *tab on each realm detail* showing a table of StorageDeposit/Unlock events
    + totals. **There is no global storage dashboard and no storage-growth chart.**
  - **GovDAO / Validators / Tokens / Events** = tables/stat cards. Validators come from the
    `r/gnops/valopers` moniker registry (for block-proposer labels), **not** `r/sys/validators` power.
- Live feed via SSE polling (3s) for new blocks/txs.

**What does NOT exist yet:**
- **No `blocks` table.** `proposer_address_raw` and `num_txs` are fetched by `indexer.go` but never stored —
  so no proposer distribution, no blocks/day, and no consecutive-block deltas for a block-time histogram.
- **No `storage_events` table.** Note that `GetStorageTimeSeries` is misleadingly named: it sums
  `LENGTH(pf.body)`, i.e. deployed *source-code* bytes, **not** `StorageDepositEvent.bytes_delta`. Storage
  economics (P1 #11–14) is greenfield.
- **No `events` table** — no `GnoEvent` persistence, so every realm-specific dashboard and the event-type
  treemap are blocked.
- **No pre-aggregated edge tables** (`transfer_edges`, `caller_edges`) → no network graphs.
- **No treemap, heatmap, sankey, or WebGL graph** — Chart.js cannot draw them; the caller/transfer networks
  need ECharts (+ echarts-gl) or a dedicated WebGL renderer (sigma.js v3 / deck.gl / cosmograph).
- Balances (beyond `/api/bankstats` total supply), validator voting power, and live proposal tallies are
  **not sourced**.

> Implementation plan for closing these gaps:
> [`docs/superpowers/specs/2026-08-13-chain-analytics-dashboards-design.md`](docs/superpowers/specs/2026-08-13-chain-analytics-dashboards-design.md).

---

## 4. Prioritized chart backlog

Legend — **Data status:** ✅ available in indexer/SQLite today · 🟡 derivable but needs new indexing
(event replay / new aggregation) · 🔴 needs chain RPC/ABCI or new indexer support.
**Viz** maps to the primitives you listed (time-series / treemap / heatmap / sankey / histogram / WebGL graph).

### P0 — Chain pulse & growth (the "is the chain healthy and growing" story)
| # | Chart | Viz | Data | Why it matters |
|---|---|---|---|---|
| 1 | Transactions per day/hour, split by message type | time-series (stacked area) | ✅ | Headline activity + composition (calls vs deploys vs sends vs runs) |
| 2 | Cumulative total_txs & cumulative addresses | time-series | ✅ | Long-run growth curve; the "up and to the right" |
| 3 | New addresses per day (first-seen) | time-series + histogram | ✅ derive first-seen | Real user acquisition, not just activity |
| 4 | Active addresses (DAU/WAU/MAU) | time-series | ✅ | Retention/engagement; ratio = stickiness |
| 5 | Block time distribution & blocks/day | histogram + time-series | ✅ (block.time deltas) | Consensus health, throughput |
| 6 | Success vs failed tx rate over time | time-series | ✅ | Reliability; spikes = buggy realm or attack |
| 7 | Activity heatmap (hour-of-day × day-of-week) | heatmap | ✅ | When the chain/community is alive; timezone of users |

### P1 — Economics: gas, fees, storage
| # | Chart | Viz | Data | Why |
|---|---|---|---|---|
| 8 | Gas used vs gas wanted over time + efficiency % | time-series | ✅ | Capacity & estimator quality |
| 9 | Gas-used per tx distribution | histogram | ✅ | Spot heavy/cheap operations, outliers |
| 10 | Fees collected (ugnot) per day; cumulative | time-series | ✅ (gas_fee) | Fee economy |
| 11 | **Storage bytes growth** (global + cumulative) | time-series (area) | ✅ (bytes_delta) | How fast chain state grows = long-term cost |
| 12 | **Top storage consumers** (bytes per realm) | **treemap** | ✅ (group StorageDeposit by pkg_path) | Who's eating state; treemap = instant "who's biggest" |
| 13 | Cost per byte / deposit locked vs refunded | time-series | ✅ (fee_delta vs fee_refund) | Storage-deposit economics in action |
| 14 | Net storage delta per realm (growth vs cleanup) | diverging bar | ✅ | Realms that grow vs realms that prune |

### P2 — Network graphs (the WebGL flagship)
| # | Chart | Viz | Data | Why |
|---|---|---|---|---|
| 15 | **Caller → realm interaction graph** | **WebGL graph (100k+)** | ✅ (MsgCall caller→pkg_path) | The map of who uses what; cluster detection |
| 16 | **Value-transfer network** (address→address, edge weight = ugnot) | **WebGL graph** | ✅ (BankMsgSend) | Money flow, whales, exchange-like hubs |
| 17 | **Token-flow sankey** (top senders → receivers) | **sankey** | ✅ (BankMsgSend) | Where GNOT concentrates/flows |
| 18 | Realm dependency / import network | WebGL graph (upgrade from D3) | ✅ (dependencies) | Composability map; foundational packages |
| 19 | Caller-overlap / co-usage graph (addresses that use same realms) | WebGL graph | 🟡 (derive) | Community/segment detection |

### P3 — Realm-specific dashboards
These mostly need **GnoEvent replay** (🟡) — the events are indexed, but mygnoscan must parse the
specific `type`+`attrs` per realm and accumulate state. A few need chain RPC for *current* state (🔴).

**`r/sys/users` — registration**
| Chart | Viz | Data |
|---|---|---|
| Registrations per day + cumulative | time-series | 🟡 GnoEvent "Registered" on r/sys/users |
| Active vs deleted users | stacked area | 🟡 "Registered"/"Deleted" events |
| Username length / collision distribution | histogram | 🟡 |
| Total registered users (current) | stat + RPC cross-check | 🔴 render/ResolveName |

**`r/sys/validators` — validator set**
| Chart | Viz | Data |
|---|---|---|
| Validator set size over time | time-series | 🟡 `ValidatorAdded`/`ValidatorRemoved` events |
| **Voting-power distribution** (per validator) | treemap / bar | 🔴 needs GetValidators() RPC |
| **Proposer distribution** (blocks proposed per validator) | bar + heatmap | ✅ block.proposer_address_raw — *already indexed!* |
| Validator churn (joins/leaves) | timeline | 🟡 events |

**`r/gov/dao` — governance**
| Chart | Viz | Data |
|---|---|---|
| Proposals created per period; by status (accepted/denied/voting) | time-series + stacked bar | 🟡 GnoEvents on r/gov/dao |
| Vote distribution per proposal (Yes/No/Abstain by tier) | stacked bar | 🟡 events / 🔴 live tally via render |
| Voter participation rate (members voting / eligible) | time-series | 🔴 needs member tiers (memberstore) |
| Member count per tier (T1/T2/T3) & power | bar | 🔴 |

**`r/gnoland/boards2` — content**
| Chart | Viz | Data |
|---|---|---|
| Threads & replies created per day | time-series | 🟡 GnoEvents on boards2 |
| Activity per board | treemap / bar | 🟡 |
| Top posters | bar | 🟡 |
| Moderation: flags / bans / hidden posts | time-series + bar | 🟡 |
| Reply-depth distribution | histogram | 🟡 |

### P4 — Aspirational / needs new sourcing
| # | Chart | Viz | Data |
|---|---|---|---|
| 20 | Wealth distribution / Lorenz curve of balances | histogram + line | 🔴 balances via bank RPC |
| 21 | Total supply & circulating over time | time-series | 🔴 RPC / genesis + mints |
| 22 | Top holders leaderboard & concentration (Gini) | bar + stat | 🔴 |
| 23 | Realm "retention": cohorts of realms by first-deploy month, still-active | cohort heatmap | 🟡 |
| 24 | Function-level call heatmap per realm (func × time) | heatmap | ✅ (MsgCall.func) |
| 25 | Event-type frequency across chain | treemap / bar | ✅ (GnoEvent.type) |
| 26 | Memo usage / tagging trends | bar | ✅ (tx.memo) |
| 27 | Gas-price trend (from getGasPrice) | time-series | ✅ indexer getGasPrice |

---

## 5. Recommended build order

1. **Add the viz toolkit** — Chart.js covers line/bar but not treemap/heatmap/sankey/WebGL, so pull in a
   library covering the rest in one dep (**ECharts** is the strongest single-dep fit and keeps the
   no-build-step / CDN model), plus a **WebGL graph renderer** (**echarts-gl**, or **sigma.js v3 +
   graphology** for 100k+ nodes). The existing D3 dependency graph and Chart.js views can stay as-is.
   This step is a prerequisite for almost everything below.
   **Also extend the existing `/api/timeseries/*` endpoints** from `?days=&granularity=` to the `?window=`
   + adaptive-bucket contract in §8.
2. **Ship the P0 pulse dashboard** — it's all available data; biggest "understand the chain" payoff per effort.
3. **Storage treemap + economics (P1 #11–14)** — directly serves your "storage economics view" goal; data is live.
4. **WebGL caller & transfer graphs (P2)** — the flagship differentiator; data exists, only the renderer is new.
5. **Proposer distribution (P3 validators)** — quick win, `proposer_address_raw` is already indexed.
6. **Event-replay realm dashboards (P3 users/govdao/boards2)** — build a generic `GnoEvent` decoder in
   the syncer, then each realm dashboard is an aggregation.
7. **Balance/supply/validator-power (P3/P4)** — last, because they require new chain-RPC sourcing beyond the indexer.

## 6. The one structural gap to plan for

mygnoscan currently only knows **transaction history**, not **chain state**. To fully "understand
everything happening on the chain" you'll eventually want a second data path: a thin **chain-RPC client**
(ABCI `Query`) for current balances, validator set + voting power, and live governance tallies — or a
**state-reconstruction layer** that folds `GnoEvent`s into materialized state tables. Decide which before
building P3/P4, because it shapes the syncer.

---

## 7. Scaling the network graphs — where the logic belongs

Network graphs do **not** scale the way time-series do, and the difference dictates where the work lives.
A chain with millions of addresses can never be drawn as one graph — not because of rendering alone, but
because **graph layout is the real bottleneck** (force-directed is O(N log N) at best; you cannot lay out
millions of nodes live in a browser tab). And a million-node hairball communicates nothing to a human anyway.
So every network view must be **scoped and/or aggregated before it reaches the client**. (Time-series,
treemaps, heatmaps and histograms are the opposite: they're inherently aggregated by a `GROUP BY` and return
~N buckets regardless of chain size — push those down to SQL and they scale forever.)

### The two ceilings (interactive, in-browser)
| Renderer | Realistic ceiling | When to use |
|---|---|---|
| ECharts canvas `graph` (force) — *value-transfer chart in the POC* | ~1–2k nodes | Scoped/curated views (top-N, ego neighborhood) |
| ECharts-gl `graphGL` (WebGL) — *caller graph in the POC* | ~10k–100k nodes | Zoomed-out overview; layout on GPU |
| sigma.js v3 + graphology / cosmograph / deck.gl | ~100k–1M | Only with **precomputed layout** streamed as coordinates |

**Layout, not rendering, is the limit.** Past ~100k nodes you must precompute positions off the request path
(a worker or a server-side `graphology-layout-forceatlas2` pass) and ship coordinates, not raw edges.

### The four scoping strategies (all backed by server-side aggregation)
1. **Ego / neighborhood** — one address, 1–2 hops out. Bounded per query → scales to *any* chain size. Should be the **default drill-down** (the POC's click-to-focus demonstrates this).
2. **Top-N + time window** — "top 500 addresses by volume, last 30d." The window + threshold prune in SQL *before* anything serializes to the browser (the POC's Window / Top-N controls demonstrate this).
3. **Aggregated meta-graph** — nodes = realms/clusters/communities, edges = rolled-up flow. Community detection runs server-side; client gets a few hundred super-nodes.
4. **Level-of-detail** — zoomed out shows clusters; expanding a cluster fetches its members on demand.

### Where each piece is built (the division of labor)
- **tx-indexer** — stays a raw mirror. Do **not** put graph logic here. It already exposes the edges you need
  (`BankMsgSend` for value transfer, `MsgCall caller→pkg_path` for caller graphs) with height/time filters.
- **mygnoscan syncer + SQLite** — this is where scoping lives. Maintain **pre-aggregated edge tables**:
  `transfer_edges(network, from, to, day, total_value, tx_count)` (collapse parallel transfers by day) and
  `caller_edges(network, caller, pkg_path, day, calls)`. Add the indexes the queries need
  (`(network, day, total_value)`, `(network, from)`, `(network, to)`). This is the same "materialize on
  ingest" pattern the existing `calls`/`bank_sends` tables already use — just rolled up.
- **mygnoscan API** — graph endpoints take scope params and return the **already-pruned** graph, never raw edges:
  - `GET /api/graph/transfers?window=30d&topN=100&min_value=…` → top-N subgraph
  - `GET /api/graph/transfers?ego=<addr>&hops=1&window=30d` → ego neighborhood
  - `GET /api/graph/callers?window=30d&topN=…` (same shape for the caller graph)
  Do the `top-N`, `time window`, `ego`, and parallel-edge collapse **in SQL** (the POC's `buildTransfer()` is
  exactly this logic, just running client-side over synthetic data — in production it's a `WHERE day >= ? …
  ORDER BY total_value DESC LIMIT ?`).
- **mygnoscan frontend** — renders whatever the API returns. Canvas force for scoped views (≤2k nodes);
  WebGL (graphGL/sigma) only for the zoomed-out overview, and only once layout cost is handled.

**Rule of thumb:** if a chart's node/row count grows with chain size, the trimming happens in SQL on the
server; the browser only ever receives a bounded, human-sized result. The POC shows the *interaction model*
(window / top-N / click-to-focus) with the filtering done client-side for demo convenience — the production
move is to lift that exact filtering into the syncer's aggregate tables and the API's SQL.

---

## 8. Time-window contract — every chart is time-bounded (yes, even time-series)

Every chart in this spec carries a time window. But the window does **three different jobs** depending on
the chart, and the design must declare which — otherwise a single uniform control silently does the wrong
thing (returns 5 years of hourly points, filters a snapshot that has no range, or makes a cumulative number
ambiguous).

### Global control + per-chart override
A single **dashboard-level time picker** (`24h · 7d · 30d · 90d · 1y · All · custom`) drives every chart so
the whole page reads the same period. Any chart may **override locally** when its nature demands it (the
function-call heatmap is pinned to ~14d because daily columns past ~30 become unreadable). **Default window:
90d** — a good balance for a young chain.

### The three window modes (determined by chart type)
| Mode | What the window does | Applies to |
|---|---|---|
| **A — Range + adaptive bucket** | picks the range **and** the bucket size (see table) | all time-series: tx/day, gas, fees, success-rate, DAU/WAU/MAU, new addresses, registrations, threads/replies, storage growth |
| **B — Range filter** | decides which events are *counted*; output size fixed by top-N / bins / categories, not by the window | network graphs, gas-per-tx histogram, block-time histogram, storage-consumers treemap, event-type treemap, function-call heatmap, proposer distribution, activity heatmap |
| **C — As-of snapshot** | a single *point in time* (default = now); a past date "rewinds" the snapshot. **No range.** | current-state 🔴 charts: voting-power treemap, live proposal tally, wealth Lorenz, total-supply *level*, validator-set size |

### Why time-series still need a window — adaptive bucket granularity
Time-series payloads are small at any chain size (it's a `GROUP BY bucket`), so the window is **not** about
pruning — it's about **readability and bucket size**. The window must set *both* the range and the bucket, so
you never return millions of empty hourly slots or an unreadably dense line:

| Window | Bucket | ~points |
|---|---|---|
| 24h | 10m–1h | ~24–144 |
| 7d | 1h | 168 |
| 30d | 6h–1d | ~30–120 |
| 90d (default) | 1d | 90 |
| 1y | 1w | ~52 |
| All | 1mo | N months |

### The cumulative subtlety
Cumulative charts (cumulative tx, cumulative addresses, cumulative storage, total supply *level*) are
from-genesis by nature. Decide **per chart**, and state it, so the number isn't ambiguous:
- **(a) Full curve, window zooms** — line runs genesis→now; the window only zooms the x-axis. *Default for cumulative charts.*
- **(b) Windowed delta** — show only what was *added* in the period ("new this 30d"). This is a **separate series**, not the same chart with a window applied.

### API shape (consistent across all endpoints)
- Time-series (mode A): `?from=&to=` **or** `?window=90d`, plus `bucket=` (or derive bucket from window server-side per the table).
- Filtered aggregates (mode B): `?window=` + the chart's scope params (`topN`, `bins`, `ego`, `min_value` per §7).
- Snapshots (mode C): `?as_of=<height|timestamp>` (default = latest). Range params are ignored.

### Per-chart defaults worth pinning (exceptions to the 90d/zoom defaults)
| Chart | Mode | Default window |
|---|---|---|
| Block-time distribution | B | 7d (recent consensus health) |
| Activity heatmap (hour × dow) | B | 90d (needs volume to fill the grid) |
| Function-call heatmap | B | 14d (fixed — daily columns) |
| Cumulative tx / addresses / storage | A | All (full curve, window zooms x-axis) |
| Total supply *level*, voting power, wealth Lorenz, live tally | C | now (as-of) |
| Everything else | A or B | 90d |

**Takeaway for the design file:** annotate each chart with its **window mode (A/B/C)** and **default window** —
not just "has a time window." The mode is what tells the backend whether to bucket, filter, or snapshot.

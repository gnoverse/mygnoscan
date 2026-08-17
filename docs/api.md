# HTTP API

All responses are JSON. Errors are `{"error": "..."}` with a non-200 status.

## Common parameters

**`network`** — accepted by most endpoints. A configured network ID restricts
results to that network. `all`, or omitting it, applies **no filter**, so results
span every configured network.

Careful with the unfiltered case on anything address-related: a balance or RPC
lookup has to resolve against exactly one network, and with no filter it uses the
first configured network that has an RPC URL, regardless of where the address is
actually active.

**`limit`, `offset`** — pagination, where supported. Noted per endpoint below.

**Time-series parameters**, on every `/api/timeseries/*` endpoint:

| parameter | values | default |
|---|---|---|
| `window` | `24h`, `7d`, `30d`, `90d`, `1y`, `all` | none |
| `days` | 1–365, clamped (`monthly` is exempt, and capped at 3650 instead) | `30` |
| `granularity` | `hourly`, `daily`, `weekly`, `monthly` | `daily` |

`window` is the current contract and resolves to a `(days, granularity)` pair —
`24h`→(1, hourly), `7d`→(7, hourly), `30d`→(30, daily), `90d`→(90, daily),
`1y`→(365, weekly). `days` and `granularity` predate it, still work, and win when
supplied alongside it.

`all` is the exception: it is sized against the network's real history rather
than a fixed pair, because a fixed one is wrong for any chain younger than its
bucket. The server takes the earliest indexed timestamp for that network — or,
with `network=all` (or omitted), the minimum across every configured network —
and picks the bucket by keeping each candidate granularity's point count under
a target: hourly up to ~250 points (~10 days), daily up to ~550 points (~18
months), weekly up to ~260 points (~5 years), monthly beyond that. This keeps
`all` returning a readable series on a week-old devnet and on a multi-year
chain alike, without the bucket boundaries needing to be re-tuned as any one
chain ages. The resulting span is clamped to the same 3650-day ceiling as
`days`. A network with nothing indexed falls back to (3650, monthly).
Supplying `days` or `granularity` opts out of the sizing entirely; an
unparseable `days` value does not count as supplying it, since
`parseTimeseriesParams` treats it as absent too.

## Meta

| endpoint | description |
|---|---|
| `GET /api/version` | build info: `git_hash`, `build_time` |
| `GET /api/networks` | configured network IDs — the fastest way to confirm which chains an instance is actually serving |

## Packages and realms

| endpoint | description |
|---|---|
| `GET /api/realms` | list realms. `limit`, `offset` |
| `GET /api/packages` | list all packages, realms and pure packages. `limit`, `offset` |
| `GET /api/realm/{path...}` | detail for one package: metadata, source files, imports, dependents, callers, MsgRun references |
| `GET /api/deps/{path...}` | dependency graph as `{path: [imports]}`. `dir=dependents` reverses direction |
| `GET /api/storage/{path...}` | storage events for a package |
| `GET /api/storage/consumers` | `{pkg_path, deposited, released, net}` per realm over the window, ordered by `net` descending. Accepts `window`/`days` (no `granularity`). `topN` caps the row count (default 20, capped at 100). `released` is negative and nothing is floored at zero, so a realm that pruned more than it stored has a negative `net` |
| `GET /api/events/{path...}` | events emitted by a package |

## Transactions and blocks

| endpoint | description |
|---|---|
| `GET /api/txs` | recent transactions. Queried live from the indexer |
| `GET /api/tx/{hash}` | one transaction: messages, events, errors |
| `GET /api/blocks` | recent blocks. `limit` |
| `GET /api/block/{height}` | one block and its transactions |
| `GET /api/allevents` | recent events across all packages |

## Addresses and accounts

| endpoint | description |
|---|---|
| `GET /api/address/{addr}` | activity for an address: calls, deploys, runs, sends, and balance |
| `GET /api/accounts` | most active accounts. **Returns the top 100 by total activity, with no pagination or sort controls** |

## Aggregates

| endpoint | description |
|---|---|
| `GET /api/stats` | totals: transactions, calls, deploys, msg_runs, sends, realms, packages, unique callers, latest block |
| `GET /api/analytics` | leaderboards and aggregate breakdowns |
| `GET /api/gas` | gas usage, by realm |
| `GET /api/bankstats` | transfer volume statistics |
| `GET /api/tokens` | detected token packages |
| `GET /api/validators` | validator set |
| `GET /api/govdao` | GovDAO activity |
| `GET /api/sanity/overview` | internal consistency counters, for debugging a sync |

## Time series

All accept `days` and `granularity`.

| endpoint | description |
|---|---|
| `GET /api/timeseries/transactions` | transaction counts |
| `GET /api/timeseries/packages` | deployments |
| `GET /api/timeseries/callers` | unique callers |
| `GET /api/timeseries/gas` | gas consumption |
| `GET /api/timeseries/active-addresses` | active addresses |
| `GET /api/timeseries/health` | chain health indicators |
| `GET /api/timeseries/storage` | `{time, deposited, released, net}` per bucket, from `storage_events` (chain-state bytes, not source-code bytes). `released` is negative and nothing is floored at zero, so a bucket that pruned more than it stored nets negative. `realm=<path>` scopes it to one realm |
| `GET /api/timeseries/storage/realms` | realms that have storage events in the window, for populating a selector |
| `GET /api/timeseries/blocks` | blocks and transactions per bucket. **Single-network** |
| `GET /api/timeseries/new-addresses` | addresses seen on-chain for the first time, bucketed by that first appearance. First-seen is derived over all indexed history, so widening the window never relabels an old address as new |
| `GET /api/timeseries/active-rolling` | `dau`, `wau`, `mau` — distinct active addresses over trailing 1/7/30-day windows. **Always daily**: `granularity` is ignored, because the three windows are day-defined. A request shorter than 7 days is widened to 7, and capped at 365 regardless of `window`/`days`/`granularity` |

`storage` and `calls/function-heatmap` are the only endpoints that accept a
`realm` parameter; there is no general per-realm activity time series.
`timeseries/storage`, `timeseries/storage/realms`, and `storage/consumers` are
all **single-network** in the dashboard — none of them are meaningfully
aggregatable across chains, since `pkg_path` is only unique within a network.

An **active address** is one that authored a message — a caller, a package
creator, a `MsgRun` caller, or a bank-send sender. Bank-send *receivers* do not
count, and failed messages do count. Every endpoint that says "address" here
means that.

Counts over `calls` / `packages` / `msg_runs` / `bank_sends` are counts of
**messages**, not transactions: one transaction carries one or more messages.
Only `transactions` rows count transactions, and `gas/per-tx-histogram` is the
one endpoint reading them.

## Distributions and heatmaps

Range-filtered rather than bucketed: the window decides what is *counted*, but
the response shape is fixed — a 24x7 grid, a fixed bin set, a functions x days
grid. Empty cells come back as an explicit `0`.

| endpoint | description |
|---|---|
| `GET /api/activity/heatmap` | messages per (hour-of-day, day-of-week) in UTC. Always 168 cells. `dow` is 0=Monday..6=Sunday, not SQLite's Sunday-first `%w`. Accepts `days`/`window`, snapped down to a whole number of weeks (floor 7 days) so every weekday column gets an equal number of occurrences |
| `GET /api/gas/per-tx-histogram` | transactions binned by `gas_used`, in half-decade log steps. Rows with `gas_used = 0` are excluded as never-recorded rather than counted as free. Accepts `days`/`window` |
| `GET /api/calls/realms` | realms called in the last 14 days, busiest first. Accepts `limit` (default 30, capped at 100) |
| `GET /api/calls/function-heatmap` | calls per (function, day) for one realm over the last 14 days, zero-filled and capped at the 20 busiest functions. `realm=<path>` is **required** (400 without it). Fixed range: `days`/`window` are ignored |

## Blocks analytics

Read from the local `blocks` table, which the syncer keeps current and backfills
backward. **All four are single-network**: pass `network=<id>`. With no filter
they return empty or all-zero results rather than an aggregate — block-time
deltas across two interleaved chains are meaningless, proposer identities do not
merge across chains, and a union coverage range would hide a lagging network.

| endpoint | description |
|---|---|
| `GET /api/blocks/time-histogram` | interval between consecutive blocks, binned. Accepts `days`/`window` |
| `GET /api/blocks/proposers` | blocks proposed per validator address. Accepts `days`/`window` and `topN` (defaults to 25 when absent, unparseable or ≤ 0) |
| `GET /api/blocks/coverage` | `min_time`, `max_time` of stored blocks and `complete`, which is true once the backward backfill has reached genesis, the indexer's pruned floor, or the configured history depth |

How much history these cover is set by `-block-history-days` (default 90; `0`
backfills the full chain, a negative value stores no blocks at all). With blocks
declined, these three endpoints return empty rather than failing.

## Graphs

| endpoint | description |
|---|---|
| `GET /api/graph/transfers?network=&window=&topN=&min_value=&ego=&hops=` | top-N addresses by transfer volume in the window (or the 1-hop neighborhood of `ego`, which ignores `topN`). Returns `{nodes: [{id, volume}], edges: [{from, to, value, tx_count}]}`. `topN` defaults to 100, capped at 1000. `hops` is accepted but only `1` has an effect in this batch |
| `GET /api/graph/callers?network=&window=&topN=&min_calls=` | top-N callers by call volume and the realms they called. Returns `{nodes: [{id, type, calls}], edges: [{caller, pkg_path, calls}]}` where `type` is `"caller"` or `"realm"`. `topN` defaults to 200, capped at 1000. No `ego` support yet |

## Search

```
GET /api/search?q=<query>
```

Searches **package paths, names and creators only**. It does not search
transaction hashes, block heights, or addresses — an address matches only when it
happens to be a package creator.

## Live feed

```
GET /api/live?network=<id>
```

Server-Sent Events. Emits `{"type": "block" | "tx", "network_id": ..., "payload": ...}`
as new blocks arrive, with a keepalive comment every 15s. Omitting `network`, or
passing `all`, subscribes to every network. The server polls the indexer every 3s
while at least one client is connected.

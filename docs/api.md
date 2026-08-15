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
`1y`→(365, weekly), `all`→(3650, monthly). `days` and `granularity` predate it,
still work, and win when supplied alongside it.

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
| `GET /api/timeseries/storage` | storage growth. `realm=<path>` scopes it to one realm |
| `GET /api/timeseries/storage/realms` | realms that have storage data, for populating a selector |
| `GET /api/timeseries/blocks` | blocks and transactions per bucket. **Single-network** |

There is no per-realm **activity** time series; `storage` is the only endpoint that
accepts a `realm` parameter.

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
| `GET /api/blocks/coverage` | `min_time`, `max_time` of stored blocks and `complete`, which is true once the backward backfill has reached genesis or the indexer's pruned floor |

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

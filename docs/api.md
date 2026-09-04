# HTTP API

All responses are JSON. Errors are `{"error": "..."}` with a non-200 status.

## Common parameters

**`network`** — accepted by most endpoints.

| value | meaning |
|---|---|
| a configured network ID | restrict to that network |
| `all`, or omitted | every **configured** network |
| anything else | `404 {"error": "network not found"}` |

This applies to the time series and the sanity counters too, which until
recently used no filter at all in that mode and so reported every network ever
synced. "Every configured network" is not the same as "no filter". Rows survive a network
being removed from the config — a retired testnet keeps its history in the
database — and those rows are excluded. Removing topaz from the config dropped
4,101 transactions and 187 realms out of the all-networks totals, which is
correct: they belong to a chain that no longer exists.

**Some endpoints refuse the unfiltered case rather than guessing.** A block height
identifies a different block on every chain, and storage figures are denominated
per chain, so `/api/block/{height}` and `/api/storage/{path...}` answer `400` when
no network is given. This is deliberate: they used to answer from an arbitrary
chain, which looked like data and was not.

**Aggregates that cannot be summed are split, not blended.** Counts (transactions,
realms, addresses) add up meaningfully across chains. Denominated amounts do not —
one chain's ugnot is not another's — so in all-networks mode those endpoints carry
a `by_network` object alongside the totals, and the totals are the arithmetic sum
only for the counts. `by_network` is omitted when a single network is selected,
because then it *is* the total.

Liveness is not merged even that far. There is no such thing as the height, or
the last block time, of four chains at once, so `/api/sanity/overview` leaves its
top-level liveness fields empty in all-networks mode and reports each chain under
`by_network` instead. Each entry carries `reachable`, which separates "this chain
is not producing blocks" from "we could not ask it" — otherwise identical.

**Ranking rows are keyed by `(identifier, network)`, never by the identifier
alone.** 193 package paths exist on more than one chain, and the busiest caller
on the site is active on two. A path or address alone no longer identifies a row,
so every ranking carries a `network` and the same path may appear once per chain
with its own figures. This is what the leaderboards mean by a "top" entry.

**`limit`, `offset`** — pagination, where supported. Noted per endpoint below.

## Caching

Successful `GET /api/*` responses are cached in memory for 30 seconds, keyed on
path plus query string, and served with `X-Cache: HIT` or `MISS`. The TTL matches
the sync interval, so it costs no freshness the pipeline could have delivered.

Errors are never cached — a cached 500 would pin a transient indexer failure for
the whole window. `/api/live` and `/api/version` bypass the cache entirely.

**Time-series parameters**, on every `/api/timeseries/*` endpoint:

| parameter | values | default |
|---|---|---|
| `days` | 1–365, clamped | `30` |
| `granularity` | `hourly`, `daily`, `weekly` | `daily` |

## Meta

| endpoint | description |
|---|---|
| `GET /api/version` | build info: `git_hash`, `build_time` |
| `GET /api/networks` | configured network IDs — the fastest way to confirm which chains an instance is actually serving |
| `GET /api/watch` | activity digest for a watchlist. Repeated `realm=` and `address=` parameters, each optionally `id@height` — that height is the baseline `new_since` counts against. Answered from stored rows only, so a watchlist costs no indexer round-trips. Capped at 100 items |
| `GET /api/labels` | display names for addresses, derived from on-chain data: `{address: {label, kind, why}}`. Currently one rule — the sole deployer of a named namespace is that namespace. `why` states the evidence so any label can be checked |

**Address labels are global, not per network.** An address is the same key on
every chain, so a name earned on one applies everywhere. `/api/labels` derives
what it can prove; the UI adds a small curated map for names that cannot be
derived — faucets and infrastructure keys — and marks any label inferred from
behaviour rather than proved, with the reasoning in its tooltip.

Nothing is derived from a namespace with more than one deployer. Seven exist on
the live chains, and naming one of their deployers would present a guess as a
fact.

## Packages and realms

| endpoint | description |
|---|---|
| `GET /api/realms` | list realms. `limit`, `offset` |
| `GET /api/packages` | list all packages, realms and pure packages. `limit`, `offset` |
| `GET /api/realm/{path...}` | detail for one package: metadata, source files, imports, dependents, callers, MsgRun references |
| `GET /api/deps/{path...}` | dependency graph as `{path: [imports]}`. `dir=dependents` reverses direction |
| `GET /api/storage/{path...}` | storage events for a package. **Requires `network`**: the figures are denominated amounts and blending chains would be meaningless |
| `GET /api/events/{path...}` | events emitted by a package. Bounded: `limit` defaults to 200, capped at 2000. In all-networks mode it queries every chain and tags each row with its `network` |

## Transactions and blocks

| endpoint | description |
|---|---|
| `GET /api/txs` | recent transactions. `limit` (default 500, max 2000), `offset`, `type` = `MsgCall`/`MsgAddPackage`/`MsgRun`/`BankMsgSend`, `success` = `true`/`false`. A `type` filter is served **from local storage** and pages properly with a real total; without one the rows come from the indexer and `total` is the fetched window. `from_storage` says which |
| `GET /api/tx/{hash}` | one transaction: messages, events, errors |
| `GET /api/blocks` | recent blocks. `limit` |
| `GET /api/block/{height}` | one block and its transactions. **Requires `network`**: a height alone does not identify a block across chains |
| `GET /api/allevents` | recent events across all packages. `limit` defaults to 200, capped at 2000. Rows carry their `network` |

`total` on `/api/txs` is the size of the fetched window, **not** the chain's
transaction count. It never could be: the indexer caps a result set at 10,000
records and exposes no count, so there is no way to ask how many exist. The UI
labels this figure "recent" for the same reason.

## Addresses and accounts

| endpoint | description |
|---|---|
| `GET /api/address/{addr}` | activity for an address, **from local storage**: calls, deploys, runs, sends (both directions), with `total` covering its whole history and `limit`/`offset` paging the rows. `balance` comes from RPC and is present only when a single network is selected. The indexer cannot serve this at chain scale — five address predicates over unindexed fields means a scan |
| `GET /api/accounts` | most active accounts. `limit` (default 100, max 500), `offset`, and `sort` = `calls`, `deploys`, `runs`, `sends` or total activity. One row per `(address, network)`: the same key on two chains is two different actors, and each row carries its `network` |

## Aggregates

| endpoint | description |
|---|---|
| `GET /api/stats` | totals: transactions, calls, deploys, msg_runs, sends, realms, packages, unique callers, latest block |
| `GET /api/analytics` | leaderboards and aggregate breakdowns. Every ranking row carries its `network`; rankings are scoped to the selected chain |
| `GET /api/gas` | gas usage, by realm. Totals and the per-realm breakdown come from rollups rebuilt every 5 minutes, with `computed_at` saying when. Neither can be indexed away — attributing gas per realm means touching every call — and both had reached 14s. Falls back to computing live if the rollups are not built yet |
| `GET /api/bankstats` | transfer volume statistics, from rollups rebuilt every 5 minutes with `computed_at` saying when; falls back to computing live before the first build. Carries `by_network` in all-networks mode, because volume cannot be summed across chains. Every ranking row is keyed by `(address, network)` and carries its `network` |
| `GET /api/tokens` | detected token packages. Rows carry their `network` |
| `GET /api/validators` | valoper registrations, **served from storage** rather than the indexer. Flat rows with `address`, `moniker`, `func` and `success` — `address` is the validator the call is about, which is not always the caller |
| `GET /api/govdao` | governance calls, **served from local storage** as a prefix match on `gno.land/r/gov/dao`. The indexer cannot answer this: its filter is a substring match over an unindexed field, so it scans until the deadline on a chain with no governance activity, and its predicate can match a message carrying no `pkg_path` at all |
| `GET /api/sanity/overview` | consistency counters and liveness. In all-networks mode liveness moves to `by_network`, one entry per chain, each with `reachable` |

## Time series

All accept `days` and `granularity`.

| endpoint | description |
|---|---|
| `GET /api/timeseries/transactions` | transaction counts |
| `GET /api/timeseries/packages` | deployments |
| `GET /api/timeseries/callers` | unique callers |
| `GET /api/timeseries/gas` | gas consumption. Buckets carry `by_network` in all-networks mode, so fees can be shown per chain rather than summed |
| `GET /api/timeseries/active-addresses` | active addresses |
| `GET /api/timeseries/health` | chain health indicators |
| `GET /api/timeseries/storage` | storage growth. `realm=<path>` scopes it to one realm |
| `GET /api/timeseries/storage/realms` | realms that have storage data, for populating a selector |

There is no per-realm **activity** time series; `storage` is the only endpoint that
accepts a `realm` parameter.

## Search

```
GET /api/search?q=<query>
```

Searches **package paths, names and creators only**. It does not search
transaction hashes, block heights, or addresses — an address matches only when it
happens to be a package creator.

The UI covers the rest without asking the server: an address, a transaction hash
or a block height is recognised by shape and offered as a direct destination
above the package matches. A bare number is offered only when a network is
selected, since a height identifies a different block on every chain.

## Live feed

```
GET /api/live?network=<id>
```

Server-Sent Events. Emits `{"type": "block" | "tx", "network_id": ..., "payload": ...}`
as new blocks arrive, with a keepalive comment every 15s. Omitting `network`, or
passing `all`, subscribes to every network that has a feed. The server polls the
indexer every 3s while at least one client is connected, and stops when the last
one disconnects.

A network configured without an indexer client gets no feed rather than a broken
one, and a subscription naming a network with no feed is accepted but silent —
the connection stays open and delivers nothing.

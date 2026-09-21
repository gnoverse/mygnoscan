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

Successful `GET /api/*` responses are cached in memory, keyed on path, query
string and negotiated content encoding. The TTL is 30 seconds and matches the
sync interval, so it costs no freshness the pipeline could have delivered.

Past the TTL an entry is **served stale while it is refreshed** — the reader gets
the old body immediately and one background refresh is started for everyone
behind them. `X-Cache` says which of the three happened:

| `X-Cache` | Meaning |
|---|---|
| `HIT` | within the TTL, served as-is |
| `STALE` | past the TTL, served anyway; a refresh is running |
| `MISS` | nothing usable was stored; the handler ran inline and the caller waited |

The stale window is 15 minutes past the TTL. Beyond it an entry stops being
servable at all: a reader returning after a long absence should not be handed a
long-dead chain tip, however fast. One stale entry produces one refresh no matter
how many readers arrive during it, and a refresh runs on its own context so it
survives the reader who triggered it closing the tab.

Errors are never cached — a cached 500 would pin a transient indexer failure for
the whole window. `/api/live` and `/api/version` bypass the cache entirely.

## Compression

Responses are gzipped when the client sends `Accept-Encoding: gzip` and the body
is over 1 KiB of a compressible type (JSON, HTML, JS, SVG, plain text). This is
the dominant term in how long a page takes to appear: `/api/txs` is 3.5 MB of
JSON on mainnet, `/api/allevents` 1.3 MB, and gzip takes roughly 85-90% of that
away. `Vary: Accept-Encoding` is set on every response.

`text/event-stream` is never compressed — `/api/live` is an open stream, and
anything that buffers it holds the live feed back for the life of the tab.

**Time-series parameters**, on every `/api/timeseries/*` endpoint:

| parameter | values | default |
|---|---|---|
| `window` | `24h`, `7d`, `30d`, `90d`, `1y`, `all` | unset |
| `days` | 1–365, clamped (`monthly` raises the cap to 3650) | `30` |
| `granularity` | `hourly`, `daily`, `weekly`, `monthly` | `daily` |

`window` is the current contract and sets both `days` and `granularity` at once:
`24h`→(1, hourly), `7d`→(7, hourly), `30d`→(30, daily), `90d`→(90, daily),
`1y`→(365, weekly). It is case-insensitive, and an unrecognised value is ignored
rather than rejected. `days` and `granularity` predate it, still work, and win
when supplied alongside it.

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

The 365-day clamp keeps hourly, daily and weekly bucket counts bounded.
`monthly` exists to span longer ranges, so it is exempt from that clamp and
bounded at 3650 days instead.

## Meta

| endpoint | description |
|---|---|
| `GET /api/version` | build info: `git_hash`, `build_time` |
| `GET /api/networks` | configured network IDs — the fastest way to confirm which chains an instance is actually serving |
| `GET /api/watch` | activity digest for a watchlist, plus a `transactions` timeline: the 50 most recent rows across every watched realm and address, merged and deduplicated. Repeated `realm=` and `address=` parameters, each optionally `id@height` — that height is the baseline `new_since` counts against (the timeline itself is not filtered by it). Answered from stored rows only, so a watchlist costs no indexer round-trips. Capped at 100 items |
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

### Sync health versus chain liveness

`by_network` answers "is this chain producing blocks". `sync` answers "are we
managing to read it", and the two come apart: `indexer.gno.land` once rejected
every query mygnoscan sent it for more than a day while liveness stayed green,
because a fallback endpoint in the same pool was answering. Nothing surfaced
that the primary had stopped, which is why `indexers` names the endpoint
actually serving each network alongside the pool it was chosen from.

`sync` covers this process only and is absent before the first pass finishes, so
an instance started with `-sync=false` reports no sync state rather than a
failing one. `last_error` survives a later success, with `last_error_at` beside
it, because a network flapping between the two is the case most worth seeing.

## Contracts map

Powers `/contracts`: every deployed package on one network, with two different
meanings available for the lines between them.

The page draws that data six ways, selected by `?view=` and served by the same
two endpoints, and switching view is a repaint, never a refetch:

| view | what it keeps, and what it gives up |
|---|---|
| `force` | the bubblemaps read: clusters emerge from the links. The only one whose positions carry no meaning you could read off an axis, and the only one that has to settle before it means anything. Above ~400 contracts it is a field of uniform dots |
| `orbit` | one contract at the centre, its direct links on the first ring and their links on the second. Trades the whole chain for one neighbourhood, and is bounded by construction: a chain of ten thousand contracts still draws one centre and two rings |
| `packed` | namespaces as nested circles, the bubblemaps *cluster* rendering taken literally. Deterministic, instant, spends the whole viewport. Gives up the edges, which come back on hover for one contract at a time |
| `bundled` | every contract on one ring grouped by namespace, edges routed through the namespace they belong to. The only legible rendering of a dense import graph; hovering separates what a contract imports from what imports it |
| `chord` | namespace to namespace, one ribbon per pair, intra-namespace links excluded. Gives up the contracts to answer who shares a population with whom |
| `treemap` | area is the metric, nested by namespace. The boring control: a packing wastes the gaps between circles, so comparing two areas is guesswork, and a treemap spends every pixel |

`?focus=` names the orbit's centre; absent, it picks the best-connected
contract. `?flow=0` stops the particle animation on `orbit` and `bundled`, which
is also what `prefers-reduced-motion` does.

| endpoint | description |
|---|---|
| `GET /api/contracts/map` | every deployed package on one network, with `calls`, `unique_callers`, `gas_used`, `storage_bytes`, `importers`, `imports`, `namespace`, `creator`, `deployed_at` and `parked` on each node. `window` = `all` (default), `30d`, `7d`, `24h` narrows the call-derived metrics only, never the node set. Unknown windows are a 400 rather than a silent fall back to all time |
| `GET /api/contracts/edges` | lines between contracts. `kind=callers` (default) links two contracts when the same addresses called both, weighted by how many overlap; `kind=imports` is the directed import graph, and ignores `window`. `min` (default 2) is the smallest overlap that counts, `limit` (default 1500, max 20000) keeps the heaviest, `maxFanout` (default 30, max 500) drops addresses that touched more contracts than that from edge generation |

**Both require a single network, and resolve one rather than refusing.** An
absent or `all` network resolves to the first configured one, and the response
says which under `network`. Every other endpoint treats an absent network as
"all of them"; this one cannot, because a bubble is identified by its path and
193 package paths exist on more than one chain. Blending them would draw one
bubble carrying two chains' traffic.

`kind=callers` has no realm-to-realm equivalent to offer instead: `calls.caller`
is the transaction signer, an externally-owned account, and a realm calling
another realm inside the VM leaves no message for the indexer to record. Shared
population is the honest substitute, not a stand-in for a call graph.

`importers` is what the import view should be read with, and the reason it
looked empty before it existed: sized by calls or gas, the packages everything
is built on are invisible dots, because a pure package receives no calls and
burns no gas of its own. `p/nt/ufmt/v0` has 91 dependents on mainnet and nothing
else at all.

The three bounds on `kind=callers` exist because the pair count is quadratic in
how many contracts one address touched: a bot that called fifty of them
contributes 1,225 pairs by itself and links everything to everything. Its calls
still count toward every node's metrics either way.

## Chain parameters

Powers `/params`: what one chain is currently configured to do. Read live from
that network's verified RPC on every cache miss; nothing here is synced, and
none of it can come from the indexer.

| endpoint | description |
|---|---|
| `GET /api/params` | `identity` (chain id, node version and moniker, `tx_index`, height, `catching_up`, plus the RPC and indexer actually in use), `groups` of catalogued params, and `consensus` from `/consensus_params` |

**Single network, resolved rather than refused**, the same rule as
`/api/contracts/*`: an absent or `all` network resolves to the first configured
one and the response says which under `network`. There is no such thing as the
code submission policy of three chains at once.

Each param carries `state`, and the four values are the point of the endpoint:

| `state` | means | looks like |
|---|---|---|
| `set` | configured, with a value | `raw` plus `value` or `list` |
| `empty` | configured, and the value is empty | `raw` is `[]` or `""` |
| `unset` | this chain has never held the key | no `raw` |
| `error` | the node refused the query | `error` |

Every read goes through the `params/` prefix, which is what makes that split
possible: a bare `vm:p:<key>` answers with empty data and no error whether the
key is unset **or** the query was malformed. Conflating `empty` with `unset`
would be a real misreport, because `vm:p:pkg_approvers` empty freezes every
parked package on the chain while unset is not a configuration at all.

`raw` is the chain's response verbatim, still JSON-encoded, and it is kept
alongside the decoded `value`/`list` rather than dropped. It is the form a
reader quotes in a proposal, and the only one that survives a decoding bug here.

`note` is a remark computed against other live state, never a restatement of the
value: `node:p:halt_height` is read against the current height, so a halt that
has already been passed says so instead of reading as armed.

A key absent from the response is one this catalogue does not list. The chain
cannot enumerate its own params, so the list is curated by hand and the page
says as much.

Reads degrade per key. One rejected query produces one `error` row and leaves
the rest of the page intact, which matters because an odd chain state is exactly
when someone opens this.

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
| `GET /api/address/{addr}` | activity for an address, **from local storage**: calls, deploys, runs, sends (both directions), with `total` covering its whole history and `limit`/`offset` paging the rows. `balance` comes from RPC and is present only when a single network is selected **and** that RPC has been confirmed to serve the same chain as the network's indexer — an unverified or mismatched RPC yields an empty balance rather than one from another chain. The indexer cannot serve this at chain scale — five address predicates over unindexed fields means a scan |
| `GET /api/accounts` | most active accounts. `limit` (default 100, max 500), `offset`, and `sort` = `calls`, `deploys`, `runs`, `sends` or total activity. One row per `(address, network)`: the same key on two chains is two different actors, and each row carries its `network` |

## Aggregates

| endpoint | description |
|---|---|
| `GET /api/stats` | totals: transactions, calls, deploys, msg_runs, sends, realms, packages, unique callers, latest block |
| `GET /api/analytics` | leaderboards and aggregate breakdowns. Every ranking row carries its `network`; rankings are scoped to the selected chain |
| `GET /api/gas` | gas usage, by realm and by caller. Totals and both breakdowns come from rollups rebuilt every 5 minutes, with `computed_at` saying when. None of this can be indexed away — attributing gas means touching every call — and it had reached 14s. Falls back to computing live if the rollups are not built yet |
| `GET /api/bankstats` | transfer volume statistics, from rollups rebuilt every 5 minutes with `computed_at` saying when; falls back to computing live before the first build. Carries `by_network` in all-networks mode, because volume cannot be summed across chains. Every ranking row is keyed by `(address, network)` and carries its `network` |
| `GET /api/tokens` | detected token packages. Rows carry their `network` |
| `GET /api/validators` | valoper registrations, **served from storage** rather than the indexer. Flat rows with `address`, `moniker`, `func` and `success` — `address` is the validator the call is about, which is not always the caller |
| `GET /api/validators/monikers` | consensus-address → name, for labelling block proposers. Sourced from [gnockpit](https://gnockpit.gno.land), not this chain's own data — a proposer's consensus key is never published to the valopers realm, which registers the *operator* key instead, so nothing indexed here can answer this. Best-effort and cached 5 minutes: an unreachable gnockpit yields `{}`, not an error |
| `GET /api/govdao` | governance calls, **served from local storage** as a prefix match on `gno.land/r/gov/dao`. The indexer cannot answer this: its filter is a substring match over an unindexed field, so it scans until the deadline on a chain with no governance activity, and its predicate can match a message carrying no `pkg_path` at all |
| `GET /api/govdao/overview` | proposal list plus memberstore tiers/members, parsed live from gov/dao's own `Render()` output over RPC (`vm/qrender`) — not reimplemented against its storage, so a rule mygnoscan does not know about (a tier threshold changing, say) still shows correctly. Cached 30s per network; a failed RPC round trip serves the last good result rather than an empty page |
| `GET /api/govdao/proposals/{id}` | one proposal's full detail: description, executor package, status, vote percentages, per-address votes (all parsed from the realm's own render), plus two independently sourced "how did this happen" trails — `related_calls` (vote/execute MsgCalls naming this proposal ID, found live on the indexer since the local `calls` table does not keep call arguments) and `related_msgruns` (`maketx run` scripts that plausibly created it, found by searching locally synced script source for both `gov/dao` and the proposal's executor package path — a heuristic, not a guarantee, given gov/dao's low proposal volume) |
| `GET /api/inert/queue` | every package currently parked under the "inert" code submission policy, newest submission first. Read live over RPC (`vm/qinertpaths` for the path list, `vm/qpkgmeta_json` per path for creator/height/reason), cached 20s per network — vm/qinertpaths returns bare paths, so each one needs its own metadata round trip, fanned out concurrently |
| `GET /api/inert/history` | recent `MsgEnablePackage`/`MsgRejectPackage` activity plus approval-speed stats (`stats.median_wait_blocks`/`median_wait_seconds` etc — the gap between a submission's height, pinned by the enabling message itself, and the block its approval landed in) |
| `GET /api/inert/package/{path...}` | one path's current `vm/qpkgmeta_json` status plus its full submission history — every `MsgAddPackage`/`MsgEnablePackage`/`MsgRejectPackage` naming it, chronological, including redeploys parked while an earlier submission at the same path was still pending |
| `GET /api/sanity/overview` | consistency counters and liveness. In all-networks mode liveness moves to `by_network`, one entry per chain, each with `reachable`. Also carries `sync` (per network: `healthy`, `last_success_at`, `last_error`, `consecutive_failures`) and `indexers` (per network: the `active` endpoint and the full `pool`) |

## Time series

All accept `days` and `granularity`.

| endpoint | description |
|---|---|
| `GET /api/timeseries/transactions` | transaction counts |
| `GET /api/timeseries/packages` | deployments |
| `GET /api/timeseries/callers` | unique callers |
| `GET /api/timeseries/gas` | gas consumption. Buckets carry `by_network` in all-networks mode, so fees can be shown per chain rather than summed |
| `GET /api/timeseries/active-addresses` | active addresses. Served from a rollup of distinct `(network, hour, kind, address)` tuples rebuilt every 5 minutes, merged with a live read of everything newer than the rollup — so the newest bucket does not lag the refresh interval. Falls back to computing live before the first build. Distinct tuples rather than per-day counts, because an address active on three days of a week is one weekly active address, not three |
| `GET /api/timeseries/health` | chain health indicators |
| `GET /api/timeseries/storage` | storage growth. `realm=<path>` scopes it to one realm |
| `GET /api/timeseries/storage/realms` | realms that have storage data, for populating a selector |
| `GET /api/timeseries/storage/deltas` | on-chain storage movement per bucket: `deposited`, `released` (negative, as the chain emits it) and `net`, from `storage_events`. `realm=<path>` scopes it to one realm. Distinct from `/api/timeseries/storage`, which counts source bytes added and only ever grows |
| `GET /api/storage/consumers` | realms ranked by absolute net storage change. `topN` (default 20, max 100). Keyed by `(network, pkg_path)`, so a realm deployed on two chains is two rows |
| `GET /api/graph/transfers` | value-transfer graph for one chain. **Requires `network`**: values are denominated and an address is a different actor per chain. `topN` (default 100, max 1000), `min_value`, or `ego=<address>` for that address's 1-hop neighbourhood, which ignores `topN`. Returns `{nodes: [{id, volume}], edges: [{from, to, value, tx_count}]}`. There is no `hops` parameter — `ego` is fixed at 1 hop |
| `GET /api/graph/callers` | caller-to-realm graph for one chain. **Requires `network`**, same reason. `topN` (default 200, max 1000), `min_calls`. Returns `{nodes: [{id, type, calls}], edges: [{caller, pkg_path, calls}]}` where `type` is `"caller"` or `"realm"`. No `ego` support yet |
| `GET /api/timeseries/blocks` | blocks and transactions per bucket. **Single-network** |
| `GET /api/timeseries/new-addresses` | addresses seen on-chain for the first time, bucketed by that first appearance. First-seen is derived over all indexed history, so widening the window never relabels an old address as new |
| `GET /api/timeseries/active-rolling` | `dau`, `wau`, `mau` — distinct active addresses over trailing 1/7/30-day windows. **Always daily**: `granularity` is ignored, because the three windows are day-defined. A request shorter than 7 days is widened to 7, and capped at 365 regardless of `window`/`days`/`granularity` |

`storage` and `calls/function-heatmap` are the only endpoints that accept a
`realm` parameter; there is no general per-realm activity time series.

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

## Embed mode

Any page accepts `?embed=1`, which strips the header, footer and sub-bar so a
host application can iframe one piece of content rather than a whole
page-in-a-page:

```html
<iframe src="https://mygnoscan.example/realm/r/demo/boards?tab=graph&embed=1"
        width="800" height="600" style="border:0"></iframe>
```

The flag is read by an inline script in `<head>`, before the body exists, so the
chrome never paints. Applying it from the SPA's own routing would let the header
render and then vanish, which inside an iframe reads as a layout glitch on every
navigation.

Everything else is unchanged: the same routes, the same query parameters, the
same data. `?network=` composes with it as usual.

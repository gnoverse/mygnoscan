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
per chain, so `/api/block/{height}`, `/api/storage/{path...}` and
`/api/storage/map` answer `400` when no network is given. This is deliberate:
they used to answer from an arbitrary chain, which looked like data and was not.
`/api/storage/map` is the starkest case: its capacity is one chain's money
supply divided by one chain's price, so used bytes summed over two chains
against it would be a fullness figure true of neither.

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
| `GET /api/labels` | display names for addresses: `{address: {label, kind, why}}`, the curated registry merged with what the chain proves |
| `GET /api/registry/apps` | the curated app directory: `categories`, `apps` and a count of known tokens |

**Address labels are global, not per network.** An address is the same key on
every chain, so a name earned on one applies everywhere.

**`kind` is the provenance, and it is the point.** Four values, never collapsed,
because a name a human vouched for, a fact the chain proves, a claim the subject
made about itself and a heuristic are four different things:

| `kind` | means | asserted by |
|---|---|---|
| `curated` | a human vouched for it in a merged pull request | a contributor |
| `derived` | proved from chain data, recomputed on every request | the chain |
| `declared` | the subject said so (`r/sys/users`, a valoper moniker) | the address itself |
| `inferred` | a heuristic over observed behaviour | this repo |

The explorer marks the two that ask a reader to take something on trust, with
the evidence in the tooltip. `declared` is attacker-controlled by construction:
anyone may call `UpdateDescription` on `r/gnops/valopers` and claim any name.

**Precedence is curated, derived, declared, inferred.** Curated winning over
derived is the one judgement call here, and the obvious argument runs the other
way, since derived is proved and live while curated can rot. It still loses,
because the registry's own rule is that a curated entry is only added for
something that *cannot* be derived. An address carrying both therefore means a
person looked at the derived name and decided a better one was needed. The
losing claim is not discarded: it is appended to the winner's `why`, so the
corroboration survives.

The curated half lives in `pkg/registry/data/`, embedded at build time, and
adding an entry is a pull request against a JSON file. `pkg/registry/README.md`
has the rules and `go test ./pkg/registry/` enforces them, including that a
`derived` label may never be written down: a stored copy of something computed
live stops being true the moment the chain moves.

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

`?metric=` decides every area and every radius drawn, so the number behind it is
printed wherever there is room for it: under the name in each treemap cell and
each packed circle, beside the namespace on its band, as a total on the count
line and on each legend entry, and on the hover card, which lists all six
metrics and marks the one the picture is sized by. Storage is formatted as
bytes, the rest compactly (`1.2k`, `4.3M`); the hover card is the exact form.
Rows for `calls` and `unique callers` carry the window, because those are the
two `?window=` moves.

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

## Chain health

Powers the diagnosis and heartbeat sections of `/sanity`, plus the header chip
that appears on every page when a network is degraded.

| endpoint | description |
|---|---|
| `GET /api/sanity/overview` | as before, plus `nodes` and `diagnosis`, keyed by network |
| `GET /api/health/heartbeat` | block cadence per network. `window` = `5m` (default), `30m`, `3h`, which select 10s, 60s and 360s cells. Always every configured network, never just the selected one |

### Why the node is asked directly

Every liveness figure on this response other than `nodes` comes from the
indexer, and the indexer cannot distinguish a chain that stopped from a data
source we have lost. Both read as unreachable, and they need different people to
fix them.

`nodes` is the node's own account, read over RPC: height, whether it is catching
up, the live consensus round, peer count and mempool depth. `diagnosis` combines
it with the indexer's view into one verdict:

| `state` | meaning |
|---|---|
| `alive` | blocks advancing, nothing to do |
| `syncing` | the node is catching up |
| `stale` | no recent block, but consensus is still advancing |
| `indexer_down` | the chain is producing blocks and our indexer is not answering |
| `wedged` | consensus has not advanced, with the height, round and step it stopped on |
| `node_unreachable` | the node did not answer, so live state is unavailable |
| `unknown` | neither source answered |

`healthy` is true only for `alive`, so a badge does not have to keep its own
list of which states are bad.

The load-bearing field is `round_age_seconds`, derived from
`/consensus_state`'s `round_state.start_time`. A stopped node answers `/status`
with a plausible height forever, which is why any check written as a floor
(`height >= 1`) passes on a chain frozen for months. The round start is
refreshed every height, so its age is seconds when things are fine and weeks
when they are not, with no baseline to keep and no second sample to take. Past
120 seconds, matching the threshold `is_alive` already uses, the verdict is
`wedged` and names the step.

### This probe talks to unverified endpoints, on purpose

Every other RPC caller here goes through the verified endpoint, which is
withheld until `VerifyRPCChains` has confirmed it serves the same chain as the
indexer. That verification reads block 1 *from the indexer*, so a network whose
indexer is gone can never have a verified RPC, and routing this probe through it
would blind the page to exactly the case it exists for.

Withholding is right for balances, where an unverified endpoint could serve a
figure from another chain and nobody would see it. Here the node's identity is
part of what is being reported: each entry carries the `chain_id` it claims and
a `verified` flag, so a mismatch surfaces instead of being silently trusted.

### The heartbeat is sync coverage, not chain cadence

`/api/health/heartbeat` is built from the local `blocks` table, so it costs one
indexed range scan per network and shows the same history after a reload. The
consequence is worth stating: a network this instance is not successfully
syncing has an empty strip even when its chain is fine, and the newest cells
trail the tip by up to one sync interval. That is why the strip sits beside the
diagnosis rather than replacing it.

Cells are anchored to `now`, not to the newest block. A grid built from the last
block shows a full strip for a chain that stopped an hour ago, which is the one
answer it must never give.

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

## Validators

| endpoint | description |
|---|---|
| `GET /api/validator/{addr}` | one validator by consensus address: its proposed blocks, and a daily share series |
| `GET /api/validators` | unchanged: the `r/gnops/valopers` registration log |

`/validators` already renders the set, deriving proposers from recent blocks,
drawing a liveness sparkline per row and joining gnockpit's power, missed-block
and spof columns onto them. That table is a snapshot; this endpoint is the one
view it cannot be, which is a single validator across the chain's history.

### Two address spaces, deliberately not joined

The word "validator" covers two disjoint keys, and conflating them is a bug this
repo has already had (`loadValMonikers` never matched a proposer):

| key | who has it | where it appears |
|---|---|---|
| **consensus** | the node signing blocks | `blocks.proposer_id`, gnockpit |
| **operator** | the account registering the validator | `valoper_registrations` |

Nothing on chain maps one to the other. This endpoint is keyed on the consensus
address, and the registration table stays its own view. Verified against mainnet
on 2026-09-20: all four gnockpit addresses matched the interned proposer
addresses exactly, so *that* join does work and is the one used here.

### What each figure comes from

`voting_power`, `missed_24h`, `missed_100`, `avg_block_ms` and `spof` come from
gnockpit. `blocks`, `txs`, `share` and `last_block_time` come from the blocks
this instance has synced, so they cover the synced range rather than all of
history.

`in_set` separates the two cases that otherwise look identical: an address with
proposal history that gnockpit no longer lists has **left the set**, and the
page says so instead of rendering an active-looking validator. A live member
this instance has never seen propose is served too, with zero blocks. An address
that is neither is a 404, because an empty page reads as an idle validator.

### No staking, and no voting-power timeline

gno has no delegation and its set is governance-assigned, so there is no APR, no
bonded ratio, no commission and no delegator list. Those columns would be four
confident zeros.

No historical validator set is stored anywhere and gnockpit reports only the
current one, so a true voting-power timeline is not derivable. `shares` is the
observable half of one: each day's blocks split between proposers, so a
validator joining or leaving shows up as its share appearing or going to zero.
A share rather than a count, because variable block time makes per-day counts
incomparable.

## Assets (GRC20)

| endpoint | description |
|---|---|
| `GET /api/assets` | every asset seen on a network: supply, holders, transfer counts, first and last seen, plus registry metadata |
| `GET /api/asset/{token...}` | one asset: top holders, recent transfers, and a daily supply series. Single network |

Built from the `Transfer` events the chain already emits, which were flowing
through the sync walk unstored. An empty `from` is a mint and an empty `to` a
burn, so replaying the column gives **exact** supply and every holder's balance
with no extra RPC call. Holders are counted from reconstructed balances, not
from distinct recipients: an address that received and passed it all on is not a
holder.

`/api/tokens` previously listed packages whose `dependencies.import_path`
matched `%grc20%`. That matches anything that *imports* grc20 rather than
anything that *is* a token, so a DEX router sat in the list beside the tokens it
calls, and the row carried no supply, holders or volume.

### Three things the events do not guarantee

Each of these was measured against mainnet on 2026-09-20 and each would produce
a plausible wrong answer if assumed away.

**The event's `pkg_path` is the library, not the token.** Every GRC20 event on
the chain reports `gno.land/p/nt/grc20/v0`. Grouping by it produces one giant
asset holding every token on the chain. The token is in the `token` attribute.

**The `token` attribute is usually, not always, `<path>.<name>.<id>`.** Two live
mainnet tokens (`COVID`, `META`) emit a bare symbol instead. The key is stored
verbatim and only split when it has the shape; for the others `pkg_path` is
empty and the UI says the realm is unknown rather than printing the symbol in a
column headed "realm".

**Not every `Transfer` carries an amount.** GRC721 emits the same event shape
without a value, so gnoswap's GNFT has 201 transfers that all parse to 0.
Summing them yields a supply of 0 and no holders, which reads as "this token is
empty" when it means "this arithmetic does not apply". Each asset therefore
carries `fungible`, derived from whether *any* of its transfers carried a
positive amount, and both figures are suppressed when it is false. Derived from
the data rather than from the library path, so a token that starts carrying
amounts starts counting.

Failed transactions are skipped: their events are still reported and counting
them would invent supply.

### What is deliberately absent

No price, no market cap, no total value. GNOT is not listed on any exchange and
there is no oracle on chain, so every such column would be a number this
explorer invented. `verified` is the curated registry's claim and is worded as
one: anyone can deploy a realm called `gns`, and nothing on chain distinguishes
the real one.

Amounts are raw units, not scaled by `decimals`. Only a handful of tokens have a
curated entry, and dividing by a guessed exponent produces a different number
wearing the right shape.

## Account balances

| endpoint | description |
|---|---|
| `GET /api/accounts/rich` | addresses ranked by balance, with `coverage`. `limit` (default 100, max 500), `offset` (1-based). Single network, resolved rather than refused |
| `GET /api/accounts/population` | `known`, `daily_active`, `weekly_active`, `monthly_active` |

Balances are the one figure here that can be neither synced nor derived. gno
carries no balance in any indexed message, and computing one from `bank_sends`
as received-minus-sent would be wrong in a way a reader could not see: it
ignores gas fees, storage deposits, genesis allocations and every transfer a
realm makes through a banker rather than a `BankMsgSend`. A rich list that is
wrong at the top is worse than no rich list.

So each balance is one live `bank/balances` read, **swept into a local table in
the background** rather than fetched on the read path. `/api/accounts` used to
fan out one request per row, 20 at a time, on every cold request, against a
single node, which is where its 7.8s came from. It is now a join.

The sweep runs every 10 minutes, 400 addresses at a time at 8 concurrent
requests, ordering never-fetched addresses first and then oldest-first, so a
cold cache fills in over several passes and a warm one refreshes round-robin.
It only asks endpoints that passed `VerifyRPCChains`: withholding a figure costs
a blank, and trusting a mismatched one costs a number from another chain that
nobody can see is wrong.

### What the ranking actually covers

`coverage` travels with every rich-list response and the page prints it:

| field | meaning |
|---|---|
| `swept` | addresses with a cached balance |
| `known` | addresses this instance has seen on chain, in any role |
| `oldest_fetch`, `newest_fetch` | how old the figures are |

This ranks what has been seen and swept, not a chain's accounts, and says so.
Cosmos explorers can claim the stronger thing because Cosmos can enumerate
accounts; gno offers no way to do that at any price.

For the same reason `known` is labelled **"addresses seen"** rather than "total
accounts" in the UI. It counts distinct addresses appearing in `calls`,
`package_submissions`, `msg_runs` or either side of `bank_sends`. It is a real
number; it is not the number of accounts that exist.

`daily_active` and friends re-deduplicate from `active_addr_rollup` rather than
summing it, because counts cannot be re-aggregated: an address active on three
days of a week is one weekly active address, not three. An empty rollup (a fresh
instance, before the first build) falls back to counting live rather than
reporting a confident zero.

An address with no cached balance is **absent** from the lookup, and rendered as
unknown rather than as zero. Those are different claims and only one of them is
safe to make about money.

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
| `GET /api/storage/map` | the whole /storage page in one response. **Requires `network`**: capacity is a chain's own supply divided by its own price per byte, so there is no total across several. Returns `capacity` (`price_per_byte`, `price_source` = `chain` or `default`, `supply_ugnot`, `capacity_bytes`, `used_bytes`, `locked_ugnot`, `realms`), `cells` (one row per realm with `namespace`, `deployer`, `bytes`, `fee`, `first_height`, `last_height`), `payers` (per account, attributed to whoever the chain charged) and `cells_truncated`. `limit` (default 2000, max 5000) caps `cells` only, and `capacity.used_bytes` is always the full total. `capacity_bytes` and the supply are **decimal strings**, not numbers: 1.3e15 ugnot is past what JSON can carry exactly |
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

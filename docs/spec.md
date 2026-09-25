# Specification

What mygnoscan is, what it stores, and what it means.

## Purpose

A block explorer for [gno.land](https://gno.land) chains, focused on the thing
generic explorers do badly: **what code is deployed, what it imports, and who
calls it.** Transactions and blocks are table stakes; the dependency and usage
graph between realms is the point.

It tracks multiple networks in one instance and one database.

## Scope

mygnoscan reads from a [tx-indexer](https://github.com/gnolang/tx-indexer)
GraphQL endpoint, and optionally from an RPC endpoint for state that the indexer
does not carry (account balances). It never writes to a chain, holds keys, or
signs anything.

## Data model

Every table below carries a `network` column that is part of its primary key or
unique constraint. **Network scoping is the central invariant**: the same package
path exists on multiple chains and means different things on each.

| table | grain | notes |
|---|---|---|
| `packages` | one deployed package per network | `is_realm` distinguishes realms from pure packages |
| `package_files` | one `.gno` file | full source body, stored verbatim |
| `dependencies` | one import edge | `package_path` → `import_path` |
| `calls` | one `MsgCall` message | caller, target path, function name |
| `msg_runs` | one `MsgRun` message | full source of the run |
| `bank_sends` | one `BankMsgSend` message | from, to, amount |
| `transactions` | one transaction | height, time, gas used/wanted/fee, success |
| `blocks` | one block per network | height, time, `num_txs`, and the interned proposer. `WITHOUT ROWID` — the range is contiguous by construction |
| `proposers` | one validator address per network | interned so a block row stores an id, not a 40-byte address |
| `users` | one registered name per network | keyed on the name, not the address: one address can hold several. `alias` marks a previous name, `deleted` a tombstone, and both stay, because `r/sys/users` never frees a name. Replayed from the realm's own events |
| `sync_state` | key/value | sync bookkeeping, keyed with the network in the key |

Derived, not stored:

- **Dependency graphs** are computed by walking `dependencies` recursively, in
  both directions (imports, and dependents).
- **MsgRun references** to a package are found by substring search over
  `msg_runs.source`. A `MsgRun` has no structured import list, so this is a text
  match, and it will match a mention inside a comment or string.
- **Account activity** is aggregated across `calls`, `msg_runs` and `bank_sends`.
- **Balances** are not stored. They are fetched live from RPC per request.

Derived and stored, on a timer:

The `*_rollup` tables hold nothing the source tables do not already imply. They
exist because a few aggregates cannot be indexed away and had grown into
double-digit seconds. All of them are replaced wholesale inside one transaction
every five minutes, so a reader sees one consistent generation, and all of them
fall back to computing live before the first build rather than reporting zero.

| table | grain | answers |
|---|---|---|
| `gas_realm_rollup` | one row per `(network, path)` | gas and fees attributed per realm |
| `gas_totals_rollup` | one row per network | chain-wide gas totals |
| `bank_totals_rollup` | one row per network | transfer volume and unique participants |
| `bank_top_rollup` | one row per `(network, leaderboard, address)` | the transfer leaderboards, truncated per leaderboard |
| `active_addr_rollup` | one row per `(network, hour, kind, address)` | how many distinct addresses were active per bucket |

The `achievements` table is the same idea with a different shape: one row per
`(network, address, slug)`, holding the block that first earned that badge. The
catalog is `pkg/achievements` and every entry is a single `GROUP BY` over one of
the history tables above, none of which can be maintained incrementally without
being exactly right across a re-sync, a backfill or a chain reset. It is rebuilt
wholesale per network on its own, slower timer (`-achievement-interval`, ten
minutes by default) because a badge is a fact about the past and nobody watches
one arrive.

| table | grain | answers |
|---|---|---|
| `achievements` | one row per `(network, address, slug)` | what an address has done on chain, and when it first did it |

`active_addr_rollup` stores tuples rather than counts because counts cannot be
re-aggregated: an address active on three days of a week is one weekly active
address, not three. The hourly grain is finer than any bucket served, so every
granularity is an exact re-deduplication. Reads merge it with a live query over
everything newer than the build, so the newest bucket does not lag the timer.

## Semantics worth knowing

- **`network` IDs are labels.** They name a configured network and key its rows.
  They are not chain IDs and nothing enforces a relationship between the two — the
  same chain can be configured under any ID, and renaming an ID orphans the rows
  stored under the old one.
- **Sync cursors are derived, not stored.** The next fetch starts from the highest
  `block_height` already stored for that network. This means deleting rows rewinds
  the cursor, and it means a chain that resets to a lower height needs explicit
  handling.
- **Chain identity is fingerprinted** by the chain ID and hash of block 1, stored
  in `sync_state`. A network that resets keeps its chain ID and comes back with a
  different block 1, so the hash is what distinguishes one chain instance from the
  next.
- **Failed transactions are stored**, with `success = false`. Statistics that
  should exclude them must filter explicitly.
- **Import extraction is regex-based**, not a Go parse. It is fast and dependency
  free; it will not understand anything that requires type information.
- **`?network=all`, or omitting the parameter, means no filter** — results span
  every configured network. For anything address- or balance-related this is a
  known source of confusion, because those resolve against a single network.
- **Storage capacity is arithmetic, not an estimate.** gno.land locks
  `vm:p:storage_price` ugnot for every byte of realm state, so a chain cannot hold
  more bytes than its money supply can pay for:
  `capacity = total_supply / storage_price`. At mainnet's figures on 2026-09-21
  that is 1,333,000,221,686,563 ugnot over 100 ugnot/byte, or 13.33 TB, the same
  sum the monorepo does in a comment beside the default price. `/storage` is built
  on it, and every one of its figures is per chain for that reason.
- **Explanations are generated, and the generator may not add a fact.** A
  Discover event is three layers: what happened (SQL, wrong only if the indexer
  is), what it means, and why it matters. The last two are generated from a
  closed `facts` map and may not introduce a single value absent from it, which
  `pkg/discover` enforces mechanically: every number resolves to a fact through a
  declared conversion table, every name shaped like a path, address or handle
  appears in the facts, claim words like "first" and "biggest" need a fact that
  licenses them, future tense is banned because the chain records what happened
  and never what is next, and layer 2 carries no glossary headword at all. A
  reader cannot see which layer they are reading, so the generated ones have to
  be as safe as the queried one.
- **One place defines the words, and it is a document.**
  [`docs/glossary.md`](./glossary.md) is embedded in the binary and served parsed
  at `GET /api/glossary`, so the file a contributor edits and the tooltip a reader
  hovers are the same bytes. `pkg/glossary` enforces what the file claims about
  itself: two columns, at most three sentences, cross-references marked in bold
  and forming a DAG over defined headwords, and no gloss restated anywhere else in
  the repo. Adding a term is a docs change and a CI run, never a code change.
- **A per-row share of that capacity is unreadable, and that is the data, not
  the formatting.** At mainnet's 43.7 MB in use against 12.1 TB the largest
  namespace is 0.0002% of the disk and the rest are exponents, a column in which
  every row says the same thing. `/storage` states fullness once, for the whole
  chain, in the header; the table's second percentage is the running total of
  what is in use, which is the question a table sorted by size can answer. The
  ruler offers both scales rather than picking one: log fits the two numbers on
  one axis but reads as a third full, and max capacity is to scale but draws
  what is used at a floor of two pixels because the true width is 0.004 of one.
- **Local `storage_events` reproduce what the chain reports.** Summing
  `bytes_delta` per realm matches `vm/qstorage` exactly: checked on mainnet
  2026-09-21, `gno.land/r/gnoland/blog` sums to 1,278,609 here and the chain
  answers `storage: 1278609, deposit: 127860900`. Nothing needs a per-realm RPC
  round trip.
- **The chain charges the caller, not the realm.**
  `processStorageDeposit(ctx, caller, ...)` bills whoever sent the message, so a
  realm anyone can write to is paid for by its users and its deployer may hold
  almost none of its bytes. `/api/storage/map` reports both attributions
  separately rather than conflating them.
- **The URL is the view, filters included.** Every control that narrows a page
  writes itself into the query string, and every page seeds its controls from it
  on entry. The full map is in [The URL is the view](#the-url-is-the-view) below.

## The URL is the view

A filter is part of what the reader is looking at, so it belongs in the URL and
not only in a module variable. Every control that narrows a page writes itself
into the query string with `replaceState`, and every page seeds its controls from
the query string on entry, so a reload and a pasted link both land on what the
sender was actually looking at rather than on the unfiltered page the sender was
deliberately not reading.

| Page | Parameters |
|---|---|
| everywhere | `network`, and `embed=1` |
| any table of 8 rows or more | `f.<table>`, `s.<table>` (add `:desc` for descending) |
| `/packages`, `/accounts` | `pv`, `av` |
| `/directory/people` | `q`, `has` (comma-separated, an AND), `sort`, `named` |
| address detail | `tab` (`achievements`) |
| `/dashboards` | `section`, `window` |
| `/txs` | `type`, `status`, `page` |
| `/blocks` | `txs` |
| `/events` | `type`, `page` |
| `/govdao/proposals` | `status` |
| `/validators` | `failed` |
| realm detail | `tab`, and `file` / `line` / `fn` on the source tab |
| realm `?tab=calls` | `window`, `kind`, `status`, `func`, `caller`, `page`, `by` |
| realm `?tab=events` | `storage` |

Four rules hold across all of them:

- **Values are the reader's words, not the wire's.** `?type=deploy`, never
  `?type=MsgAddPackage`.
- **A default is deleted rather than spelled out**, so an untouched page has a
  clean URL and every parameter present means something was chosen.
- **`replaceState`, never `pushState`.** A filter box fires per keystroke, and a
  history entry per character makes the back button useless. Back leaves the page
  rather than undoing the filter.
- **`navigate()` pushes a bare path**, so a real navigation drops all of them and
  nothing has to clean up after itself.

A table is keyed on the slug of its header row, not on a positional index:
several pages draw their second table only when it has rows, so an index means a
different table depending on data the link's recipient may not have. The
contracts map and the realm dependency graph are the two surfaces not covered:
their controls are a viewport plus half a dozen knobs rather than a list filter.

## Non-goals

- Writing to chains, custody, or signing.
- Being a general-purpose indexer: mygnoscan is a cache over one, not a
  replacement for it.
- Historical state reconstruction. It stores what transactions say, not what state
  was at a given height.

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

## Non-goals

- Writing to chains, custody, or signing.
- Being a general-purpose indexer: mygnoscan is a cache over one, not a
  replacement for it.
- Historical state reconstruction. It stores what transactions say, not what state
  was at a given height.

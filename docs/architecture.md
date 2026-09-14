# Architecture

## Shape

One Go binary. SQLite on disk. Frontend compiled in via `go:embed`. No Node.js,
no separate frontend build, no external services beyond the tx-indexer it reads
from.

```
                    ┌──────────────┐
                    │  tx-indexer  │  GraphQL, one per network
                    └──────┬───────┘
                           │
        ┌──────────────────┼──────────────────┐
        │                  │                  │
   ┌────▼─────┐      ┌─────▼─────┐      ┌─────▼─────┐
   │  syncer  │      │    api    │      │ live feed │
   │ (per net)│      │ (handlers)│      │   (SSE)   │
   └────┬─────┘      └─────┬─────┘      └─────┬─────┘
        │                  │                  │
   ┌────▼──────────────────▼────┐             │
   │        SQLite (db.go)      │             │
   └────────────────────────────┘             │
                    │                         │
              ┌─────▼─────────────────────────▼─────┐
              │   frontend/index.html (go:embed)    │
              └─────────────────────────────────────┘
```

## Components

**`main.go`** — flags, config resolution, database open, one syncer goroutine per
configured network, route table, graceful shutdown. Thin by intent; the route
table is the bulk of it.

**`config.go`** — `NetworkConfig` (id, indexer URL, optional RPC URL) and
`ResolveConfig`, which decides where configuration comes from: an explicit
`-config` file, single-network flags, a `networks.json` in the working directory,
or built-in defaults. Incomplete or contradictory input is an error rather than a
silent fallback, because an instance pointed at the wrong chain looks healthy.

**`indexer.go`** — GraphQL client. One instance per network. Every query the
explorer needs lives here: transactions by various filters, blocks, latest height.

**`syncer.go`** — the background loop, one per network, every 30s. Each pass
checks chain identity, syncs blocks, then syncs packages, calls/sends, and
msg_runs. Cursors come from the highest stored height, so a pass only fetches
what is new.

Block sync is the one part with two cursors, both derived from the `blocks` table
itself: head sync walks forward from `MAX(height)` to the tip, and a backfill
walks backward from `MIN(height)`, bounded per pass so a multi-million-block
history spreads over many passes instead of stalling everything behind it.
Backward rather than forward, so the dashboard's recent window populates first.
The stored range stays contiguous, which is what lets both cursors be derived
rather than stored. The backfill stops at genesis, or at a pruned floor once a
single-block probe confirms the indexer really has nothing below it.

**`analyzer.go`** — takes `MsgAddPackage` source and extracts
`import "gno.land/..."` statements by regex, then writes package, file and
dependency rows.

**`db.go`** — schema, startup migrations, and every query. Large and not yet split
by domain.

**`api.go`** — HTTP handlers. Most read SQLite; some query the indexer live.

**`ws.go`** — Server-Sent Events. Polls the indexer for new blocks every 3s and
fans out to connected browsers. Starts polling on the first subscriber and stops
when the last one leaves.

**`frontend/index.html`** — the whole client: markup, CSS and vanilla JS in one
file, with D3 for the dependency graph. Routing is client-side; the server serves
`index.html` for any non-API path.

## Data flow

1. The syncer asks the indexer for transactions above its cursor.
2. `MsgAddPackage` messages go to the analyzer, which extracts imports and writes
   packages, files and dependency edges. `MsgCall`, `MsgRun` and `BankMsgSend`
   become `calls`, `msg_runs` and `bank_sends` rows. Every transaction also becomes
   a `transactions` row with its gas and success status.
3. Block times are fetched per unique height and stamped onto the rows. Blocks
   themselves are stored separately by the block sync above, with the proposer
   address interned into `proposers`, and back the block charts.
4. Handlers read SQLite, and for a few endpoints query the indexer or RPC live.
5. The frontend calls `/api/*` and renders.

## Design decisions

**SQLite on disk, not in memory.** Survives restarts, so a restart is not a full
re-sync, and it makes the recursive dependency queries and time-series aggregates
possible at all.

**Regex import extraction, not a Go parser.** Imports are the only thing needed,
they are syntactically trivial, and this keeps the binary free of the Go toolchain.
The tradeoff is no understanding of anything needing type resolution.

**Recursive dependency walking at query time, not materialized.** The graph is
small enough, and materializing it would need invalidation on every new deployment.

**Per-network syncer goroutines sharing one database.** Networks sync
independently at their own pace; the `network` column keeps them separate. An
application-level `RWMutex` guards writes, which is redundant with WAL and mostly
serializes writers — a known wart.

**MsgRun references by text search.** A `MsgRun` carries source, not a structured
import list, so a substring match is the only option. Accepts false positives.

**DOM construction, never HTML strings.** The explorer renders package names,
source and addresses straight from chain data, all of which is attacker-supplied.
A single `innerHTML` with interpolation would be an XSS hole, so the frontend uses
an `el()` helper throughout.

**Polling SSE rather than WebSockets.** The indexer has no subscription API worth
relying on, and SSE survives proxies without special configuration.

## Known weak points

Documented so they are not rediscovered as surprises:

- `db.go` (schema + migrations + all queries) and `api.go` are large and mix
  concerns.
- The startup migration path rebuilds tables to add columns, running against real
  data on every deploy.
- Several aggregate readers swallow errors and return zeroes rather than failing.
- Some analytics joins group by path without including `network`, risking
  cross-network double counting.
- Database methods do not take a `context.Context`, so a client disconnect does not
  cancel an expensive scan.
- Block-time stamping issues one indexer call per unique height per list request.
- The frontend is a single 3000+ line file with no component boundaries.

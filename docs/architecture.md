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
   │      SQLite (pkg/store)    │             │
   └────────────────────────────┘             │
                    │                         │
              ┌─────▼─────────────────────────▼─────┐
              │   pkg/web/frontend/index.html (go:embed)    │
              └─────────────────────────────────────┘
```

## Components

**`main.go`** — flags, config resolution, database open, one syncer goroutine per
configured network, route table, graceful shutdown. Thin by intent: everything
else lives under `pkg/`.

**`pkg/config`** — `NetworkConfig` (id, indexer URL, optional RPC URL) and
`ResolveConfig`, which decides where configuration comes from: an explicit
`-config` file, single-network flags, a `networks.json` in the working directory,
or built-in defaults. Incomplete or contradictory input is an error rather than a
silent fallback, because an instance pointed at the wrong chain looks healthy.

**`pkg/indexer`** — GraphQL client. One instance per network. Every query the
explorer needs lives here: transactions by various filters, blocks, latest height.

**`pkg/syncer`** — the background loop, one per network, every 30s. Each pass
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

**`pkg/analyzer`** — takes `MsgAddPackage` source and extracts
`import "gno.land/..."` statements by regex, then writes package, file and
dependency rows.

**`pkg/store`** — schema, startup migrations, and every query, split by domain:
`schema.go`, then `packages.go`, `accounts.go`, `transactions.go`,
`analytics.go`, `rollups.go`, `storage.go`.

**`pkg/httpapi`** — HTTP handlers, split the same way (`api_packages.go`,
`api_accounts.go`, `api_analytics.go`, `api_chain.go`). Most read SQLite; some
query the indexer live. `ws.go` here serves Server-Sent Events.

**`pkg/web`** — the embedded single-file frontend and the SPA handler that serves
index.html for any route it does not have a file for, so deep links work.

**Server-Sent Events** Polls the indexer for new blocks every 3s and
fans out to connected browsers. Starts polling on the first subscriber and stops
when the last one leaves.

**`pkg/web/frontend/index.html`** — the whole client: markup, CSS and vanilla JS in one
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

**Stale-while-revalidate on both sides, rather than a faster cold path.** The
explorer is read-only over data that moves once every 30 seconds, so "how fast is
this query" matters far less than "was anyone made to wait for it". Two layers
say no:

- *Server* (`pkg/httpapi/cache.go`): an expired entry is served immediately and
  refreshed behind the reader. Only an empty cache blocks. This is what removes
  the cliff where every visitor arriving more than 30 seconds after the last one
  paid the full cold cost — 1.8s on `/api/govdao/overview`, 7.8s on
  `/api/accounts`.
- *Client* (`apiSWR` in `index.html`): the last payload for every `/api` path is
  kept in `sessionStorage`, rendered the instant a view opens, and replaced when
  the network answers. A render function therefore runs up to twice, and has to
  be synchronous and rebuild its container from scratch. A header chip says when
  what is on screen came from cache, and a hairline under the header says when
  anything is in flight.

Cached *data*, never cached DOM: reviving a stored `innerHTML` would be the one
place the DOM-construction rule below stopped holding.

**Sections load independently where their sources differ in speed.** `/govdao`
is the worked example: its activity table reads local SQLite (~130ms) and its
proposal list waits on gov/dao's own `Render()` over RPC (~1.8s cold). They are
two `apiSWR` calls into two containers rather than one `Promise.all`, so the fast
half never waits for the slow one. Same shape on a realm page, where the events,
storage and dependency tabs each load on their own.

## Known weak points

Documented so they are not rediscovered as surprises:

- The startup migration path rebuilds tables to add columns, running against real
  data on every deploy.
- Several aggregate readers swallow errors and return zeroes rather than failing.
- Some analytics joins group by path without including `network`, risking
  cross-network double counting.
- Database methods do not take a `context.Context`, so a client disconnect does not
  cancel an expensive scan.
- Block-time stamping issues one indexer call per unique height per list request.
- The frontend is a single 3000+ line file with no component boundaries.

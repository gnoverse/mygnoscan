# AGENTS.md

Conventions and invariants for working in this repo. Read this first; it is meant
to stay stable. For what the system *is* and how it behaves, see [`docs/`](docs/)
— that changes far more often and is deliberately kept separate.

## What this is

A single Go binary that serves a gno.land block explorer with an embedded
frontend. No build step for the frontend, no Node.js in the shipped artifact.

## Layout

```
main.go       entrypoint: flags, wiring, HTTP routes
config.go     network configuration and flag resolution
indexer.go    GraphQL client for the tx-indexer API
db.go         SQLite storage: schema, migrations, all queries
analyzer.go   import extraction from .gno source, dependency graph building
syncer.go     background sync from tx-indexer into SQLite
api.go        REST API handlers
ws.go         SSE live feed (polls the indexer, fans out to browsers)
frontend/     static HTML/JS/CSS, embedded with go:embed
e2e/          browser tests (Node, development only — never in the binary)
docs/         project documentation
```

See [`docs/architecture.md`](docs/architecture.md) for how these fit together.

## Invariants

Break these and things go wrong in ways that are hard to see:

- **Everything is network-scoped.** Rows in `packages`, `package_files`,
  `dependencies`, `calls`, `msg_runs`, `bank_sends` and `transactions` all carry a
  `network` column, and it is part of the primary key or unique constraint. Any
  new query, join or aggregate must filter or group by `network`, otherwise data
  from two chains gets silently mixed. Joins on `pkg_path` alone are the usual way
  this goes wrong.
- **Sync is incremental and cursor-driven.** Cursors are derived from the highest
  stored `block_height` for that network, not stored separately. Anything that
  deletes or rewrites rows moves the cursor as a side effect.
- **`network` IDs are labels, not chain IDs.** They name a configured network and
  key its data. Renaming one orphans its existing rows.
- **The frontend builds DOM, never HTML strings.** Use the `el()` helper. There is
  no `innerHTML` with interpolated data anywhere, and it should stay that way —
  the explorer renders on-chain content, all of which is attacker-controlled.
  This is also why the optimistic-UI cache stores payloads and not rendered
  markup: a revived `innerHTML` would be the one place this stopped being true.
- **A block height is drawn by `blockWithAge`, never by `blockLink` alone.**
  A bare height answers "which block" and leaves "when" to a second page load,
  which is the question a reader of a table actually had. The shape is
  `1,234 (3d)`, and the age carries `data-age` so the 10s ticker refreshes it
  on a tab left open. The exception is a column that already has a timestamp
  beside it (`/blocks`, the home tx feed, the tx detail table): there the age
  is a second way of saying the same thing. `e2e/tests/block-age.spec.js`
  holds the line. An endpoint that returns a height and no time is the bug to
  fix, not a reason to drop back to `blockLink`.

- **An `apiSWR` render function runs up to twice, and must be synchronous.**
  Loaders paint cached data first and fresh data second, so a render has to
  rebuild its container from scratch (appending to something a previous pass
  filled is how you get two of everything) and must not `await` (an await
  reopens the interleaving that rebuilding exists to close). Kick long work off
  in an async IIFE with a generation guard, the way `renderTsCharts` does.
- **Anything attached to a painted row has to survive that row being replaced.**
  The corollary of the above, and the one that is easy to miss: the fresh pass
  throws away the rows the cached pass drew, so a one-off applied to them (a
  filter hiding rows, a sort reordering them, a highlight) is gone a moment
  later, leaving a control that says it is doing something it is not. The cache
  is `sessionStorage`, so the second render only happens on the *second* visit
  to a page, and on localhost the two often collapse into one, which makes this
  a bug that passes in isolation and fails in the suite. `enhanceTables` is the
  pattern to copy: register the work with `registerTableRestore` and let it be
  re-applied whenever a row turns up without the `data-tstate` mark. Delay the
  fresh fetch with `page.route` to test it, or the test proves nothing.
- **The nav is described twice, and a test keeps the two identical.** The rail
  is static HTML in `index.html`; the `NAV` table in the script beside it drives
  the `.pagenav` section strips and `route()`'s active-state bookkeeping. The
  rail is not generated from the table because the two CDN `<script>` tags at
  the bottom of the file are render-blocking, and a generated rail would make
  the whole navigation hostage to a reachable CDN. Add an entry to both, in the
  same order, or `TestRailMatchesNavTable` fails. Left to drift it fails
  silently: a rail entry missing from the table navigates fine and simply has no
  section strip.
- **Never commit the built binary.** `mygnoscan` and `*.db` are gitignored.

## Conventions

- **Go**, latest stable. Toolchain version comes from `go.mod`.
- **Formatting is enforced.** `gofmt -l .` must be empty; CI fails otherwise.
- **Tests are table-driven** where there is more than one case, with a temp
  SQLite file rather than a mock. The driver (`modernc.org/sqlite`) is pure Go, so
  a real database works everywhere including CI.
- **Commits are conventional and single-line**: `feat:`, `fix:`, `docs:`, `ci:`,
  `refactor:`, `test:`, `chore:`. No trailing co-author lines.
- **`make` targets** are the entry points: `test`, `e2e`, `run`, `install`, `dev`.
  `test` is Go only. `e2e` drives a headless browser and is the only thing in the
  repo that needs Node; changing anything in `frontend/` should run it.
- **Errors go up, not into logs.** The exception is the sync loop, which logs and
  continues per-item so one bad package cannot stall a whole pass. Do not copy
  that pattern into query paths — several aggregate readers currently swallow
  errors and return zeroes, and that is a known bug, not a style to follow.

## Before opening a PR

```bash
gofmt -l .        # must print nothing
go vet ./...
go test ./...
```

CI runs the same three plus `golangci-lint` and a multi-arch Docker build. See
[`CONTRIBUTING.md`](CONTRIBUTING.md).

## Gotchas

- `db.go` is large and mixes schema, migrations and every query. Adding to it is
  fine; just know that queries are not grouped by domain yet.
- The startup migration path rebuilds tables (`packages_new` and friends) to add
  columns SQLite cannot add in place. It runs against real user data on every
  deploy, so treat changes there as high-risk.
- `network` is string-concatenated into a few aggregate queries rather than bound
  as a parameter. It is quote-escaped, so not currently exploitable, but do not
  add more of it — bind parameters.
- Some `/api/*` endpoints query the indexer live on every request instead of
  reading local SQLite. Check which before assuming an endpoint is cheap.

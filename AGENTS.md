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

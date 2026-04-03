# mygnoscan

A fast, minimal blockchain explorer for [gno.land](https://gno.land) that actually shows useful data.

Built on the [tx-indexer](https://github.com/gnolang/tx-indexer) GraphQL API with a local SQLite cache for dependency analysis.

## Features

- **Realm inspector** — source, imports, dependents, callers, MsgRun references
- **Dependency graph** — interactive D3 visualization of what imports what
- **Transaction inspector** — full message details, events, errors
- **Usage tracking** — direct MsgCall, indirect imports from other contracts, MsgRun references
- **Smart caching** — SQLite stores computed dependency graphs and usage stats
- **Single binary** — Go backend with embedded frontend, no Node.js

## Quick start

```bash
make install
mygnoscan
# open http://localhost:8888
```

## Usage

```
mygnoscan [flags]
  -listen    listen address (default ":8888")
  -indexer   tx-indexer GraphQL endpoint (default "https://indexer.gno.land/graphql/query")
  -db        SQLite database path (default "mygnoscan.db")
  -sync      sync data from indexer on start (default true)
```

## API

| Endpoint | Description |
|---|---|
| `GET /api/stats` | Aggregate statistics |
| `GET /api/realms` | List realms (query: `limit`, `offset`) |
| `GET /api/packages` | List all packages |
| `GET /api/realm/{path}` | Realm/package detail with deps, calls, source |
| `GET /api/tx/{hash}` | Transaction detail |
| `GET /api/txs` | Recent transactions |
| `GET /api/address/{addr}` | Address activity |
| `GET /api/search?q=...` | Search packages by path, name, creator |
| `GET /api/deps/{path}` | Dependency graph (`?dir=dependents` for reverse) |

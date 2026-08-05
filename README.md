# mygnoscan

A fast, minimal blockchain explorer for [gno.land](https://gno.land) that actually shows useful data.

Built on the [tx-indexer](https://github.com/gnolang/tx-indexer) GraphQL API with a local SQLite cache for dependency analysis.

## Features

- **Realm inspector** — source, imports, dependents, callers, MsgRun references
- **Dependency graph** — interactive D3 visualization of what imports what
- **Transaction inspector** — full message details, events, errors
- **Usage tracking** — direct MsgCall, indirect imports from other contracts, MsgRun references
- **Multi-network** — track several chains in one instance and one database
- **Smart caching** — SQLite stores computed dependency graphs and usage stats
- **Single binary** — Go backend with embedded frontend, no Node.js

## Quick start

```bash
make install
mygnoscan
# open http://localhost:8888
```

By default it syncs the built-in default networks. To point it at one specific
indexer:

```bash
mygnoscan -indexer https://indexer.topaz.testnets.gno.land/graphql/query -network topaz
```

For several networks at once, use a config file:

```json
{
  "networks": [
    {"id": "topaz", "indexer": "https://indexer.topaz.testnets.gno.land/graphql/query", "rpc": "https://rpc.topaz.testnets.gno.land"},
    {"id": "betanet", "indexer": "https://indexer.betanet.testnets.gno.land/graphql/query", "rpc": "https://rpc.betanet.testnets.gno.land"}
  ]
}
```

```bash
mygnoscan -config networks.json
```

## Flags

```
mygnoscan [flags]
  -listen    listen address (default ":8888")
  -db        SQLite database path (default "mygnoscan.db")
  -config    JSON config file, for multiple networks
  -network   single network ID
  -indexer   single network tx-indexer GraphQL URL
  -rpc       single network RPC URL (needed for account balances)
  -sync      run the background sync (default true)
```

`-config` and the single-network flags are mutually exclusive. Startup logs which
configuration source was used and which networks resulted — worth checking, along
with `/api/networks`, after any config change.

## Docker

```bash
docker run -p 8888:8888 ghcr.io/gnoverse/mygnoscan:main
```

## Documentation

| | |
|---|---|
| [docs/spec.md](docs/spec.md) | what mygnoscan is, and its data model |
| [docs/architecture.md](docs/architecture.md) | components, data flow, design decisions |
| [docs/api.md](docs/api.md) | full HTTP API reference |
| [docs/development.md](docs/development.md) | local development |
| [docs/deployment.md](docs/deployment.md) | running it for real |
| [CONTRIBUTING.md](CONTRIBUTING.md) | how to contribute |
| [AGENTS.md](AGENTS.md) | conventions and invariants for code changes |

## API

The most-used endpoints — see [docs/api.md](docs/api.md) for all of them, including
analytics, time series and the SSE live feed.

| Endpoint | Description |
|---|---|
| `GET /api/networks` | Configured network IDs |
| `GET /api/stats` | Aggregate statistics |
| `GET /api/realms` | List realms (`limit`, `offset`) |
| `GET /api/packages` | List all packages |
| `GET /api/realm/{path}` | Realm/package detail with deps, calls, source |
| `GET /api/deps/{path}` | Dependency graph (`?dir=dependents` for reverse) |
| `GET /api/tx/{hash}` | Transaction detail |
| `GET /api/txs` | Recent transactions |
| `GET /api/address/{addr}` | Address activity |
| `GET /api/search?q=...` | Search packages by path, name, creator |
| `GET /api/live` | Live block/tx feed (SSE) |

Most endpoints accept `?network=<id>` to scope results to one network; omitting it
spans all of them.

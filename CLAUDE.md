# mygnoscan

## Architecture

Single Go binary with embedded frontend. No build step for frontend.

```
main.go       - entrypoint, HTTP server, route setup
indexer.go    - GraphQL client for tx-indexer API
db.go         - SQLite storage layer, queries, schema
analyzer.go   - import extraction from .gno source, dependency graph building
syncer.go     - background sync from tx-indexer to local SQLite
api.go        - REST API handlers
frontend/     - static HTML/JS/CSS (embedded via go:embed)
```

## Data flow

1. **Syncer** periodically fetches all transactions from tx-indexer GraphQL
2. **Analyzer** parses MsgAddPackage source files to extract `import "gno.land/..."` statements
3. **DB** stores packages, files, dependency edges, call records, MsgRun sources
4. **API** serves cached data from SQLite + live queries to tx-indexer
5. **Frontend** is a vanilla JS SPA with D3 for dependency graphs

## Key design decisions

- SQLite for local cache (not in-memory) — survives restarts, enables complex queries
- Import extraction via regex on .gno files — no Go parser needed, fast
- Dependency graph is recursive (walks imports of imports)
- MsgRun references found by LIKE search on stored source text
- Frontend uses safe DOM construction (el() helper) — no innerHTML

## tx-indexer endpoint

Default: `https://indexer.gno.land/graphql/query`

Queries: `getTransactions` (with filters), `getBlocks`, `latestBlockHeight`
Message types: MsgAddPackage, MsgCall, MsgRun, BankMsgSend

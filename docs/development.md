# Development

## Requirements

Go, version per `go.mod`. Nothing else — `modernc.org/sqlite` is pure Go, so there
is no CGO and no system SQLite, and the frontend needs no build step.

## Loop

```bash
make run
# http://localhost:8888
```

```bash
make dev     # same, with hot reload via goloop
```

`make install` puts the binary on your `PATH`. `make test` runs `go test ./...`.

Frontend changes are picked up on rebuild, since `frontend/` is embedded with
`go:embed`. There is no watcher for it on its own — `make dev` restarting the
binary is the whole story.

## Pointing it somewhere

By default it syncs the built-in default networks. To use one specific indexer:

```bash
go run . -indexer http://127.0.0.1:8546/graphql/query -network local
```

For more than one network, use a config file — see
[`deployment.md`](deployment.md) for the format and the full flag list.

To work against existing data without hitting an indexer at all:

```bash
go run . -db ./mygnoscan.db -sync=false
```

`-sync=false` is the flag to reach for when iterating on the frontend or on
handlers: no background sync, no network traffic, stable data.

## First sync

A full sync of a busy chain takes a while and makes a lot of indexer requests. For
day-to-day work, prefer a small testnet, or copy an existing database and run with
`-sync=false`.

## Tests

```bash
go test ./...
go test ./... -run TestResolveConfig -v
```

Tests use temp SQLite files rather than mocks, and an `httptest` server in place of
a real indexer. Table-driven where there is more than one case.

### Browser tests

Go tests cannot see the frontend break. Its failure mode is "the JSON was fine,
the JS threw", so there is a second suite that runs the real binary against a
seeded fixture and drives it in a headless browser:

```bash
make e2e
```

It is not part of `make test`, because it needs Node and a browser — neither of
which the binary ever does. Node lives in `e2e/` and nowhere else; the frontend
still has no build step and the shipped artifact still has no Node in it.

A failure leaves a screenshot, a video and a Playwright trace under
`e2e/test-results/`, and CI uploads the same directory. See
[`e2e/README.md`](../e2e/README.md).

## Before pushing

```bash
gofmt -l .        # must print nothing
go vet ./...
go test ./...
```

Same checks CI runs, plus `golangci-lint` and a multi-arch Docker build.

## Poking at it

```bash
curl -s localhost:8888/api/networks          # which chains am I actually serving
curl -s localhost:8888/api/version           # which build is this
curl -s localhost:8888/api/stats | jq
curl -s localhost:8888/api/sanity/overview | jq
curl -N localhost:8888/api/live              # watch the SSE feed
```

`/api/networks` is the first thing to check when data looks wrong — a
misconfigured instance syncs happily from a chain you did not intend.

The database is ordinary SQLite:

```bash
sqlite3 mygnoscan.db 'SELECT network, COUNT(*) FROM packages GROUP BY network;'
```

## Layout

See [AGENTS.md](../AGENTS.md) for file-by-file layout and the invariants to
respect, and [architecture.md](architecture.md) for how the pieces interact.

# Contributing

## Setup

Go, version per `go.mod`. That is all — no CGO, no system SQLite, no Node.js.

```bash
git clone https://github.com/gnoverse/mygnoscan
cd mygnoscan
make run      # http://localhost:8888
```

See [`docs/development.md`](docs/development.md) for the development loop, how to
point the binary at a specific indexer, and how to work offline against an existing
database.

## Make targets

| target | what it does |
|---|---|
| `make run` | run from source |
| `make dev` | run with hot reload (`goloop`) |
| `make test` | `go test ./...` |
| `make install` | install the binary |

## Before opening a PR

```bash
gofmt -l .        # must print nothing
go vet ./...
go test ./...
```

CI runs those three, plus `golangci-lint` and a multi-arch Docker build. A PR that
fails `gofmt` fails CI.

## Commits

Conventional, single line, imperative:

```
feat: add per-realm activity time series
fix: honor -indexer without -network
docs: document network scoping
ci: pin actions to commit SHAs
```

Prefixes in use: `feat`, `fix`, `docs`, `ci`, `refactor`, `test`, `chore`, `build`.

Keep commits atomic — a formatting sweep and a behavior change should not share a
commit.

## Code

Read [AGENTS.md](AGENTS.md) before a first change. The short version:

- **Everything is network-scoped.** New queries, joins and aggregates must filter or
  group by `network`. Joining on package path alone silently mixes chains.
- **Bind SQL parameters.** A few existing aggregate queries concatenate the network
  string; do not add more.
- **The frontend builds DOM, never HTML strings.** Use the `el()` helper. Everything
  rendered comes from the chain and is attacker-controlled.
- **Return errors, do not log and continue.** The sync loop is the deliberate
  exception, so one bad package cannot stall a pass.

## Tests

Table-driven where there is more than one case. Prefer a temp SQLite file over a
mock, and an `httptest` server over a fake client — the driver is pure Go, so real
databases work in CI.

Coverage is thin right now, so tests with new code are especially welcome. High-value
untested areas are listed in
[#14](https://github.com/gnoverse/mygnoscan/issues/14).

## Pull requests

- Say what changes and why. If it fixes a bug, describe the failure it produces.
- Link the issue (`Closes #N`).
- Call out behavior changes that affect running deployments — config handling and
  sync semantics both have live consequences.
- One concern per PR.

## Reporting bugs

Include the output of `/api/version` and `/api/networks`, the network in question,
and whether the instance uses a config file or flags. A surprising number of issues
turn out to be an instance pointed at a different chain than intended.

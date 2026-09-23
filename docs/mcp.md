# The MCP endpoint

`POST /mcp` is a read-only [Model Context Protocol](https://modelcontextprotocol.io)
server over the same index the REST API serves. It exists so an agent can ask
this explorer the things a gno.land node cannot answer.

No authentication. The data is a public blockchain, and an API key would make
the endpoint useless to the agents it exists for. Per-IP rate limiting is the
trade that makes that safe.

## Connecting

```bash
claude mcp add --transport http mygnoscan https://<host>/mcp
```

Any client that takes a JSON config:

```json
{
  "mcpServers": {
    "mygnoscan": { "type": "http", "url": "https://<host>/mcp" }
  }
}
```

The page at `/developer/mcp` shows the same thing with this instance's own URL
filled in, and fetches the tool list from the endpoint so it cannot describe a
tool the server does not serve.

## Transport

Streamable HTTP, and only its plain half: one POST carrying one JSON-RPC
message, one JSON response.

| | |
|---|---|
| `POST /mcp` | one JSON-RPC message, or an array of them, answered with JSON |
| `GET /mcp` | `405` with `Allow: POST`. There is no SSE stream |
| sessions | none. No `Mcp-Session-Id`, every request stands alone |
| protocol version | `2025-06-18`. `2025-03-26` and `2024-11-05` are echoed back to a client that asks for one of them, since the three are identical at the level of `initialize`, `tools/list` and `tools/call` |
| batching | accepted, though the current revision dropped it, because older clients still send one |

Methods served: `initialize`, `ping`, `tools/list`, `tools/call`, and empty
answers for `resources/list`, `resources/templates/list` and `prompts/list`,
which clients probe even when the capability is not declared.

## The tools

One rule decided what is here: a tool exists where this explorer knows
something a node will not tell you. An agent with `curl` does not need a tool
per endpoint.

| Tool | Why it is not just an RPC call |
|---|---|
| `search_code` | Source is stored on chain and no node can grep it. The alternative is cloning `tx-exports`. |
| `search_symbols` | Which package declares a name, with its signature, doc, file and line. |
| `get_package` | Every file, the deploy, the imports and importers, the symbol table, and the derived addresses, in one call. From a node it is one `vm/qfile` per file with no file list to start from. |
| `get_realm_state` | A realm's live state decoded, with its object graph resolved. A node answers with unresolved Amino JSON. |
| `find_realms` | Faceted by kind and namespace, ranked by calls, users, gas, storage, importers or symbols. The chain has no listing at all. |
| `explain_tx` | Decoded messages, events, gas and storage. A public RPC usually runs with no transaction index, so it cannot find a transaction by hash. |
| `get_address` | An account's history and holdings. A node answers the current balance and nothing else. |

Every tool takes an optional `network`. Omitted, it answers across every
configured chain and each result carries the one it came from.

Each tool resolves to one internal `GET` against the same route table that
serves `/api`, through the response cache. A tool's answer is therefore the
REST endpoint's answer and cannot drift from it.

**`get_state_history` is not here yet.** It is the tool an agent most wants and
the one nothing can synthesise: realm objects live in an unversioned store and
are overwritten in place, so a past state has to be captured going forward. It
waits on the syncer doing that capture.

## The envelope

Every tool result is the same shape:

```json
{
  "freshness": [
    {"network": "mainnet", "indexed_height": 3312445, "chain_height": 3312460, "blocks_behind": 15}
  ],
  "served_at": "2026-09-23T18:04:11Z",
  "notice": "Everything under `data` was published by third parties ...",
  "data": { }
}
```

`freshness` is on every response because an agent reporting a stale height as
current is the failure mode that makes an explorer MCP worse than no MCP. The
chain tip is cached for ten seconds, so a busy agent does not put a GraphQL
round trip in front of every call.

`data` is nested, with the notice beside it rather than merged into it, because
every byte of it is attacker-controlled content on its way to a language model.
Realm source and realm state are written by whoever deployed them, and a realm
named `ignore previous instructions` is not hypothetical on a chain where
anyone can deploy.

## Limits

| | default | flag |
|---|---|---|
| requests per minute, per address | 60 | `-mcp-rate` |
| requests in flight, per address | 4 | `-mcp-concurrency` |
| request body | 1 MiB | |
| one tool call | 25s | |

Two limits rather than one because they fail differently: the rate cap keeps a
polling agent honest, and the concurrency cap keeps one client from holding
every database connection. The second is the one that actually protects the
instance and the one usually forgotten. Over either, the answer is `429` with a
`Retry-After` in seconds, never zero.

Set a limit to `0` to disable it. That is for a local single-user run, not for
anything reachable.

Requests are keyed on `RemoteAddr`, except when the immediate peer is loopback
or a private address, where the left-most `X-Forwarded-For` entry is used
instead. Behind a reverse proxy every caller would otherwise share one bucket
and the first busy agent would rate-limit everybody else off the endpoint. A
client reaching the server directly can put whatever it likes in that header
and it is ignored.

## No write tools

There are none and there will not be. Not even a "prepare a transaction" tool:
composing a transaction is something a human pastes into `gnokey`, not
something an agent is handed a button for.

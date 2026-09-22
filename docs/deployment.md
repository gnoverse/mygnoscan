# Deployment

One static binary and one SQLite file. No runtime dependencies.

## Flags

| flag | default | description |
|---|---|---|
| `-listen` | `:8888` | listen address |
| `-db` | `mygnoscan.db` | SQLite database path |
| `-config` | — | JSON config file, for multiple networks |
| `-network` | — | single network ID |
| `-indexer` | — | single network tx-indexer GraphQL URL |
| `-rpc` | — | single network RPC URL, needed for account balances |
| `-sync` | `true` | run the background sync |
| `-block-history-days` | `90` | days of block history to backfill. `0` backfills the full chain; a negative value stores no blocks at all |
| `-analytics-script` | — | URL of an analytics script to load in the frontend. Empty serves no third-party script at all |
| `-gnoshot` | — | base URL of a [gnoshot](https://github.com/gnoverse/gnoshot) capture service. Empty draws no realm screenshots at all |

## Configuration

Config comes from exactly one of these, in order:

1. `-config <path>`
2. the single-network flags (`-indexer`, optionally `-network` and `-rpc`)
3. `networks.json` in the working directory
4. built-in defaults

Combining `-config` with the single-network flags is an error, as is passing
`-network` or `-rpc` without `-indexer`. Startup logs which source was used and the
resulting network IDs:

```
networks [pearl sapphire] (from config file)
```

Check that line, or `/api/networks`, after any config change. A wrongly configured
instance starts cleanly, syncs real data and looks healthy — it is just the wrong
chain.

### Config file

```json
{
  "networks": [
    {
      "id": "pearl",
      "indexer": "https://indexer.pearl.testnets.gno.land/graphql/query",
      "rpc": "https://rpc.pearl.testnets.gno.land"
    },
    {
      "id": "sapphire",
      "indexer": "https://indexer.sapphire.testnets.gno.land/graphql/query",
      "rpc": "https://rpc.sapphire.testnets.gno.land"
    }
  ]
}
```

`id` and `indexer` are required for every network, and IDs must be unique. `rpc` is
optional but **account balances need it** — an address on a network with no `rpc`
shows no balance.

Give each network an `rpc` if you can. Balance lookups resolve against a single
network, and with no network filter they fall back to the first configured network
that has one.

### Fallback endpoints

`indexers` and `rpcs` list additional interchangeable endpoints for the same
chain:

```json
{
  "id": "mainnet",
  "indexer": "https://indexer.gno.land/graphql/query",
  "indexers": ["https://indexer.example.org/graphql/query"],
  "rpc": "https://rpc.gno.land",
  "rpcs": ["https://rpc.example.org"]
}
```

They buy two different things, and the second is the one that matters:

- An endpoint that is **down** is skipped.
- An endpoint that is merely **behind** is overtaken. This is the failure that
  is worth configuring for, because it does not look like a failure: gno.land's
  mainnet indexer once sat at block 785 while the chain was at 36,000, answering
  every query promptly and correctly for the 785 blocks it knew about. Nothing
  errored; the explorer simply reported a stalled chain as a healthy one.

So endpoints are ranked by how far along they are, re-checked every couple of
minutes, and the singular `indexer`/`rpc` is just the first entry — listing a
stale endpoint first costs nothing.

Members must serve the same chain. The first entry that can identify itself
defines which chain that is (by chain ID *and* the hash of block 1, so a reset
network is not mistaken for the original), and any endpoint disagreeing with it
is never selected — however healthy or far along it is. A fast, healthy, wrong
chain is the worst member a pool can have: `gnoland-1` and `gnoland1` are one
hyphen apart.

## Analytics

Off by default, and deliberately: the frontend is one file compiled into the
binary and shared by every deployment, so a tag written into it would make
everyone running mygnoscan report to one account. `-analytics-script <url>`
adds a single `<script async src="…">` to the `<head>` at startup:

```
mygnoscan -analytics-script https://scripts.simpleanalyticscdn.com/latest.js
```

The URL must be an absolute `http(s)` URL; anything else is a startup error
rather than a broken tag served to every reader. Startup logs the line
`analytics: frontend loads <url>` when one is configured, and the page's ETag
changes with it, so readers holding the previous build get the new document
instead of a cached one.

[Simple Analytics](https://www.simpleanalytics.com) is what the public instance
uses. It sets no cookie and writes nothing to the device, so it needs no consent
banner. Any provider shipping a single self-contained script drops in the same
way.

Two things to know before reading the numbers, both properties of the frontend
rather than of the provider:

- **A pageview is a `pushState`.** The script patches `pushState` and listens
  for `popstate` and `hashchange` (verified against `latest.js` v11,
  2026-09-21), which is exactly what `navigate()` calls. Tab switches inside a
  realm page use `replaceState` and are correctly *not* counted as separate
  views.
- **The network is in the query string, and query strings are dropped.**
  `/realms?network=mainnet` and `/realms?network=pearl` arrive as one page. Per
  network figures need the provider's own parameter allow-list, not a code
  change here.

## Choosing network IDs

The `id` labels the network and keys every row belonging to it. It is not the chain
ID and nothing ties the two together.

**Renaming an ID orphans the data stored under the old one.** Since sync cursors
are derived from the highest stored height per network, a renamed network looks
brand new and re-syncs from genesis. To relabel while keeping history, update the
`network` column across all nine network-scoped tables (`packages`,
`package_files`, `dependencies`, `calls`, `msg_runs`, `bank_sends`,
`transactions`, `blocks`, `proposers`) — that preserves the cursor too. Back up
first.

## Reset-prone networks

Portal-loop and staging style chains restart from a low height. mygnoscan detects
this by fingerprinting block 1 (chain ID plus hash) per network: when the
fingerprint changes, that network's rows are discarded and it re-syncs from the new
genesis. Chain ID alone is not enough — a reset chain keeps its chain ID.

A lagging indexer replica reporting a tip below the stored height is *not* treated
as a reset. It logs a warning and changes nothing.

## Running it

Any process supervisor works. A systemd unit needs nothing special:

```ini
[Unit]
Description=mygnoscan
After=network.target

[Service]
ExecStart=/usr/local/bin/mygnoscan --listen 127.0.0.1:8888 --db /var/lib/mygnoscan/mygnoscan.db --config /etc/mygnoscan/networks.json
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

Put it behind a reverse proxy for TLS. `/api/live` is Server-Sent Events, so
disable response buffering for it (nginx: `proxy_buffering off`); Caddy's default
`reverse_proxy` needs no change.

## Docker

A multi-arch image is published to GHCR on every push to `main`:

```bash
docker run -p 8888:8888 \
  -v mygnoscan-data:/data \
  ghcr.io/gnoverse/mygnoscan:main \
  --listen :8888 --db /data/mygnoscan.db --config /data/networks.json
```

Prefer the image over hand-copied binaries. A deployment updated by scp drifts, and
a stale binary is hard to notice: check `/api/version`, and if `git_hash` is `dev`
the build carries no version information at all.

## Operating notes

- **Sync runs every 30s per network**, incrementally, from the highest stored
  height. Restarts do not re-sync from scratch.
- **A full first sync of a busy chain is expensive** in time and indexer requests.
  Adding a network to an existing instance triggers one for that network only.
- **Use a local indexer where you have one.** It is faster and avoids depending on
  a public endpoint.
- **The database grows with source code**, since full `.gno` file bodies are stored.
  Expect tens of MB per busy network.
- **Blocks cost roughly 130 bytes each** including their index — about **430 MB per
  network** at mainnet's ~3.3M blocks, on top of the source-code storage above.
  `-block-history-days` bounds **the initial backfill depth only** — how far back
  it walks from the tip before stopping. It does not bound total storage: head
  sync keeps appending new blocks at the tip for as long as the process runs, and
  nothing ever deletes a stored block, so a server run for a year at
  `-block-history-days=90` ends up holding a year of blocks *plus* the original 90
  days, not 90 days. The default of 90 keeps the initial backfill to what the
  dashboards' default window actually shows, `0` backfills the whole chain, and a
  negative value declines block storage entirely (the block charts then render
  empty). The startup log line says which mode is in effect.
  **Lowering the flag later reclaims nothing** — existing rows are never pruned.
  **Raising it** (including to `0`) past the depth an earlier, capped backfill
  already completed at makes that backfill resume from where it stopped, walking
  further back automatically; the change takes effect on the next sync pass, no
  manual intervention needed.
- **The block backfill runs automatically** on startup, bounded per pass so it
  cannot stall the rest of the sync. It walks backward from the tip and takes
  roughly 16 minutes to cover a mainnet-sized chain at `-block-history-days=0`.
  It logs its position each pass and its termination reason — genesis, a pruned
  indexer floor, or the configured depth. Until it finishes, block charts cover
  only recent history and say so; `/api/blocks/coverage` reports the stored range
  and whether it is complete.
- **WAL mode is on**, so back up the `-wal` and `-shm` files alongside the database,
  or take the backup with the service stopped.

## Health checks

```bash
curl -s localhost:8888/api/version    # which build
curl -s localhost:8888/api/networks   # which chains
curl -s localhost:8888/api/stats      # is data actually landing
```

In the logs, per-pass `synced N packages` lines with small counts mean incremental
sync is working. Large counts on every pass mean it is re-syncing everything, which
is the signature of a build predating incremental sync.


## Realm screenshots

Off unless `-gnoshot` names a capture service. With one, `/api/shot` turns a
package path into a picture of that realm's gnoweb page, and the frontend puts
it on the realm page and in the `/realms` and `/packages` listings.

```bash
# on the same box, two workers, warmed from this explorer's own path list
gnoshot serve -root /var/lib/gnoshot -source http://127.0.0.1:8888
mygnoscan -gnoshot http://127.0.0.1:8890
```

Three things worth knowing before turning it on:

- **A network needs a `gnoweb` in its config** to be photographable. The
  built-in defaults set it for `gnoland1` and `pearl`; a network without one
  simply gets no pictures, rather than an error on every row.
- **The proxy is on this origin on purpose.** A listing opens fifty thumbnails,
  and pointing them at another host costs a DNS lookup and a TLS handshake
  before the first byte of the first one. It is also the only place the
  parameter validation can live.
- **The capture service being down is not this being down.** `/api/shot`
  answers 503 with `Cache-Control: no-store`, and the page draws its own tile.
  Nothing else on the page changes.

The frontend learns whether the feature is on from a flag injected into the
document at serve time, not from `/api/version`: the first listing row is drawn
before that request comes back, and a page that grows a column afterwards is
worse than either outcome on its own.

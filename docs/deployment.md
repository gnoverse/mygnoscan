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
networks [topaz betanet] (from config file)
```

Check that line, or `/api/networks`, after any config change. A wrongly configured
instance starts cleanly, syncs real data and looks healthy — it is just the wrong
chain.

### Config file

```json
{
  "networks": [
    {
      "id": "topaz",
      "indexer": "https://indexer.topaz.testnets.gno.land/graphql/query",
      "rpc": "https://rpc.topaz.testnets.gno.land"
    },
    {
      "id": "betanet",
      "indexer": "https://indexer.betanet.testnets.gno.land/graphql/query",
      "rpc": "https://rpc.betanet.testnets.gno.land"
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

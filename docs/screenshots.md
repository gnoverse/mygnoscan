# Screenshots

The images in `README.md` and elsewhere in `docs/` are generated, not pasted, so
they cannot silently drift from the UI.

## Regenerating

```bash
DB=/path/to/mygnoscan.db ./scripts/screenshots.sh
```

Rerun after any change that alters the home, realms, transactions, analytics or
blocks views, and commit the result alongside the change.

## How it works

The script runs the real binary against a database snapshot with **syncing
disabled**, then drives headless Chrome over the pages:

- `-sync=false` means no indexer traffic and no moving data, so two runs over the
  same snapshot produce the same images.
- `-config testdata/screenshots-networks.json` pins the network list. Without it
  the binary falls back to its built-in defaults, the network selector renders
  empty, and the header shows a block height belonging to a different chain than
  the rows beneath it.
- A fixed 1400x900 window keeps framing stable between runs.
- The script waits for `/api/version` to answer rather than sleeping a guessed
  interval, and gives the SPA a virtual-time budget to finish fetching before the
  capture.

## Configuration

| variable | default | meaning |
|---|---|---|
| `DB` | `mygnoscan.db` | database snapshot to render |
| `OUT` | `docs/images` | output directory |
| `PORT` | `8899` | port for the temporary server |
| `NETWORK` | `topaz` | network to select in the UI |
| `CONFIG` | `testdata/screenshots-networks.json` | network list |
| `WIDTH` / `HEIGHT` | `1400` / `900` | window size |
| `CHROME` | auto-detected | path to Chrome or Chromium |

## Getting a snapshot

Any database with real data works. Use a local instance that has synced, or copy
one from a deployment — with SQLite in WAL mode, take it via the backup API rather
than copying the file out from under a running process:

```bash
python3 -c 'import sqlite3
src = sqlite3.connect("/path/to/live.db"); dst = sqlite3.connect("/tmp/snapshot.db")
src.backup(dst); dst.close(); src.close()'
```

Pick a network with enough activity to make the tables and charts look like
something; an empty chain produces empty screenshots.

## Not yet automated

CI does not regenerate or verify these — see
[#17](https://github.com/gnoverse/mygnoscan/issues/17). The intent is to upload
freshly generated images as artifacts on PRs touching `frontend/`, so a reviewer
can see the visual effect of a change without checking the branch out.

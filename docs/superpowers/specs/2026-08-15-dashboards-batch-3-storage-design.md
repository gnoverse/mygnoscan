# Dashboards Batch 3 — Storage Economics Design

Batch 3 of the chain-analytics dashboards: a `storage_events` table and the four charts it unlocks.
Extends [`2026-08-13-chain-analytics-dashboards-design.md`](2026-08-13-chain-analytics-dashboards-design.md),
whose §5 roadmap row for batch 3 this document replaces, and whose §10 and §11 carry-forward requirements
it honours.

---

## 1. What measurement changed about this batch

The roadmap sized batch 3 as a persistence problem comparable to batch 2a's blocks. It is not.

**The syncer already fetches these events and throws them away.** `txFieldsLight` (`indexer.go:264`) — the
query the main sync loop runs on every pass via `GetTransactionsFromHeight` — already requests
`StorageDepositEvent` and `StorageUnlockEvent` with `bytes_delta`, `fee_delta`/`fee_refund` and `pkg_path`.
So there is no new indexer query, no new fetch cost, and no backfill-depth decision: the existing
transaction cursor governs coverage.

Volume is trivial. Measured against the live `gnoland1` indexer on 2026-08-15:

| Quantity | Value |
|---|---|
| Transactions on mainnet, all history | ~2,795 (`total_txs` at the tip) |
| Deposit events in a 201-transaction sample | 400 |
| Unlock events in **all** of mainnet history | **4** |
| Distinct `pkg_path` values in the sample | 23 |
| Largest number of storage events in one transaction | **58** |
| Transactions emitting 2+ events with the same `(kind, pkg_path)` | **13 of 201** |

The whole table is a few thousand rows.

### 1.1 The proposed primary key was wrong

The parent spec suggested `PRIMARY KEY (network, tx_hash, pkg_path, kind)`. The measurement above kills it:
13 of 201 transactions (6.5%) emit two or more events sharing both `kind` and `pkg_path`, so that key would
silently collapse them and under-count stored bytes. The key must carry the event's ordinal within the
transaction.

### 1.2 Sign convention, measured rather than assumed

- `StorageDepositEvent.bytes_delta` is **positive** — 0 negative out of 400 sampled.
- `StorageUnlockEvent.bytes_delta` is **negative** (−8192, −8176, −8162, −7162).

So `SUM(bytes_delta)` across both types is already a correct net figure with no `CASE`. Fees confirm the
recap's constant exactly — 14592 bytes → 1,459,200 ugnot, i.e. **100 ugnot/byte**, refunds at the same rate.

---

## 2. Decisions

| Decision | Choice | Rationale |
|---|---|---|
| Syncer placement | A separate `syncStorageEvents` pass with its own cursor | Matches how `syncCalls` and `syncMsgRuns` are filled, backfills naturally, and self-heals if the table is cleared. Piggybacking on `syncCalls`' walk costs zero extra fetching but leaves the table **permanently empty** on any database whose calls are already synced to the tip — which is every existing install — so it needs the separate backfill anyway. |
| Primary key | `(network, tx_hash, event_index)` | §1.1. Any key without an ordinal drops 6.5% of real transactions' events. |
| `bytes_delta` storage | Signed exactly as emitted | `SUM` nets correctly with no `CASE`, and a negative cumulative total stays visible as the signal it is. |
| Negative cumulative bytes | **Do not floor at zero** | With full history a cumulative sum cannot go negative, so a negative value means events are being summed against history the indexer pruned — a data problem worth surfacing. For the per-realm net-delta chart, negative is the entire point: it is what "this realm pruned state" looks like. Flooring erases both signals. |
| Fee storage | Store it, do not derive | It is exactly `abs(bytes) × 100 ugnot` today, but that rate is a chain parameter. |
| The source-bytes metric | **Retire and replace** | `GetStorageTimeSeries` sums `LENGTH(pf.body)` — deployed source-code bytes, not chain state. `/api/timeseries/storage` is repointed at the real metric; the sanity view's chart and realm selector migrate with it. |
| Chart scoping | `networkScoped: true` on all four | Realm paths collide across chains (`gno.land/r/gnoland/blog` exists on both configured networks), so merging their bytes under one label would be actively wrong. Scoping all four keeps the section consistent rather than half-aggregating. |
| Section | Economics | Where gas and fees already live, and where the recap files storage economics. Takes it to six cards. |

---

## 3. Schema

Additive, created in `initSchema`. No `ALTER`, no table rebuild, so the `packages_new` path `AGENTS.md`
flags as high-risk stays untouched.

```sql
CREATE TABLE IF NOT EXISTS storage_events (
  network      TEXT NOT NULL DEFAULT 'gnoland1',
  tx_hash      TEXT NOT NULL,
  event_index  INTEGER NOT NULL,   -- ordinal within the transaction's full event list
  block_height INTEGER NOT NULL,
  block_time   TEXT,
  pkg_path     TEXT NOT NULL,
  kind         TEXT NOT NULL,      -- 'deposit' | 'unlock'
  bytes_delta  INTEGER NOT NULL,   -- signed as the chain emits it
  fee_amount   INTEGER NOT NULL DEFAULT 0,
  fee_denom    TEXT,
  PRIMARY KEY (network, tx_hash, event_index)
);
CREATE INDEX IF NOT EXISTS idx_storage_events_time ON storage_events(network, block_time);
CREATE INDEX IF NOT EXISTS idx_storage_events_pkg  ON storage_events(network, pkg_path);
```

`event_index` is the ordinal in the transaction's **full** event list, not among storage events only — so it
stays stable if a future batch persists `GnoEvent` rows from the same list.

`block_time` is denormalized as on every other table, so charts bucket without a join.

---

## 4. Syncer

`syncStorageEvents(ctx)` in `SyncAll`, cursor derived from `MAX(block_height) WHERE network = ?` on
`storage_events` — per the convention that cursors come from the table they fill, not from separate state.
It walks via the existing `walkTransactions` + `GetTransactionsFromHeight`, exactly as `syncCalls` and
`syncMsgRuns` do.

For each transaction it scans `Response.Events`, keeps entries whose `__typename` is
`StorageDepositEvent` or `StorageUnlockEvent`, and records each with its index in the full list.

Writes go through a batched `UpsertStorageEvents(network, rows)` in one lock and one SQLite transaction,
following `UpsertTransactions`/`UpsertBlocks`. This is not an optimisation: one observed transaction emitted
58 events, and the comment on `UpsertTransactions` records that per-row writes made read requests queue
behind a backfill of a hundred rows.

A failed page logs and returns, resuming next pass — the sync loop's own convention, and the only place it
applies. Query paths still return errors up.

---

## 5. API

| Endpoint | Returns | Mode | Serves |
|---|---|---|---|
| `/api/timeseries/storage` *(repointed)* | `[{time, deposited, released, net}]` | A | cumulative growth; deposited vs released |
| `/api/storage/consumers` | `[{pkg_path, deposited, released, net}]` | B, top-N | treemap; net delta by realm |
| `/api/timeseries/storage/realms` *(repointed)* | `[pkg_path]` | — | the sanity view's selector |

`deposited` is the sum of positive deltas, `released` the sum of negative deltas (kept negative), `net`
their total.

### 5.1 Route precedence

`/api/storage/{path...}` already exists as a live per-realm endpoint, and `/api/storage/consumers` falls
inside that wildcard. Go 1.22's `ServeMux` resolves this deterministically — the literal pattern matches a
strict subset of the wildcard, so it is more specific and wins. The consequence is that a realm at exactly
`gno.land/consumers` would be shadowed; realms live under `/r/` and `/p/`, so this is theoretical, but the
precedence is pinned by a test rather than left to reasoning.

### 5.2 Empty-bucket semantics (parent spec §10.1)

| Endpoint | Empty bucket | Reason |
|---|---|---|
| `/api/timeseries/storage` | `0` | Sums of bytes. A bucket with no storage activity genuinely had zero net change, and nulling it would break the cumulative running sum. |
| `/api/storage/consumers` | n/a | Fixed top-N shape; no empty-bucket concept. |

---

## 6. Charts

Four cards added to the **Economics** section, all `networkScoped: true`.

| Chart | Viz | Mode | Window |
|---|---|---|---|
| Cumulative storage growth | area line | A | pinned `all` |
| Storage deposited vs released | stacked bar | A | global |
| Top storage consumers | **treemap** | B | global, top-N control |
| Net storage change by realm | diverging bar | B | global, top-N control |

The two mode-B charts read the same endpoint but keep **independent** `state.topN`, like any other pair of
cards — a shared control would couple two cards through the config in a way the render pipeline has no
concept of. The cumulative chart pins `all` for the same reason batch 1's cumulative transactions does: a
running total over a partial window understates the real figure.

The treemap is the first non-cartesian chart in this codebase: no `xAxis`/`yAxis`, so `dashCatAxis` and
`dashValAxis` do not apply and it needs its own option shape.

**Its labels and tooltip render `pkg_path`, which is attacker-controlled** — the same class of data that
produced the XSS fixed in batch 2b, where a function name reached ECharts' HTML tooltip renderer. The
tooltip must go through `dashTooltipNode`, and the treemap's own **label formatter** must be checked
separately, since it is a distinct formatter from the tooltip's.

---

## 7. Migration off the source-bytes metric

`/api/timeseries/storage` currently returns `{time, bytes_added, files_added, packages_added}` from
`LENGTH(pf.body)`. Repointing it changes the response shape, so two existing consumers migrate in this batch:

- `renderStorageChart` (`frontend/index.html` ~934) reads `bytes_added`/`files_added`/`packages_added` and
  must read `deposited`/`released`/`net`.
- The sanity view's realm selector (~1959) reads `/api/timeseries/storage/realms`, which now lists realms
  with storage **events** rather than realms with package files.

The realm-detail storage tab uses `/api/storage/{path...}`, which queries the indexer live and is unchanged.

---

## 8. Error handling

Inherited from earlier batches: per-card isolation, empty distinguished from failed, query paths returning
errors up rather than the zero-returning readers `AGENTS.md` calls a known bug.

**One failure mode is carried in deliberately rather than rediscovered.** Batch 2b shipped a Critical where
a single malformed `block_time` returned 500 from four endpoints: the column is nullable TEXT compared as a
string, so `'not-a-timestamp' >= '2026-07-15T…'` is true, `strftime` yields NULL, and `Scan` fails. Every
query here buckets on `block_time`, so all of them scan into `sql.NullString` and skip invalid rows **from
the start**, with a test.

---

## 9. Testing

Go, table-driven against a temp SQLite file:

- **The event-index key** — a transaction with two events sharing `kind` and `pkg_path` stores both. This is
  the test that would have caught the parent spec's proposed key, and it reflects 13 of 201 real
  transactions.
- **Sign convention** — deposits positive, unlocks negative, `SUM(bytes_delta)` nets correctly and may
  legitimately go negative.
- **Network isolation** — two networks holding the *same* `pkg_path`, asserting the consumer aggregates do
  not merge them. Realm paths genuinely collide across chains, and this is the invariant `AGENTS.md` says
  fails silently.
- **Malformed `block_time`** — skipped, not a 500.
- **Cursor derivation** from `MAX(block_height)`, and **batched upsert idempotency** seeded with a 58-event
  transaction, since that is the real observed maximum.
- **Route precedence** — `/api/storage/consumers` reaches its own handler, not the `{path...}` wildcard.

Frontend keeps the repo's no-test-runner constraint, verified by driving the running app: `getOption()` data
assertions per §10.3, plus batch 2b's additions — a screenshot for any card with a bar series, and
exercising any control twice including one forced failure.

**An XSS probe on the treemap** seeds a realm path containing a live payload and asserts it renders as
literal text with no injected element and no execution. Two chart types in two batches have now rendered
chain-authored strings; this probe should be a standing step for any chart displaying chain data, not a
one-off.

---

## 10. Scope boundary

**In:** `storage_events` table, `syncStorageEvents`, three endpoints (two repointed, one new), four charts,
and the sanity-view migration off source bytes.

**Out:** persisting generic `GnoEvent` rows (batch 5 — though `event_index` is defined against the full
event list so the two can share a numbering scheme), the network graphs of batch 4, and any change to the
live per-realm `/api/storage/{path...}` endpoint.

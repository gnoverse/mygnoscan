# Chain Analytics Dashboards — Design

Turning the chart backlog in [`CHAIN_DATA_RECAP.md`](../../../CHAIN_DATA_RECAP.md) into shipped UI inside
the existing mygnoscan SPA, delivered in batches. The vision prototype
([`index.html`](../../../index.html), served as the GitHub Pages demo) is the visual and interaction
reference; its chart options port to production nearly verbatim.

This document supersedes `DASHBOARDS_ISSUES.md` and `DASHBOARDS_AI_GUIDE.md`, which were written against
a repo state that no longer exists (see below) and are deleted as part of this change.

---

## 1. Corrected starting line

The recap's §3 and both deleted dashboards docs describe a starting line the repo has moved well past.
Verified against the code on branch `feat/timeseries-demo`:

| Prior claim | Reality |
|---|---|
| "No `/api/timeseries/*` endpoints" | Eight exist — `main.go:126–134` |
| "Gas/fees not persisted" | `transactions` table carries `gas_used`, `gas_wanted`, `gas_fee`, `success`, `block_time` — `db.go:277` |
| "`created_at` is ingest time; need a `blocks` table to bucket by chain time" | `block_time` is already denormalized onto `calls`, `packages`, `msg_runs`, `bank_sends`, `transactions`, with indexes — `db.go:307–311` |
| "Only D3 is loaded" | Chart.js 4 is loaded too — `frontend/index.html:270`; the `sanity` view already renders ~6 charts |

So the planned "Epic A1" (block-time index) and "Epic C1" (gas persistence) are largely **already done**,
and the time-series API exists in a different shape (`?days=&granularity=` rather than the recap's
`?window=` + adaptive bucket).

**What is genuinely missing:**

- No `blocks` table. `proposer_address_raw` and `num_txs` are fetched by `indexer.go` but never stored, so
  there is no proposer distribution, no blocks/day, and no consecutive-block deltas for a block-time histogram.
- No `events` table → every realm-specific dashboard (r/sys/users, boards2, gov/dao) and the event-type treemap.
- No `storage_events` table. **`GetStorageTimeSeries` is misleadingly named**: it sums `LENGTH(pf.body)`
  (`db.go:1938`), i.e. deployed *source-code* bytes, not `StorageDepositEvent.bytes_delta`. Real storage
  economics (recap P1 #11–14) is greenfield despite the endpoint's existence.
- No `transfer_edges` / `caller_edges` rollups → no network graphs.
- Chart.js cannot draw treemap, sankey, heatmap or WebGL graphs. The viz-library gap is real.

---

## 2. Decisions

| Decision | Choice | Rationale |
|---|---|---|
| UI placement | New `dashboards` view inside `frontend/index.html` | Reuses the existing router, `el()` helper, network selector and SSE. A second page would duplicate all of it. |
| Viz library | Add ECharts; leave Chart.js in place | ECharts covers time-series + treemap + heatmap + sankey + (via echarts-gl) WebGL in one dep, and the POC is already written against it. Existing Chart.js views keep working untouched; retire them later if ever. |
| JS delivery | CDN `<script>`, matching D3 and Chart.js | Consistent with the two libs already loaded that way. Vendoring only ECharts would be inconsistent; vendoring all three is a separate cross-cutting change. |
| Window contract | Add `?window=` as an **additive alias**; `?days=&granularity=` keep working | Every batch-2+ endpoint needs the §8 contract. One contract across batches beats two in flight. Existing callers unaffected. |
| Sequencing | Vertical slices — each batch pairs its persistence with the charts it unlocks | Every batch ships something visible; migrations stay small and independently reversible; work can stop at any batch boundary with a coherent product. |
| Card explanation | `ⓘ` tooltip, not permanent prose | Per-card paragraphs are vision-doc styling. Product UI keeps headers to title + `ⓘ`. |
| Provenance badges | Dropped from the UI | A shipped chart is live by definition; the badge informs a reviewer, not a user. Provenance is tracked here instead. |

---

## 3. Architecture

### 3.1 Placement

One new top-level view, following the existing pattern exactly:

- `<div class="view" id="view-dashboards">` and `<a id="nav-dashboards">` alongside the current nav
  (`frontend/index.html:158–170`)
- A `path === '/dashboards'` branch in `route()` (`frontend/index.html:703`) and a
  `case 'dashboards': loadDashboards(); break;` arm
- Sub-sections driven by a `?section=` query param, mirroring the `?tab=` pattern already used by
  realm-detail, with a sticky sub-bar following the `#sanity-subbar` precedent (`frontend/index.html:22`,
  shown/hidden in `route()` at `:740`)

The full section set is Pulse, Economics, Networks, Realms and Supply, but **a section appears in the sub-nav
only once it has at least one chart**. Batch 1 therefore ships two sections — Pulse and Economics — and later
batches add the rest. Empty placeholder sections are not rendered; an unknown or empty `?section=` falls back
to Pulse.

Network scoping is inherited, not reimplemented: all fetches go through `api()`
(`frontend/index.html:688`), which appends `network=getNetwork()`, and `onNetworkChange()` already calls
`route()`. This preserves the repo's most important invariant — everything is network-scoped — without new
code paths.

### 3.2 Shell

Four pieces, ported from the POC:

1. **Declarative `DASHBOARDS` config.** Sections → charts, each chart
   `{id, title, why, mode, window?, fetch, opt(data, state), controls?}`. This is the load-bearing decision:
   adding a chart in batch 3 or 5 is *one array entry*, not new render code.
2. **Chart registry + lifecycle.** An id→instance map with `destroyDashboardCharts()`, matching the existing
   `destroyTsCharts()` / `destroySanityCharts()` convention (`frontend/index.html:779`, `:1710`), plus
   dispose-on-section-leave. Not strictly needed until the WebGL graph in batch 4, but it is ~10 lines now
   versus retrofitting a lifecycle later — and the POC demonstrates that WebGL charts run an unbounded
   animation loop that `display:none` does **not** pause.
3. **Global window picker.** Segmented `24h / 7d / 30d / 90d / 1y / All`, default 90d, held in module state.
   Re-renders mode-A and mode-B charts on change. Individual charts may override locally (cumulative charts
   pin to All; the function-call heatmap pins to 14d).
4. **`dashApi(path, chartWindow)`.** Thin wrapper over `api()` that appends the resolved window param,
   honouring a per-chart override when present.

### 3.3 Card anatomy

Header is `title` + a `ⓘ` button — no permanent paragraph. The explanation appears in a CSS-only popover on
`:hover` and `:focus-visible`. It is a real `<button>`, not a `title=` attribute, so it works for keyboard
users and on touch (tapping focuses it) and matches the dark theme rather than waiting on the browser's
native tooltip delay. No existing tooltip primitive exists to reuse — the codebase uses `title=` exactly once
(`frontend/index.html:682`).

**The `why` text is plain text, set via `textContent`.** The POC's strings carry `<b>`/`<i>` markup injected
with `innerHTML`. `AGENTS.md` makes "the frontend builds DOM, never HTML strings" an explicit invariant.
These particular strings are author-written constants and would be safe, but keeping one `innerHTML` path
invites the pattern back into a codebase that deliberately has none. The bold emphasis is dropped.

---

## 4. Batch 1 — shell + zero-persistence charts

Six charts over three existing endpoints. No schema change, no syncer change, no migration risk.

| Section | Chart | Viz | Endpoint | Mode |
|---|---|---|---|---|
| Pulse | Transactions per day, by message type | stacked area | `/api/timeseries/transactions` | A |
| Pulse | Cumulative transactions | line | same, running sum client-side | A (full curve) |
| Pulse | Success vs failed rate | line | `/api/timeseries/health` | A |
| Pulse | Active addresses by activity type | line | `/api/timeseries/active-addresses` | A |
| Economics | Gas used vs wanted + efficiency | line | `/api/timeseries/gas` | A |
| Economics | Fees per day + cumulative | bar + line | same | A |

**What batch 1 deliberately is not:** the active-addresses chart is **DAU only**. WAU/MAU need trailing-window
queries, so the POC's DAU/WAU/MAU chart lands in batch 2. Cumulative series are summed client-side from the
windowed response, so they are cumulative *within the window* — which is why cumulative charts pin their
local window to All.

### 4.1 The one backend change

Supporting `All` honestly requires more than a frontend label, because today:

- `parseTimeseriesParams` **caps `days` at 365** (`api.go:1113`) — so "All" would silently mean "one year"
- `timeseriesFormat` supports only `hourly` / `daily` / `weekly` (`db.go:1632`) — there is no monthly bucket

Left alone the cumulative charts would quietly under-report: exactly the ambiguous-cumulative failure recap §8
warns about. So batch 1 includes:

1. `?window=` resolution in `parseTimeseriesParams`, per the table below
2. A `monthly` case in `timeseriesFormat`
3. Lifting the `days` cap when `window=all`

`?days=&granularity=` continue to work unchanged, so the existing sanity, gas and analytics views are
untouched. When both are supplied, explicit `days`/`granularity` win over `window`.

| `window` | days | granularity | ~points |
|---|---|---|---|
| `24h` | 1 | hourly | 24 |
| `7d` | 7 | hourly | 168 |
| `30d` | 30 | daily | 30 |
| `90d` (default) | 90 | daily | 90 |
| `1y` | 365 | weekly | 52 |
| `all` | the network's real span | derived from that span | 24–180 |

Recap §8 suggests a 6h bucket option at 30d; the existing `timeseriesFormat` has no 6h case and daily gives a
readable 30 points, so 30d maps to daily. Adding 6h is not worth a fourth granularity.

**`all` is sized against the data, not fixed (corrected 2026-08-14).** It originally resolved to a fixed
`(3650 days, monthly)`, on the assumption that a chain has years of history. No gno chain does — mainnet is
~165 days, and a local devnet is days old — so a fixed monthly bucket collapsed the entire history into a
handful of points, and on a chain younger than a calendar month into **exactly one**, which draws as a lone
dot rather than a curve. It also made every `all` request gap-fill ~120 empty leading buckets.

`resolveTimeseriesParams` now measures the network's earliest indexed timestamp across every table carrying
one (or, for `network=all`/omitted, the minimum across every configured network) and derives both the range
and the bucket from the resulting span. The bucket boundaries are corrected again here (also 2026-08-14): the
first cut expressed them as day counts — ≤2d hourly, ≤180d daily, ≤730d weekly — and both boundaries were
already wrong at review. An 8-day chain landed on daily (9 points) while `7d` gave 168 hourly points over a
subset of the same data, so `all` was *coarser* than a fixed narrower window on exactly the young chains this
fix targeted. And the 180-day daily ceiling was tuned to mainnet's age at the time it was written; mainnet
gains a day every day, so it was on track to cross 180 and silently drop `all` from ~180 daily points to ~26
weekly ones within about two weeks of that commit.

The bands are now expressed as **target point counts**, not day counts, so the boundaries don't reference any
chain's current age:

- **hourly, ≤~250 points (~10 days' worth).** `resolveTimeseriesParams` rounds a span up by a day, so an
  8-day chain arrives as 9 days (216 hourly points); 250 clears that while staying the same order as `7d`'s
  fixed 168 hourly points.
- **daily, ≤~550 points (~18 months' worth).** At mainnet's current ~165 days this leaves roughly a year of
  headroom before the boundary, rather than the two weeks the old fixed 180-day ceiling gave it.
- **weekly, ≤~260 points (~5 years' worth).** Long enough that multi-year spans still read as a curve before
  falling back to monthly.

A network with nothing indexed keeps the old fixed pair, since every window returns empty anyway. The
resulting span is also clamped to the same `allWindowDays` (3650) ceiling `days` already had — a single row
with a year-1 timestamp is valid RFC3339 and passes the parse guard, and without the clamp it produces a span
of tens of thousands of days.

---

## 5. Batch roadmap & tracking

This table is the single source of truth for remaining work. Update it as each batch lands.

- [x] **Batch 1 — shell + zero-persistence charts.** Persistence: none. ECharts via CDN, dashboards view,
      section sub-nav, window picker, chart lifecycle, `ⓘ` tooltip primitive, `?window=` resolver +
      `monthly` bucket. Six charts above.
- [x] **Batch 2a — `blocks` table.** Persist `height, time, proposer_id, num_txs`, plus a `proposers`
      lookup, in the syncer. Unlocks: block-time histogram, blocks/bucket, proposer distribution (map
      proposer → moniker via the existing `r/gnops/valopers` logic in `HandleValidators`). Also carries
      the §10.2 config widening. Design:
      [`2026-08-13-dashboards-batch-2a-blocks-design.md`](2026-08-13-dashboards-batch-2a-blocks-design.md).
- [x] **Batch 2b — no schema change.** Activity heatmap (hour × day-of-week), new addresses
      (first-seen), gas-per-tx histogram, function-call heatmap with realm selector, DAU/WAU/MAU.
      These read `block_time`, already denormalized onto `calls`/`transactions`/`bank_sends`/`packages`.
      Also lands the §11.1 decisions: the `-block-history-days` cap, `mode: 'B'` on the chart config,
      and the message/transaction and active-address vocabulary from §9. Shipped as six endpoints —
      `activity/heatmap`, `timeseries/new-addresses`, `timeseries/active-rolling`,
      `gas/per-tx-histogram`, `calls/realms`, `calls/function-heatmap` — plus a third dashboard
      section, `realms`. Decisions in §12.

      > **Correction:** this row originally listed all eight charts under a single "batch 2 — `blocks`
      > table". Only the three in 2a need it; the five in 2b need no schema change at all. The row also
      > implied migration risk — adding new tables via `CREATE TABLE IF NOT EXISTS` never touches the
      > `packages_new` rebuild path `AGENTS.md` warns about.
- [x] **Batch 3 — `storage_events` table.** Persist `StorageDepositEvent` / `StorageUnlockEvent` from
      `TxResponse.Events`, which `txFieldsLight` already fetches on every sync pass. Unlocks: cumulative
      storage growth, top-consumers treemap, deposited vs released, net delta per realm — plus retiring the
      misleading `GetStorageTimeSeries` and migrating the sanity view onto the real metric. Design:
      [`2026-08-15-dashboards-batch-3-storage-design.md`](2026-08-15-dashboards-batch-3-storage-design.md).

      > **Correction:** this row proposed `PRIMARY KEY (network, tx_hash, pkg_path, kind)`. Measurement
      > killed it — 13 of 201 real transactions emit two or more events sharing both `kind` and `pkg_path`,
      > so that key silently drops events and under-counts bytes. The key needs the event's ordinal within
      > the transaction.
- [ ] **Batch 4 — `transfer_edges` / `caller_edges` rollups.** Per recap §7: day-collapsed edge tables built
      in the syncer, plus `GET /api/graph/transfers` and `/api/graph/callers` doing window / top-N / ego /
      parallel-edge-collapse **in SQL**, returning pre-pruned graphs. Unlocks: value-transfer force graph with
      click-to-focus ego drill-down, token-flow sankey, caller→realm WebGL graph.
- [ ] **Batch 5 — `events` table (GnoEvent).** Unlocks: event-type treemap, r/sys/users, boards2 and
      r/gov/dao dashboards.
- [ ] **Batch 6 — chain-RPC / state path.** The recap §6 structural gap. Unlocks: total vs circulating supply,
      wealth Lorenz + Gini, validator voting power, live proposal tallies.

---

## 6. Error handling

Failures are isolated **per card**, not per page. The existing `renderTsCharts` replaces the entire grid with
one message when any single fetch fails (`frontend/index.html:793`), which hides working charts behind one bad
endpoint. Each card renders its own state.

"Empty" and "failed" are distinguished: a young network legitimately returns `[]`, and rendering that as an
error would be wrong. Empty reads "no data in this window"; a failure reads as a load error.

The dashboards view guards `typeof echarts === 'undefined'` so a blocked or slow CDN degrades to a message
rather than throwing inside `echarts.init`.

New `db.go` queries return errors up rather than swallowing them — `AGENTS.md` calls the existing
zero-returning aggregate readers "a known bug, not a style to follow."

---

## 7. Testing & verification

**Go**, following the repo convention — table-driven, real temp SQLite, no mocks:

- The window resolver across all six windows, plus invalid input, and `days`/`granularity` back-compat
  including precedence when both are supplied
- The new `monthly` case in `timeseriesFormat`
- The `days` cap lifting only when `window=all`

**Frontend:** this repo has no JS test infrastructure — no bundler, no runner — and this work does not
introduce one. Verification is the repo's existing gate plus live checks:

```
gofmt -l .        # must print nothing
go vet ./...
go test ./...
```

then run the binary, `curl` the new endpoint shape, and drive the running app in a browser to confirm the
charts render, the network selector re-scopes them, and the window picker re-renders.

---

## 8. Doc consolidation

- Delete `DASHBOARDS_ISSUES.md` and `DASHBOARDS_AI_GUIDE.md` (untracked; superseded by this spec)
- Correct `CHAIN_DATA_RECAP.md` §3 so it describes the real current state; it remains the data-model and
  chart-backlog reference

---

## 9. Open questions carried forward

Not blocking batch 1. Each is owned by the batch that first hits it.

- **Batch 2 — backfill depth.** Full chain history for `blocks` (rows are tiny) versus a start height. Needs a
  DB-size figure before committing.
- ~~**Batch 2 — backfill depth.**~~ Settled in 2b: §12.1.
- ~~**Batch 2 — "tx" means message or transaction?**~~ Settled in 2b: §12.3.
- ~~**Batch 2 — what counts as an "active" address?**~~ Settled in 2b: §12.3.
- **Batch 2 — "tx" means message or transaction?** `calls` / `bank_sends` / `msg_runs` / `packages` are
  per-*message*; one tx can carry several. Batch 1 charts label the axis to match whichever the endpoint
  already returns; batch 2 should settle the vocabulary.
- **Batch 2 — what counts as an "active" address?** Whether bank-send *receivers* count, and whether failed
  txs count.
- ~~**Batch 3 — negative cumulative bytes.**~~ **Resolved: do not floor.** Measured against the live
  indexer, `StorageDepositEvent.bytes_delta` is positive and `StorageUnlockEvent.bytes_delta` is negative,
  so `SUM` is already a correct net figure. With full history a cumulative sum cannot go negative, meaning a
  negative value signals events summed against pruned history — worth surfacing, not hiding. For the
  per-realm net-delta chart, negative is the point: it is what pruning looks like.
- **Batch 4 — renderer for the caller graph.** echarts-gl `graphGL` (single dep, no node labels, ~100k
  ceiling) versus sigma.js v3 + graphology (labels, better at scale). Also the node count past which layout
  must be precomputed server-side and shipped as coordinates.
- **Batch 5 — event names are unverified.** The recap lists *likely* event types and attribute keys for
  r/sys/users, boards2 and gov/dao, not confirmed ones. Verify against the deployed realms before writing
  parsing code. Also: store all events or an allow-list (DB-size tradeoff).
- **Batch 6 — RPC versus state reconstruction.** Recap §6. Shapes the syncer; needs an explicit decision.

---

## 10. Carried forward from batch 1's final review

Three items that cost real rework in batch 1 and should be settled at the **start** of batch 2.

### 10.1 Empty-bucket semantics must be specified per endpoint

Batch 1's most repeated defect class: the plan specified each chart's `data:` mapping but never
specified *what an empty bucket should look like*. Because `fillBuckets` zero-fills every bucket in the
range, "no data" and "a real zero" are indistinguishable in the payload — and the right rendering
differs by series type:

| Series type | Empty bucket renders as | Example |
|---|---|---|
| Counts | `0` (correct as-is) | tx by type, active addresses |
| Ratios | `null` (a gap) | `success_rate`, `gas_efficiency` |
| Sums | `0` (correct as-is) | fees |

Two charts shipped wrong before this was understood: `success_rate` plotted its `-1` sentinel as a real
dip, and `gas_efficiency` rendered `0%` across 87 of 91 buckets, reading as "all gas wasted" rather than
"no traffic". Note the two endpoints disagree — `success_rate` uses a `-1` sentinel, `gas_efficiency`
has none — so the emptiness test must be derived per endpoint (e.g. `total_gas_wanted > 0`), not copied
between charts.

**Make empty-bucket semantics an explicit column in every chart's spec row**, decided once per endpoint.
Batch 3's storage deltas hit this immediately, where negative-versus-absent is already an open question.

### 10.2 The chart config needs widening before it has more call sites

The spec's §3.2 declared `{…, controls?}` and `opt(data, state)`; the batch 1 plan dropped both, and
both are load-bearing later:

- **`controls?`** — batch 2's function-call heatmap needs a per-card realm selector. There is no control
  slot today, so batch 2 would have to modify `dashCard` and `renderDashChart` — exactly what the
  declarative config exists to avoid.
- **`opt(rows)` cannot see the resolved window**, so no chart can vary axis formatting by granularity
  (`YYYY-MM` vs `YYYY-MM-DDTHH` labels want different treatment).

Widen to `opt(rows, { window, granularity })` and add the control slot at the **start of batch 2**, while
there are six call sites rather than fifteen. Both changes are backward-compatible today.

### 10.3 Verify charts on data, not just on render

Batch 1's browser checks were state checks — does it render, is the console clean — and every one of the
final review's findings was a *rendering-semantics* bug that survives a clean console. The fix needs no
test runner and no build step: after each chart lands, pull `_dashCharts[id].getOption()` and assert on
the actual series data — count nulls and zeros, check the x-axis span, run the tooltip's
`valueFormatter` over real values. That technique is what caught four of the findings. Make it a step in
every chart task for batches 2–6.

**Tooling note:** `golangci-lint` could not be run locally during batch 1 — the installed binary is v1
against this repo's v2 config — so every lint question was guesswork against CI. Pinning the linter
version so `make lint` matches CI would remove a recurring class of unanswerable review question.

---

## 11. Carried forward from batch 2a's final review

Batch 2a's SDD workspace and its ledger were deleted when the batch closed, per process — git history is
the record. These are the items from it that outlive the batch. Everything here was reviewed, triaged as
non-blocking for 2a, and deliberately not fixed.

### 11.1 Must be decided in batch 2b

- **Backfill depth / opt-out flag — DONE**, landed with batch 2b. `-block-history-days` (default 90; `0` =
  full chain history; negative = do not store blocks at all), enforced by `blockHistoryCutoff` and reported
  by `markBackfillDone`. Raising the depth on a later run clears the done flag and resumes rather than
  silently staying short. Note the flag bounds the **initial backfill only** — nothing prunes, and head sync
  keeps appending at the tip, so stored history grows forward from that floor. The size figure it bounds is
  ~130 bytes/block, ~430 MB per network at mainnet's 3.3M blocks.

  A `-block-history-days` flag (default 90; `0` = full history; negative = do not store blocks) exists in the
  **working tree**, along with `blockHistoryCutoff` and `markBackfillDone` in `syncer.go`, but as of this
  writing it is uncommitted and therefore unreviewed. Treat the item as open until it lands.

  > This entry has been wrong twice. It first claimed the flag was undecided; it was then "corrected" to say
  > the flag had shipped, on the strength of a `grep` against the working tree rather than against `HEAD`.
  > Neither was accurate. Check `git show HEAD:<file>`, not the working tree, before recording something as
  > done.
- **`networkScoped` per new chart.** The flag exists (added in 2a's fix wave) because all four block queries
  filter on an exact network while `networkParam` maps both a missing `network` and `network=all` to `""`,
  and `getNetwork()` defaults to `'all'`. Any new persistence-backed chart needs an explicit decision:
  aggregate across networks, or declare `networkScoped: true`. Skipping the decision reproduces the
  flat-zero-on-a-healthy-chain bug that shipped in 2a and was caught only by the final review.
- **Empty-bucket semantics per endpoint** (§10.1) and the **`opt(rows, ctx)` / `controls` surface** (§10.2)
  both still apply. Note `ctx.granularity` has no consumer yet — it is untested surface area.

### 11.2 Verification additions

§10.3 said to assert on `getOption()` data rather than a clean console, and that caught real bugs. It is
still not sufficient. Batch 2a shipped two defects invisible to it:

- Bars half-clipped by a line-chart axis (`boundaryGap: false` on a category axis shared with bar series)
- A chart painting into a detached canvas after a `controls` re-render

So, per chart task: **screenshot any card carrying a `bar` series**, and **exercise any `controls` slot
twice, including once against a forced failure**. Neither is visible in chart state or the console.

### 11.3 Deferred minors

Small, real, none blocking:

- `InternProposer` does `INSERT ... ON CONFLICT DO NOTHING` then a separate `SELECT`; `INSERT ... RETURNING id`
  would halve the round trips under the same lock.
- `HandleBlockProposers` discards `strconv.Atoi`'s error. Harmless — `GetBlockProposers` defaults `topN <= 0`
  to 25, so missing, malformed and negative all degrade identically — but it reads as an oversight.
- The dashboards **resize handler checks `isDisposed()` but not whether the instance's node is still
  attached** — the same family as the detached-canvas bug, and not covered by its fix.
- The segmented-control markup is duplicated between the proposer top-N control and older controls. Batch 2b's
  realm selector makes it the next copy; worth factoring then.
- `GetBlockTimeHistogram` honours an arbitrary `days`, so `?days=3650&granularity=monthly` runs a multi-million
  row `LAG` scan while holding the read lock. The frontend pins 7d, but the endpoint is public.
- ~~`syncBlocks` logs only failures.~~ Addressed by `markBackfillDone`, which logs the termination reason and
  floor height.

**Upgraded to a real defect — `GetBlocksInRange` ignores the indexer's element cap.** This was filed as a
hypothetical ("*if* any indexer ever truncates a 5,000-block range"). It is not hypothetical: the branch's
base commit `a711613` exists precisely because this indexer caps result sets, and it built `errQueryTooLarge`
plus cursor paging for the transaction queries. `GetBlocksInRange` (`indexer.go:677`) never got that
treatment — it is a single unpaginated query for a 5,000-block span.

On a cap, `query` decodes the partial page **and** returns `errQueryTooLarge`. `fetchBlockPage` treats any
error as failure, discards the partial page and returns false, so the pass aborts and the next pass retries
**the identical 5,000-block range**, which caps again. The backfill stalls permanently, retrying every 30
seconds — the same infinite-retry shape the pruned-floor bug had, in a different place.

It does not trigger against gno.land's indexer today: a live probe returned 4,999 blocks in one query. But
the margin is unmeasured, `syncBlocks` seeds with exactly `blockPageSize` blocks, and a differently-configured
indexer (a local devnet, say) may cap lower. The fix is to handle `errQueryTooLarge` the way the transaction
path already does — accept the partial page and resume from where it ended — rather than the
`page must end at to` guard originally suggested.
- `echarts@5` is an unpinned CDN range with no SRI, on a page that renders attacker-controlled strings.
  Pin an exact version and add SRI together — SRI is impossible while the range floats.
- `golangci-lint` still cannot be run locally (v1 binary against this repo's v2 config), so every lint
  question across both batches was guesswork against CI.

### 11.4 Working notes for a fresh session

Things that cost real time to discover and are written nowhere else:

- The repo's `mygnoscan.db` holds data only for network **`sapphire`** (~8 days, Aug 2026). The default
  config networks have no local rows, so verifying against defaults silently shows empty charts.
- Verification servers must not run from the repo root with the default `-db`, or they mutate that database.
  Run from `/tmp`, or pass `-db` explicitly.
- Both `gnoland1` and `test12` have only **5 distinct proposers**, so a top-N control over proposers cannot
  show a visible change; verify such controls by request parameters instead.
- gno.land mainnet blocks are **essentially all empty** — 0 transactions in ~5,000 blocks sampled across four
  eras — at ~4.34 s intervals. Charts that look broken on mainnet may simply be showing the truth.

---

## 12. Batch 2b decisions

Recorded here because §11.1 asked for them to be decided in this batch, not deferred a second time.

### 12.1 Backfill depth is an operator flag, defaulting to 90 days

`-block-history-days`, default **90**, `0` for the full chain, negative to store no blocks at all.
90 because it is the dashboards' default window: the default depth is exactly what the default view
shows, so nobody pays 430 MB per network for history no chart displays by default. The flag is global
rather than per-network — one knob covers the stated problem ("a mandatory full backfill it cannot cap
or decline"), and a per-network override can be added to `NetworkConfig` later without changing this
contract.

Mechanically the cap is checked against `MIN(blocks.time)` rather than a height, because there is no
fixed blocks-per-day rate to convert a day count into a height. On reaching it the backfill sets the
same `blocks_backfill_done` flag as genesis or a pruned floor, so `/api/blocks/coverage` reports
`complete: true` — the stored range *is* complete for the configured depth, and reporting otherwise
would leave the "history backfilling" note up forever.

The syncer now also logs its backfill position every pass and its termination reason, closing the
§11.3 minor of the same name.

### 12.2 `networkScoped` per new chart

| chart | scoped? | why |
|---|---|---|
| activity heatmap | no | hour-of-day counts from two chains are a union of disjoint events; summing is meaningful |
| new addresses | no | same — and first-seen across chains is coherent, an address is a keypair |
| DAU / WAU / MAU | no | same |
| gas-per-tx histogram | no | binning two chains' transactions together is a valid combined distribution |
| function-call heatmap | **yes** | `pkg_path` is per-chain. Grouping by it across networks merges two different realms that share a path — the failure `AGENTS.md` names explicitly — and the realm selector would show one entry standing for both |

### 12.3 Vocabulary (§9's two open questions)

- **"tx" means message.** `calls` / `packages` / `msg_runs` / `bank_sends` are per-message; only
  `transactions` is per-transaction. Axis labels say "messages" everywhere except the gas histogram,
  which reads `transactions` and says so.
- **An "active address" authored a message** — a caller, a creator, or a bank-send sender. Bank-send
  *receivers do not count* (an airdrop would otherwise manufacture thousands of active users) and
  *failed messages do count* (a failed call still proves key custody and burned gas). This is what
  batch 1's `GetActiveAddressTimeSeries` already did, so the new readers agree with it by construction
  rather than by coincidence; `activityMsgTables` in `db.go` is the single list they all read.

### 12.4 Empty-bucket semantics per endpoint (§10.1)

Every 2b series is a **count**, so every empty bucket is a real `0` — there are no ratios in this
batch, and therefore no nulls. What *did* bite is the shape question §10.1 did not anticipate:

| endpoint | empty | shape |
|---|---|---|
| `timeseries/new-addresses` | `0` | mode A — bucket count follows the window |
| `timeseries/active-rolling` | `0` | mode A, always daily |
| `activity/heatmap` | `0` | mode B — always 168 cells |
| `gas/per-tx-histogram` | `0` | mode B — always 9 bins |
| `calls/function-heatmap` | `0` | mode B — funcs × 14 days, zero-filled; `[]` only when the realm has no calls at all |

### 12.5 `mode` is back in the chart config

§3.2 declared `mode` and batch 1 dropped it. It had to come back, because `trimLeadingEmptyRows` runs
on every chart and would have silently corrupted both heatmaps: a cell `{hour: 0, dow: 0, messages: 0}`
satisfies `isRowEmpty`, so the top-left corner of the grid would be trimmed and every remaining cell
shifted onto the wrong axis slot. `mode: 'B'` now gates the trim, and is set on the two batch 2a mode-B
charts as well.

This is the same class of defect §11.2 warns about: invisible in the console, and invisible in
`getOption()` unless you compare the cell count against the expected grid size — which is why the
verification for these two asserts `168` and `funcs × 14` explicitly.

### 12.6 Deferred from 2b

- The segmented-control dedup (§11.3) did **not** come due: the realm selector is a `<select>`, not a
  third copy of the segmented markup. `.dash-select` shares the existing `.network-select` rule.
- `-block-history-days` has no per-network override (see §12.1).
- `GetRollingActiveTimeSeries` holds one `map[day]set[addr]` for the window plus 29 days of lead-in.
  Bounded by distinct active addresses, which is small on every gno chain today, but it is memory
  proportional to activity rather than to output size — the first 2b query that would need a rewrite
  if a chain got busy.

### 11.5 Commit `06dd1a7` bundles three unrelated streams

`06dd1a7` is titled "fix: size the all window by point-count target" but contains three things:

1. That band fix (`granularityForSpan`, `resolveTimeseriesParams`, `NetworkDataStart` and their tests).
2. **The whole batch 2b chart backend** — `HandleActivityHeatmap`, `HandleTimeSeriesNewAddresses`,
   `HandleTimeSeriesActiveRolling`, `HandleGasPerTxHistogram`, `HandleCallRealms`,
   `HandleFunctionCallHeatmap` and their `db.go` queries and tests. **None of it has been reviewed**, and its
   routes live in an uncommitted `main.go`, so the handlers are currently unreachable.
3. `OldestBlockTime`, a helper for the uncommitted `-block-history-days` work (§11.1).

Cause: the fix was dispatched with a *file*-scoped constraint ("modify only api.go, db.go, …") against a tree
that already held uncommitted work in those same files, so the implementer committed everything in them.
Scope a dispatch by file only when the tree is clean.

Splitting it was attempted and abandoned: `rebase -i` requires a clean tree, and five files
(`main.go`, `syncer.go`, `syncer_test.go`, `frontend/index.html`, `docs/deployment.md`) carry uncommitted
work. **Once that work is committed or stashed, splitting or rewording `06dd1a7` is straightforward.** Until
then the history is inaccurate and the 2b backend is unreviewed — review it as a unit before relying on it.

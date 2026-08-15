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
| `all` | uncapped | monthly | N months |

Recap §8 suggests a 6h bucket option at 30d; the existing `timeseriesFormat` has no 6h case and daily gives a
readable 30 points, so 30d maps to daily. Adding 6h is not worth a fourth granularity.

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
- [ ] **Batch 2b — no schema change.** Activity heatmap (hour × day-of-week), new addresses
      (first-seen), gas-per-tx histogram, function-call heatmap with realm selector, DAU/WAU/MAU.
      These read `block_time`, already denormalized onto `calls`/`transactions`/`bank_sends`/`packages`.

      > **Correction:** this row originally listed all eight charts under a single "batch 2 — `blocks`
      > table". Only the three in 2a need it; the five in 2b need no schema change at all. The row also
      > implied migration risk — adding new tables via `CREATE TABLE IF NOT EXISTS` never touches the
      > `packages_new` rebuild path `AGENTS.md` warns about.
- [ ] **Batch 3 — `storage_events` table.** Persist `StorageDepositEvent` / `StorageUnlockEvent`
      (`bytes_delta`, fee, `pkg_path`) from `TxResponse.Events`; the fields already exist in `indexer.go`'s
      `TxEvent`. Unlocks: storage growth (cumulative area), top-consumers treemap, deposit-locked vs
      refunded, net storage delta per realm. Rename or retire the misleading `GetStorageTimeSeries`.
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
- **Batch 2 — "tx" means message or transaction?** `calls` / `bank_sends` / `msg_runs` / `packages` are
  per-*message*; one tx can carry several. Batch 1 charts label the axis to match whichever the endpoint
  already returns; batch 2 should settle the vocabulary.
- **Batch 2 — what counts as an "active" address?** Whether bank-send *receivers* count, and whether failed
  txs count.
- **Batch 3 — negative cumulative bytes.** Storage deposits predate some state; whether to floor the
  cumulative curve at zero.
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

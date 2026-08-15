# Chain Analytics Dashboards — Batch 1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship a new `dashboards` view in the mygnoscan SPA containing six ECharts time-series charts built entirely from endpoints that already exist, plus the `?window=` API contract they need.

**Architecture:** A new top-level view in the existing single-file frontend, driven by a declarative `DASHBOARDS` config array so later batches add charts as array entries rather than render code. Charts come from the existing `/api/timeseries/*` endpoints, which gain an additive `?window=` parameter resolving to the `(days, granularity)` pair they already take. No schema change, no syncer change.

**Tech Stack:** Go (stdlib + `modernc.org/sqlite`), vanilla JS with the repo's `el()` DOM helper, ECharts 5 from CDN. No bundler, no build step.

Spec: [`docs/superpowers/specs/2026-08-13-chain-analytics-dashboards-design.md`](../specs/2026-08-13-chain-analytics-dashboards-design.md)

## Global Constraints

Every task's requirements implicitly include these.

- **Everything is network-scoped.** All frontend fetches must go through the existing `api()` helper (`frontend/index.html:688`), which appends `network=getNetwork()`. Never build a `/api/` URL by hand.
- **The frontend builds DOM, never HTML strings.** Use `el()`. No `innerHTML` with interpolated data anywhere — this is an `AGENTS.md` invariant because the explorer renders attacker-controlled on-chain content.
- **No build step.** No bundler, no npm, no framework. ECharts loads from CDN via a `<script>` tag.
- **Backward compatibility is mandatory.** `?days=` and `?granularity=` must keep working exactly as they do today; the existing `analytics`, `gas` and `sanity` views depend on them and must not be touched.
- **Go gates before any commit:** `gofmt -l .` prints nothing, `go vet ./...` passes, `go test ./...` passes.
- **Commits are conventional and single-line** (`feat:`, `fix:`, `docs:`, `refactor:`, `test:`, `chore:`). **No co-author or attribution trailers.**
- **Go tests are table-driven** with a real temp SQLite file, never mocks.
- **No data-provenance badges** (`✅ LIVE` / `🟡 INDEX` / `🔴 RPC`) in the UI. The POC has them; the product does not.
- **Card explanation text is plain text set via `textContent`.** The POC's `why` strings contain `<b>`/`<i>` markup — strip it.

---

## File Structure

| File | Change | Responsibility |
|---|---|---|
| `db.go` | Modify `timeseriesFormat` (`:1632`), `bucketKey` (`:1654`) | Add the `monthly` bucket granularity |
| `db_test.go` | Add tests | Cover the monthly bucket and guard the `fillBuckets` stepping loop |
| `api.go` | Modify `parseTimeseriesParams` (`:1108`) | Resolve `?window=` to `(days, granularity)` |
| `api_test.go` | **Create** | Table-driven coverage of the window resolver |
| `frontend/index.html` | Modify: `<head>` CSS, nav, view divs, `<script>` tags, `route()` | The whole dashboards view |

`frontend/index.html` is already 3,489 lines. This plan appends a single self-contained `// --- Dashboards ---` block rather than restructuring, matching how `analytics`, `gas` and `sanity` are organised today.

**One deliberate divergence from the spec:** §3.1 describes the section sub-bar as sticky, following the `#sanity-subbar` precedent. That precedent requires measuring the header height in `route()` at runtime (`frontend/index.html:740-747`). This plan renders the sub-bar inside `#dashboards-content`, non-sticky, to avoid that coupling. Making it sticky later is a CSS-only change.

---

## Task 1: Monthly bucket granularity

The `All` window needs a monthly bucket. `timeseriesFormat` currently supports only `hourly`/`weekly`/daily-default, and `bucketKey` mirrors it. Both must learn `monthly`, and the step duration must be proven to advance — `fillBuckets` (`db.go:1665`) loops `cur = truncFn(cur.Add(step))`, so a step shorter than the longest month would truncate back to the same month and **hang the server in an infinite loop**.

**Files:**
- Modify: `db.go:1632-1663` (`timeseriesFormat`, `bucketKey`)
- Test: `db_test.go` (append)

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `timeseriesFormat("monthly")` returns `("%Y-%m", 31*24*time.Hour, <first-of-month truncation>)`; `bucketKey(t, "monthly")` returns `"2006-01"` format. Task 2 relies on the string `"monthly"` being a valid granularity.

- [ ] **Step 1: Write the failing tests**

Append to `db_test.go`:

```go
func TestTimeseriesFormatMonthly(t *testing.T) {
	sqlFmt, step, truncFn := timeseriesFormat("monthly")

	if sqlFmt != "%Y-%m" {
		t.Errorf("sqlFmt = %q, want %q", sqlFmt, "%Y-%m")
	}
	// Must be at least the longest month, or the fillBuckets loop below stalls.
	if step < 31*24*time.Hour {
		t.Errorf("step = %v, want >= 31 days", step)
	}

	got := truncFn(time.Date(2026, 3, 17, 9, 30, 45, 0, time.UTC))
	want := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("truncFn = %v, want %v", got, want)
	}
}

func TestBucketKeyMonthly(t *testing.T) {
	got := bucketKey(time.Date(2026, 3, 17, 9, 0, 0, 0, time.UTC), "monthly")
	if got != "2026-03" {
		t.Errorf("bucketKey = %q, want %q", got, "2026-03")
	}
}

// The fillBuckets loop advances with cur = truncFn(cur.Add(step)). If a monthly
// step ever truncates back into the month it started in, the loop never
// terminates and the request hangs. Walk two years, including a leap February.
func TestMonthlyStepAlwaysAdvances(t *testing.T) {
	_, step, truncFn := timeseriesFormat("monthly")

	cur := truncFn(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
	for i := 0; i < 24; i++ {
		next := truncFn(cur.Add(step))
		if !next.After(cur) {
			t.Fatalf("monthly step did not advance from %v (got %v)", cur, next)
		}
		cur = next
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -run 'Monthly' ./... -v`

Expected: FAIL. `TestTimeseriesFormatMonthly` fails on `sqlFmt = "%Y-%m-%d", want "%Y-%m"` (unknown granularity falls through to the daily default), and `TestBucketKeyMonthly` fails with `"2026-03-17", want "2026-03"`.

- [ ] **Step 3: Add the monthly cases**

In `db.go`, add a `case "monthly"` to `timeseriesFormat`, immediately before the `default:` arm:

```go
	case "monthly":
		// The step must exceed the longest month (31 days) so that
		// truncating cur.Add(step) always lands in the next month —
		// otherwise fillBuckets never advances.
		return "%Y-%m", 31 * 24 * time.Hour, func(t time.Time) time.Time {
			return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
		}
```

And add the matching arm to `bucketKey`, before its `default:`:

```go
	case "monthly":
		return t.UTC().Format("2006-01")
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -run 'Monthly' ./... -v`
Expected: PASS (3 tests).

- [ ] **Step 5: Run the full gate**

```bash
gofmt -l . && go vet ./... && go test ./...
```
Expected: `gofmt` prints nothing; vet and test pass.

- [ ] **Step 6: Commit**

```bash
git add db.go db_test.go
git commit -m "feat: add monthly bucket granularity to time-series queries"
```

---

## Task 2: `?window=` resolver

Add the recap §8 window contract as an **additive alias** over the existing `?days=&granularity=` parameters.

**Files:**
- Modify: `api.go:1108-1123` (`parseTimeseriesParams`)
- Test: `api_test.go` (**create**)

**Interfaces:**
- Consumes: the `"monthly"` granularity from Task 1.
- Produces: `parseTimeseriesParams(r)` keeps its existing signature `(days int, granularity string)` — all nine existing handlers call it unchanged. New package-level identifiers: `allWindowDays` (int const) and `windowSpecs` (map).

- [ ] **Step 1: Write the failing test**

Create `api_test.go`:

```go
package main

import (
	"net/http/httptest"
	"testing"
)

func TestParseTimeseriesParams(t *testing.T) {
	tests := []struct {
		name            string
		query           string
		wantDays        int
		wantGranularity string
	}{
		{"empty falls back to the historical default", "", 30, "daily"},
		{"window 24h", "window=24h", 1, "hourly"},
		{"window 7d", "window=7d", 7, "hourly"},
		{"window 30d", "window=30d", 30, "daily"},
		{"window 90d", "window=90d", 90, "daily"},
		{"window 1y", "window=1y", 365, "weekly"},
		{"window all is monthly and exceeds the 365 cap", "window=all", allWindowDays, "monthly"},
		{"window is case-insensitive", "window=ALL", allWindowDays, "monthly"},
		{"unknown window is ignored", "window=nope", 30, "daily"},
		// Back-compat: the existing analytics/gas/sanity views pass these and
		// must keep their exact behaviour.
		{"legacy days and granularity still work", "days=14&granularity=hourly", 14, "hourly"},
		{"legacy days still capped at 365", "days=5000", 365, "daily"},
		{"legacy invalid granularity still falls back to daily", "granularity=yearly", 30, "daily"},
		// Explicit parameters win over window.
		{"explicit days overrides window", "window=all&days=7", 7, "monthly"},
		{"explicit granularity overrides window and re-applies the cap", "window=all&granularity=daily", 365, "daily"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/api/timeseries/transactions?"+tt.query, nil)
			days, granularity := parseTimeseriesParams(r)
			if days != tt.wantDays {
				t.Errorf("days = %d, want %d", days, tt.wantDays)
			}
			if granularity != tt.wantGranularity {
				t.Errorf("granularity = %q, want %q", granularity, tt.wantGranularity)
			}
		})
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -run TestParseTimeseriesParams ./... -v`

Expected: FAIL to **compile** with `undefined: allWindowDays`.

- [ ] **Step 3: Implement the resolver**

In `api.go`, replace `parseTimeseriesParams` (`:1108-1123`) entirely with:

```go
// allWindowDays bounds the "all" window. gno.land's genesis is comfortably
// inside this, and a finite bound keeps the monthly bucket loop terminating.
const allWindowDays = 3650

// windowSpecs maps a spec §8 window name onto the (days, granularity) pair the
// time-series queries already take. See the design doc's window table.
var windowSpecs = map[string]struct {
	days        int
	granularity string
}{
	"24h": {1, "hourly"},
	"7d":  {7, "hourly"},
	"30d": {30, "daily"},
	"90d": {90, "daily"},
	"1y":  {365, "weekly"},
	"all": {allWindowDays, "monthly"},
}

// parseTimeseriesParams resolves the time range for a time-series request.
// ?window= is the current contract; ?days= and ?granularity= predate it and
// still work, and win when both are supplied.
func parseTimeseriesParams(r *http.Request) (days int, granularity string) {
	q := r.URL.Query()
	days, _ = strconv.Atoi(q.Get("days"))
	granularity = q.Get("granularity")

	if spec, ok := windowSpecs[strings.ToLower(q.Get("window"))]; ok {
		if days <= 0 {
			days = spec.days
		}
		if granularity == "" {
			granularity = spec.granularity
		}
	}

	if days <= 0 {
		days = 30
	}
	// The 365-day cap keeps hourly/daily/weekly bucket counts sane. The monthly
	// bucket exists precisely to span longer ranges, so it is exempt — but is
	// still bounded by allWindowDays.
	if days > 365 && granularity != "monthly" {
		days = 365
	}
	if days > allWindowDays {
		days = allWindowDays
	}

	switch granularity {
	case "hourly", "daily", "weekly", "monthly":
	default:
		granularity = "daily"
	}
	return
}
```

`strings` and `strconv` are already imported in `api.go` — no import changes needed.

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test -run TestParseTimeseriesParams ./... -v`
Expected: PASS (14 subtests).

- [ ] **Step 5: Verify end to end against real data**

```bash
go build -o /tmp/mygnoscan . && /tmp/mygnoscan -listen :8899 -sync=false
```

In a second shell:

```bash
curl -s 'localhost:8899/api/timeseries/transactions?network=gnoland1&window=90d' | head -c 300
```
Expected: a JSON array of points with `time` values in `YYYY-MM-DD` form.

```bash
curl -s 'localhost:8899/api/timeseries/transactions?network=gnoland1&window=all' | head -c 300
```
Expected: `time` values in `YYYY-MM` form (monthly buckets).

```bash
curl -s 'localhost:8899/api/timeseries/transactions?network=gnoland1&days=7&granularity=hourly' | head -c 300
```
Expected: unchanged legacy behaviour — `time` values in `YYYY-MM-DDTHH` form.

Stop the server when done.

- [ ] **Step 6: Run the full gate**

```bash
gofmt -l . && go vet ./... && go test ./...
```

- [ ] **Step 7: Commit**

```bash
git add api.go api_test.go
git commit -m "feat: accept ?window= on time-series endpoints"
```

---

## Task 3: Dashboards view scaffolding

Add the ECharts script tag, the nav entry, the view container and the router wiring. Deliverable: `/dashboards` is reachable and renders a placeholder, with a graceful message if the CDN is blocked.

**Files:**
- Modify: `frontend/index.html` — nav (`:158-170`), view divs (after `:241`), script tags (`:269-270`), `route()` (`:703-770`)

**Interfaces:**
- Consumes: `el()`, `clear()`, `navigate()`, `route()` — all existing.
- Produces: `loadDashboards()` (called by `route()`); DOM node `#dashboards-content`; nav anchor `#nav-dashboards`; view div `#view-dashboards`. Tasks 4–7 render into `#dashboards-content`.

- [ ] **Step 1: Add the ECharts script tag**

In `frontend/index.html`, after the Chart.js tag at `:270`, add:

```html
<script src="https://cdn.jsdelivr.net/npm/echarts@5/dist/echarts.min.js"></script>
```

- [ ] **Step 2: Add the nav entry**

In the `<nav>` block, after the `analytics` anchor (`:168`), add:

```html
      <a onclick="navigate('/dashboards')" id="nav-dashboards">dashboards</a>
```

- [ ] **Step 3: Add the view container**

After the `view-gas` div (`:241`), add:

```html
  <div class="view" id="view-dashboards"><main><div id="dashboards-content"></div></main></div>
```

- [ ] **Step 4: Wire the router**

In `route()`, after the `/analytics` branch (`:726`), add:

```js
  else if (path === '/dashboards') { view = 'dashboards'; }
```

In the same function's `switch`, after the `analytics` case (`:761`), add:

```js
    case 'dashboards': loadDashboards(); break;
```

- [ ] **Step 5: Add the minimal loader**

At the end of the `<script>` block, just before `</script>` (`:3487`), add:

```js
// --- Dashboards ---
function loadDashboards() {
  const root = clear('dashboards-content');
  if (typeof echarts === 'undefined') {
    root.appendChild(el('div', { className: 'dash-msg' },
      'charts unavailable — the ECharts library failed to load'));
    return;
  }
  root.appendChild(el('div', { className: 'dash-msg' }, 'dashboards placeholder'));
}
```

- [ ] **Step 6: Verify in the browser**

```bash
go build -o /tmp/mygnoscan . && /tmp/mygnoscan -listen :8899 -sync=false
```

Open `http://localhost:8899/dashboards`. Expected: the `dashboards` nav item is highlighted and the page reads "dashboards placeholder". Check the browser console has no errors, and confirm `echarts` is defined by typing `typeof echarts` in the console — expected `"function"` or `"object"`.

- [ ] **Step 7: Commit**

```bash
git add frontend/index.html
git commit -m "feat: add dashboards view scaffolding"
```

---

## Task 4: Card and info-tooltip primitives

Build the card shell and the `ⓘ` tooltip. Deliverable: a static demo card with a working hover/keyboard tooltip.

**Files:**
- Modify: `frontend/index.html` — CSS (after `:95`), the Dashboards script block from Task 3

**Interfaces:**
- Consumes: `el()` from Task 3's context; `#dashboards-content`.
- Produces: `infoTip(text) -> HTMLElement` and `dashCard(chart) -> HTMLElement`, where `chart` is `{id, title, why, wide?}`. `dashCard` creates the chart host `#dash-chart-<chart.id>`. Tasks 5–7 pass real chart objects to `dashCard`.

- [ ] **Step 1: Add the CSS**

In the `<style>` block, after the skeleton rules (`:95-99`), add:

```css
/* Dashboards */
.dash-grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(440px, 1fr)); gap: 16px; }
.dash-card { background: var(--bg2); border: 1px solid var(--border); border-radius: 4px; padding: 12px; }
.dash-card.wide { grid-column: 1 / -1; }
.dash-head { display: flex; align-items: center; gap: 6px; margin-bottom: 8px; }
.dash-title { font-size: 13px; color: var(--fg); }
.dash-chart { width: 100%; height: 300px; }
.dash-msg { color: var(--fg2); font-size: 12px; padding: 20px 0; }
.dash-bar { display: flex; align-items: center; gap: 16px; flex-wrap: wrap; margin-bottom: 12px; }
.dash-grp { display: inline-flex; align-items: center; gap: 6px; }
.dash-bar .label { font-size: 11px; color: var(--fg2); text-transform: uppercase; letter-spacing: 1px; }
.dash-seg { display: inline-flex; border: 1px solid var(--border); border-radius: 4px; overflow: hidden; }
.dash-seg button { background: var(--bg2); color: var(--fg2); border: none; border-left: 1px solid var(--border); padding: 4px 10px; cursor: pointer; font-family: var(--mono); font-size: 11px; }
.dash-seg button:first-child { border-left: none; }
.dash-seg button.on { background: var(--accent); color: var(--bg); font-weight: bold; }
/* Info tooltip: a real button so it works on keyboard and touch, not title= */
.info { position: relative; display: inline-flex; }
.info-btn { background: none; border: 1px solid var(--border); color: var(--fg2); border-radius: 50%; width: 16px; height: 16px; font-size: 10px; line-height: 1; padding: 0; cursor: help; font-family: var(--mono); }
.info-btn:hover, .info-btn:focus-visible { color: var(--accent); border-color: var(--accent); }
.info-pop { display: none; position: absolute; top: 22px; left: 0; z-index: 300; width: 300px; background: var(--bg3); border: 1px solid var(--border); border-radius: 4px; padding: 8px 10px; font-size: 11px; line-height: 1.5; color: var(--fg2); }
.info:hover .info-pop, .info-btn:focus-visible + .info-pop { display: block; }
```

- [ ] **Step 2: Add the builders**

In the Dashboards script block, above `loadDashboards`, add:

```js
function infoTip(text) {
  const btn = el('button', { type: 'button', className: 'info-btn', 'aria-label': 'what this chart shows' }, 'i');
  const pop = el('span', { className: 'info-pop', role: 'tooltip' }, text);
  return el('span', { className: 'info' }, btn, pop);
}

function dashCard(chart) {
  const head = el('div', { className: 'dash-head' }, el('span', { className: 'dash-title' }, chart.title));
  if (chart.why) head.appendChild(infoTip(chart.why));
  const host = el('div', { className: 'dash-chart', id: 'dash-chart-' + chart.id });
  return el('div', { className: 'dash-card' + (chart.wide ? ' wide' : '') }, head, host);
}
```

Both build DOM through `el()`, so the `why` text lands via `createTextNode` — no `innerHTML`, per the invariant.

- [ ] **Step 3: Render a demo card**

Replace the placeholder line in `loadDashboards` with:

```js
  const grid = el('div', { className: 'dash-grid' });
  grid.appendChild(dashCard({
    id: 'demo',
    title: 'demo card',
    why: 'This card proves the tooltip primitive works. It is removed in the next task.',
  }));
  root.appendChild(grid);
```

- [ ] **Step 4: Verify in the browser**

Rebuild and reload `http://localhost:8899/dashboards`. Expected:
1. A card titled "demo card" with a small `i` button beside it.
2. Hovering the `i` shows the explanation popover.
3. Pressing `Tab` until the `i` is focused shows the same popover — this is the keyboard path a `title=` attribute would not give.
4. Console has no errors.

- [ ] **Step 5: Commit**

```bash
git add frontend/index.html
git commit -m "feat: add dashboard card and info-tooltip primitives"
```

---

## Task 5: Chart machinery, section nav, window picker, first chart

Introduce the `DASHBOARDS` config, the ECharts theme helpers, the chart registry with dispose-on-leave, the section sub-nav, the global window picker, and the first real chart end to end.

**Files:**
- Modify: `frontend/index.html` — Dashboards script block; `route()` (dispose hook)

**Interfaces:**
- Consumes: `dashCard()` and `infoTip()` from Task 4; `api()`; `?window=` from Task 2.
- Produces:
  - `DASHBOARDS` — array of `{id, title, charts: []}`; Tasks 6–7 push chart objects into `charts`.
  - Chart object shape: `{id, title, why, window?, wide?, fetch: (window) => Promise<Array>, opt: (rows) => Object}`. `fetch` receives the resolved window string — `chart.window` when the chart pins its own range, otherwise the global picker's value.
  - `dashApi(path, chartWindow) -> Promise<Array>`; `dashBase(opt)`, `dashCatAxis(times)`, `dashValAxis(name)`, `cumulative(values)`, `DASH_PAL`; `destroyDashCharts()`; `renderDashChart(chart)`.

- [ ] **Step 1: Add the theme helpers and state**

In the Dashboards script block, above `infoTip`, add:

```js
const DASH_PAL = ['#4ecdc4', '#ff6b6b', '#51cf66', '#ffd43b', '#b197fc', '#4dabf7', '#ff922b', '#e599f7'];
const DASH_AXIS = {
  axisLine: { lineStyle: { color: '#2a2a2a' } },
  axisLabel: { color: '#888', fontSize: 10 },
  splitLine: { lineStyle: { color: '#1e1e1e' } },
};
const DASH_WINDOWS = ['24h', '7d', '30d', '90d', '1y', 'all'];
let _dashWindow = '90d';
let _dashSection = null;
const _dashCharts = {};

function dashBase(opt) {
  return Object.assign({
    backgroundColor: 'transparent',
    textStyle: { fontFamily: "'SF Mono','Cascadia Code','Fira Code',monospace", color: '#e0e0e0' },
    tooltip: { backgroundColor: '#1e1e1e', borderColor: '#2a2a2a', textStyle: { color: '#e0e0e0', fontSize: 11 } },
    grid: { left: 60, right: 56, top: 30, bottom: 28 },
    color: DASH_PAL,
  }, opt);
}
function dashLegend(names) {
  return { data: names, textStyle: { color: '#888', fontSize: 11 }, top: 0, right: 4 };
}
function dashCatAxis(times) {
  return Object.assign({ type: 'category', data: times, boundaryGap: false }, DASH_AXIS);
}
function dashValAxis(name) {
  return Object.assign({ type: 'value', name: name, nameTextStyle: { color: '#888' } }, DASH_AXIS);
}
function cumulative(values) {
  let sum = 0;
  return values.map(v => (sum += (v || 0)));
}
async function dashApi(path, chartWindow) {
  const w = chartWindow || _dashWindow;
  return api(path + (path.includes('?') ? '&' : '?') + 'window=' + w);
}
function destroyDashCharts() {
  Object.keys(_dashCharts).forEach(k => {
    const inst = _dashCharts[k];
    if (inst && !inst.isDisposed()) inst.dispose();
    delete _dashCharts[k];
  });
}
```

- [ ] **Step 2: Add the config with the first chart**

Below the helpers, add:

```js
const DASHBOARDS = [
  {
    id: 'pulse',
    title: 'chain pulse',
    charts: [
      {
        id: 'tx-by-type',
        title: 'transactions per bucket, by message type',
        why: 'Total on-chain activity and what it is made of. A shift in the mix between calls, sends, runs and deploys shows what the chain is actually being used for. Counts messages, not transactions: one transaction can carry several.',
        wide: true,
        fetch: w => dashApi('timeseries/transactions', w),
        opt: rows => dashBase({
          tooltip: { trigger: 'axis' },
          legend: dashLegend(['calls', 'sends', 'runs', 'deploys']),
          xAxis: dashCatAxis(rows.map(r => r.time)),
          yAxis: dashValAxis('messages'),
          series: [
            { name: 'calls', type: 'line', stack: 't', areaStyle: { opacity: 0.5 }, showSymbol: false, lineStyle: { width: 1 }, data: rows.map(r => r.calls) },
            { name: 'sends', type: 'line', stack: 't', areaStyle: { opacity: 0.5 }, showSymbol: false, lineStyle: { width: 1 }, data: rows.map(r => r.sends) },
            { name: 'runs', type: 'line', stack: 't', areaStyle: { opacity: 0.5 }, showSymbol: false, lineStyle: { width: 1 }, data: rows.map(r => r.msg_runs) },
            { name: 'deploys', type: 'line', stack: 't', areaStyle: { opacity: 0.5 }, showSymbol: false, lineStyle: { width: 1 }, data: rows.map(r => r.deploys) },
          ],
        }),
      },
    ],
  },
];
```

- [ ] **Step 3: Add the render pipeline**

Replace `loadDashboards` (from Task 4) entirely with:

```js
async function renderDashChart(chart) {
  const host = document.getElementById('dash-chart-' + chart.id);
  if (!host) return;
  let rows;
  try {
    // chart.window pins a chart to its own range, ignoring the global picker.
    rows = await chart.fetch(chart.window || _dashWindow);
  } catch (err) {
    host.textContent = '';
    host.appendChild(el('div', { className: 'dash-msg' }, 'could not load this chart'));
    return;
  }
  if (!rows || rows.length === 0) {
    host.textContent = '';
    host.appendChild(el('div', { className: 'dash-msg' }, 'no data in this window'));
    return;
  }
  const inst = echarts.init(host);
  inst.setOption(chart.opt(rows), true);
  _dashCharts[chart.id] = inst;
}

function dashSubnav(sections) {
  const seg = el('div', { className: 'dash-seg' });
  sections.forEach(s => {
    const b = el('button', { type: 'button', className: s.id === _dashSection ? 'on' : '' }, s.title);
    b.addEventListener('click', () => {
      const params = new URLSearchParams(window.location.search);
      params.set('section', s.id);
      history.replaceState(null, '', window.location.pathname + '?' + params.toString());
      loadDashboards();
    });
    seg.appendChild(b);
  });
  return seg;
}

function dashWindowPicker() {
  const seg = el('div', { className: 'dash-seg' });
  DASH_WINDOWS.forEach(w => {
    const b = el('button', { type: 'button', className: w === _dashWindow ? 'on' : '' }, w);
    b.addEventListener('click', () => { _dashWindow = w; loadDashboards(); });
    seg.appendChild(b);
  });
  return el('span', { className: 'dash-grp' }, el('span', { className: 'label' }, 'window'), seg);
}

function loadDashboards() {
  destroyDashCharts();
  const root = clear('dashboards-content');
  if (typeof echarts === 'undefined') {
    root.appendChild(el('div', { className: 'dash-msg' },
      'charts unavailable — the ECharts library failed to load'));
    return;
  }
  // A section appears only once it has at least one chart, so later batches
  // can add sections without leaving empty tabs behind.
  const sections = DASHBOARDS.filter(s => s.charts.length > 0);
  if (sections.length === 0) {
    root.appendChild(el('div', { className: 'dash-msg' }, 'no dashboards yet'));
    return;
  }
  const urlSection = new URLSearchParams(window.location.search).get('section');
  _dashSection = sections.some(s => s.id === urlSection) ? urlSection : sections[0].id;

  const bar = el('div', { className: 'dash-bar' }, dashSubnav(sections), dashWindowPicker());
  root.appendChild(bar);

  const section = sections.find(s => s.id === _dashSection);
  const grid = el('div', { className: 'dash-grid' });
  section.charts.forEach(c => grid.appendChild(dashCard(c)));
  root.appendChild(grid);
  section.charts.forEach(c => renderDashChart(c));
}

let _dashResizeTimer = null;
window.addEventListener('resize', () => {
  clearTimeout(_dashResizeTimer);
  _dashResizeTimer = setTimeout(() => {
    Object.keys(_dashCharts).forEach(k => {
      const inst = _dashCharts[k];
      if (inst && !inst.isDisposed()) inst.resize();
    });
  }, 120);
});
```

- [ ] **Step 4: Dispose charts when leaving the view**

In `route()`, immediately before the `switch (view)` statement (`:749`), add:

```js
  if (view !== 'dashboards' && typeof destroyDashCharts === 'function') destroyDashCharts();
```

This is the lifecycle the WebGL graph in batch 4 depends on — a hidden ECharts GL instance keeps an animation loop running that `display:none` does not pause.

- [ ] **Step 5: Verify in the browser**

Rebuild and reload `http://localhost:8899/dashboards`. Expected:
1. A "chain pulse" section button and a window picker showing `90d` selected.
2. A wide stacked-area chart with a calls/sends/runs/deploys legend, populated from real data.
3. Clicking `7d` re-renders with finer buckets; clicking `all` re-renders with `YYYY-MM` labels.
4. Switching the network selector re-renders the chart for that network.
5. Navigating to another view then back leaves no console errors, and the chart re-renders.
6. Resizing the window resizes the chart.

- [ ] **Step 6: Commit**

```bash
git add frontend/index.html
git commit -m "feat: add dashboards chart machinery and transaction-mix chart"
```

---

## Task 6: Remaining Chain Pulse charts

Add the other three Pulse charts.

**Files:**
- Modify: `frontend/index.html` — the `pulse` section's `charts` array

**Interfaces:**
- Consumes: everything Task 5 produced. Response fields used — `/api/timeseries/transactions`: `time`, `total`; `/api/timeseries/health`: `time`, `success_rate`; `/api/timeseries/active-addresses`: `time`, `total_active`, `unique_callers`, `unique_deployers`, `unique_senders`.
- Produces: nothing new; Task 7 is independent.

- [ ] **Step 1: Append the three chart definitions**

Add to the `pulse` section's `charts` array, after the `tx-by-type` entry:

```js
      {
        id: 'cumulative-tx',
        title: 'cumulative transactions',
        window: 'all',
        why: 'The long-run growth curve, summed across all indexed history. This chart is pinned to the full range, so the window picker above does not change it — a cumulative total over a partial window would understate the real figure.',
        fetch: w => dashApi('timeseries/transactions', w),
        opt: rows => dashBase({
          tooltip: { trigger: 'axis' },
          xAxis: dashCatAxis(rows.map(r => r.time)),
          yAxis: dashValAxis('messages'),
          series: [{
            type: 'line', smooth: true, showSymbol: false,
            areaStyle: { opacity: 0.25 },
            data: cumulative(rows.map(r => r.total)),
          }],
        }),
      },
      {
        id: 'success-rate',
        title: 'transaction success rate',
        why: 'Reliability. A sudden dip usually means a buggy realm deploy, a spam wave, or a gas-estimation problem, so this doubles as a chain-health alarm.',
        fetch: w => dashApi('timeseries/health', w),
        opt: rows => dashBase({
          tooltip: { trigger: 'axis', valueFormatter: v => v + '%' },
          xAxis: dashCatAxis(rows.map(r => r.time)),
          yAxis: Object.assign(dashValAxis('%'), { min: 0, max: 100 }),
          series: [{
            type: 'line', smooth: true, showSymbol: false,
            areaStyle: { opacity: 0.2 },
            lineStyle: { color: DASH_PAL[2] }, itemStyle: { color: DASH_PAL[2] },
            data: rows.map(r => r.success_rate),
          }],
        }),
      },
      {
        id: 'active-addresses',
        title: 'active addresses per bucket',
        why: 'Engagement, split by what each address did. This counts addresses active within each bucket only. Rolling 7- and 30-day active counts need trailing-window queries and arrive in a later batch.',
        fetch: w => dashApi('timeseries/active-addresses', w),
        opt: rows => dashBase({
          tooltip: { trigger: 'axis' },
          legend: dashLegend(['total', 'callers', 'deployers', 'senders']),
          xAxis: dashCatAxis(rows.map(r => r.time)),
          yAxis: dashValAxis('addresses'),
          series: [
            { name: 'total', type: 'line', smooth: true, showSymbol: false, data: rows.map(r => r.total_active) },
            { name: 'callers', type: 'line', smooth: true, showSymbol: false, data: rows.map(r => r.unique_callers) },
            { name: 'deployers', type: 'line', smooth: true, showSymbol: false, data: rows.map(r => r.unique_deployers) },
            { name: 'senders', type: 'line', smooth: true, showSymbol: false, data: rows.map(r => r.unique_senders) },
          ],
        }),
      },
```

- [ ] **Step 2: Verify in the browser**

Rebuild and reload `http://localhost:8899/dashboards`. Expected:
1. Four cards in the pulse section.
2. The cumulative chart is monotonically non-decreasing and shows `YYYY-MM` labels **regardless** of the window picker — click `7d` and confirm it does not change while the other three do.
3. Success rate stays within 0–100.
4. Each card's `i` tooltip shows its explanation.
5. No console errors.

- [ ] **Step 3: Commit**

```bash
git add frontend/index.html
git commit -m "feat: add cumulative, success-rate and active-address charts"
```

---

## Task 7: Economics section

Add the second section with the two gas/fee charts, proving the section sub-nav works with more than one entry.

**Files:**
- Modify: `frontend/index.html` — append a section to `DASHBOARDS`

**Interfaces:**
- Consumes: everything Task 5 produced. Response fields from `/api/timeseries/gas`: `time`, `total_gas_used`, `total_gas_wanted`, `gas_efficiency`, `total_fees`.
- Produces: the `economics` section, completing batch 1.

**Unit note (corrected 2026-08-13):** `gas_efficiency` is a **0–1 fraction**, not a percentage — `db.go:2119` computes `float64(r.totalGasUsed) / float64(r.totalGasWanted)`. It must be multiplied by 100 for the 0–100 axis below. Empty buckets yield `0` (no sentinel), so no null-mapping is needed here — unlike `success_rate`, which uses `-1`. Do not "fix" this in the API: the existing sanity view at `frontend/index.html:1936` already applies its own `* 100` to the raw fraction.

- [ ] **Step 1: Append the section**

Add to the `DASHBOARDS` array, after the `pulse` section object:

```js
  {
    id: 'economics',
    title: 'economics',
    charts: [
      {
        id: 'gas-used-wanted',
        title: 'gas used vs wanted',
        wide: true,
        why: 'Capacity and estimator quality. The gap between gas wanted and gas used is headroom that was paid for but never consumed; the efficiency line tracks that gap as a percentage.',
        fetch: w => dashApi('timeseries/gas', w),
        opt: rows => dashBase({
          tooltip: { trigger: 'axis' },
          legend: dashLegend(['gas wanted', 'gas used', 'efficiency %']),
          xAxis: dashCatAxis(rows.map(r => r.time)),
          yAxis: [
            dashValAxis('gas'),
            Object.assign(dashValAxis('%'), { position: 'right', min: 0, max: 100 }),
          ],
          series: [
            { name: 'gas wanted', type: 'line', showSymbol: false, areaStyle: { opacity: 0.15 }, data: rows.map(r => r.total_gas_wanted) },
            { name: 'gas used', type: 'line', showSymbol: false, areaStyle: { opacity: 0.3 }, lineStyle: { color: DASH_PAL[1] }, itemStyle: { color: DASH_PAL[1] }, data: rows.map(r => r.total_gas_used) },
            // gas_efficiency is a 0-1 fraction (db.go: gasUsed/gasWanted), so it
            // must be scaled to percent for the 0-100 axis. The existing sanity
            // view compensates the same way. Do NOT change the API payload —
            // that view consumes the raw fraction.
            { name: 'efficiency %', type: 'line', yAxisIndex: 1, showSymbol: false, lineStyle: { color: DASH_PAL[3] }, itemStyle: { color: DASH_PAL[3] }, data: rows.map(r => r.gas_efficiency * 100) },
          ],
        }),
      },
      {
        id: 'fees',
        title: 'fees per bucket & cumulative',
        wide: true,
        why: 'The fee economy in ugnot: how much the chain collects each bucket, plus the running total. The cumulative line accumulates within the selected window, not from genesis.',
        fetch: w => dashApi('timeseries/gas', w),
        opt: rows => dashBase({
          tooltip: { trigger: 'axis' },
          legend: dashLegend(['fees', 'cumulative']),
          xAxis: dashCatAxis(rows.map(r => r.time)),
          yAxis: [
            dashValAxis('ugnot'),
            Object.assign(dashValAxis('cumulative'), { position: 'right' }),
          ],
          series: [
            { name: 'fees', type: 'bar', data: rows.map(r => r.total_fees), itemStyle: { color: DASH_PAL[0] } },
            { name: 'cumulative', type: 'line', yAxisIndex: 1, showSymbol: false, lineStyle: { color: DASH_PAL[3] }, itemStyle: { color: DASH_PAL[3] }, data: cumulative(rows.map(r => r.total_fees)) },
          ],
        }),
      },
    ],
  },
```

- [ ] **Step 2: Verify in the browser**

Rebuild and reload `http://localhost:8899/dashboards`. Expected:
1. Two section buttons: "chain pulse" and "economics".
2. Clicking "economics" shows the two gas charts and puts `?section=economics` in the URL; reloading that URL lands on economics directly.
3. An unknown `?section=nope` falls back to chain pulse.
4. Efficiency stays within 0–100; the cumulative fee line is non-decreasing.
5. The window picker re-renders both charts.
6. No console errors.

- [ ] **Step 3: Run the full gate**

```bash
gofmt -l . && go vet ./... && go test ./...
```

- [ ] **Step 4: Update the spec's tracking table**

In `docs/superpowers/specs/2026-08-13-chain-analytics-dashboards-design.md` §5, tick batch 1:

```markdown
- [x] **Batch 1 — shell + zero-persistence charts.**
```

- [ ] **Step 5: Commit**

```bash
git add frontend/index.html docs/superpowers/specs/2026-08-13-chain-analytics-dashboards-design.md
git commit -m "feat: add economics dashboard section"
```

---

## Done when

- `/dashboards` shows two sections with six charts, all from real data
- The window picker drives every chart except the pinned cumulative one
- The network selector re-scopes every chart
- `?window=` works on all nine existing time-series endpoints; `?days=`/`?granularity=` are unchanged
- `gofmt -l .` is empty, `go vet ./...` and `go test ./...` pass
- Batch 1 is ticked in the spec's tracking table

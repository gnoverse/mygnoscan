# Browser tests

The frontend is one file of markup, CSS and vanilla JS served from `go:embed`,
and the Go tests can say nothing about it. Their failure mode is "the JSON was
fine, the JS threw" — which no amount of backend coverage catches, and which
used to be found by a human loading the page.

These tests run the real binary against a seeded SQLite fixture and drive it in
a headless browser.

## Running them

```bash
make e2e            # from the repo root: installs, builds, runs
```

or, once `npm ci` has been run in this directory:

```bash
npx playwright test                       # everything
npx playwright test tests/graph.spec.js   # one file
npx playwright test --headed              # watch it happen
npx playwright test --debug               # step through it
```

A failure leaves a screenshot, a video and a trace under `test-results/`:

```bash
npx playwright show-trace test-results/<test>/trace.zip
```

CI uploads the same directory as an artifact.

**Node 24 or newer.** The fixture writes through `node:sqlite`, which needs
`--experimental-sqlite` on 22 and is stable from 24 — which is also why there is
no `sqlite3` CLI dependency here.

## About the Node dependency

Node lives here and nowhere else. It is not a build step for the frontend, it is
not in `go.mod`, and nothing it produces is embedded in the binary — the "no
Node.js in the shipped artifact" property the README advertises is unchanged.
`make test` remains Go-only; `make e2e` is the one that needs this directory.

The alternative considered was `chromedp`, which would keep everything in Go.
Playwright won on the thing the tests are for: when one fails in CI, the trace
viewer shows the DOM, the network log and the console at every step, and
`chromedp` has no equivalent.

## How it is wired

`harness/global-setup.mjs` does the whole thing, in an order that matters:

1. `go build` the binary.
2. Start `harness/fake-indexer.mjs`, so the suite is offline. It answers the
   three GraphQL queries the client makes with valid, empty payloads — enough
   that indexer-backed endpoints return "nothing here" rather than "the indexer
   is down", which would otherwise show up as a failed request on every page.
3. Run the binary with `-sync=false` against a fresh database. **The binary owns
   the schema**, so it has to start before anything can be inserted; the schema
   is never restated in JavaScript, because two copies of it would drift.
4. Seed `harness/fixture.mjs` straight into the SQLite file.

This is a `globalSetup` rather than Playwright's `webServer` for step 3 and 4:
`webServer` only knows how to wait for a URL to answer, and it answers before
the fixture exists.

## What the fixture is

Two networks, `alpha` and `beta`, with the same package path deployed on both —
so anything that joins on path alone instead of `(path, network)` shows up as
wrong counts. On `alpha`, one hub realm with 60 dependents and 12 shared
packages, which is what makes the dependency graph dense enough for the label
assertions to mean anything, plus calls, runs, sends and transactions.

## What is asserted

`tests/pages.spec.js` visits every route in the nav and every tab on the realm
page, and requires: HTTP 200, something rendered, **no uncaught JS exceptions,
no console errors, and no failed requests**. The last one is what catches the
frontend calling an endpoint the backend does not serve — it found exactly that
on its first run (`/api/storage` answers 400 in all-networks mode by design, and
the realm page asked anyway).

`tests/graph.spec.js` covers the dependency graph, which is the feature the tool
exists for and the one D3 can break while the JSON stays perfect: nodes actually
render, no two labels overlap, zooming reveals labels rather than magnifying the
pile, and a label the declutter dropped comes back on hover.

Content assertions are deliberately weak. This suite is not trying to pin what
each page says; it is trying to notice when one of them stops working at all.

## Adding a test

Waiting is the part that goes wrong. The app signals nothing when it has
finished rendering, so:

- `settle(page)` in `tests/helpers.js` waits for the network to go quiet and the
  loading skeletons to clear.
- The graph needs more: `waitStable` polls until every node position *and* the
  label visibility pattern repeat three times running. Sampling one node twice
  was not enough — a slow-drifting layout produces two equal-looking samples
  while still moving, which passed alone and failed in the full run.

If a new page calls an endpoint this harness cannot answer, add it to
`EXPECTED_FAILURES` in `tests/helpers.js` **with the reason**. Every entry there
is a claim that a failure is the harness's fault rather than the page's.

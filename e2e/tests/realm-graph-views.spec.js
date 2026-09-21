import { expect, test } from '@playwright/test';

import { HUB_ROUTE, PAIR_CALLERS, PAIRED_REALMS } from '../harness/fixture.mjs';
import { unexpected, watch } from './helpers.js';

// The contracts map's controls, applied to one realm's graph: a link mode
// (imports, or two contracts linked because the same addresses called both), a
// size metric and a scale.
//
// The fixture is built for exactly this shape. Both PAIR_CALLERS called both
// PAIRED_REALMS and nothing else, so consumer00/app has exactly one co-usage
// partner shared by both of its callers. HUB is called by two addresses that
// called nothing else at all, which is the empty state, and it is the common
// case on a real chain: on mainnet on 2026-09-22 only 35 of 348 contracts had
// ever been called.

const PAIRED_ROUTE = PAIRED_REALMS[0].replace('gno.land/', '');
const PARTNER_LABEL = PAIRED_REALMS[1].split('/').slice(-2).join('/');

// The graph's own SVG, not the legend's: the legend draws a circle and a
// diamond in their own tiny inline <svg> elements in the same container.
const GRAPH_SVG = '#dep-graph > svg';
// A node group. zoomG holds a link layer and a node layer, so the groups that
// carry a shape are grandchildren of the zoom group, not children of it.
const NODE_G = '#dep-graph > svg > g > g > g';

async function openDeps(page, route) {
  await page.goto(`/realm/${route}?network=alpha&tab=deps`);
  await page.waitForSelector('#dep-graph-controls button', { timeout: 20_000 });
}

// Blocks until the force layout stops moving. Same approach as graph.spec.js:
// the simulation emits no event when it cools, and sampling once catches a
// slow-drifting layout mid-flight.
async function waitStable(page) {
  await page.waitForFunction(() => {
    const svg = document.querySelector('#dep-graph > svg');
    if (!svg) return false;
    const nodes = Array.from(svg.querySelectorAll('g'))
      .filter(g => g.querySelector(':scope > circle, :scope > polygon'));
    if (!nodes.length) return false;
    const signature = nodes.map(n => n.getAttribute('transform')).join('|');
    window.__stable = signature === window.__signature ? (window.__stable || 0) + 1 : 0;
    window.__signature = signature;
    return window.__stable >= 3;
  }, null, { timeout: 30_000, polling: 300 });
}

function pill(page, group, label) {
  return page.locator(`#dep-graph-controls div:has(> span:text-is("${group}")) button:text-is("${label}")`);
}

test('the deps graph offers the map\'s link modes, and caller mode draws the co-usage graph', async ({ page }) => {
  const seen = watch(page);
  await openDeps(page, PAIRED_ROUTE);

  await pill(page, 'links', 'shared callers').click();
  await waitStable(page);

  // Centre plus exactly one partner: both of this realm's callers also called
  // the other paired realm, and neither of them called anything else.
  // zoomG holds two layers, links then nodes, so a node group is a
  // grandchild: svg > g(zoom) > g(node layer) > g(node).
  await expect(page.locator(NODE_G)).toHaveCount(2);
  await expect(page.locator(`${GRAPH_SVG} text`, { hasText: PARTNER_LABEL })).toHaveCount(1);

  // The edge carries the weight, so it must not be the default 1.5px every
  // import edge is drawn at.
  const width = await page.locator(`${GRAPH_SVG} line`).first().evaluate(l => parseFloat(l.getAttribute('stroke-width')));
  expect(width).toBeGreaterThan(1.5);

  // "shared callers" is symmetric, so no arrowhead asserts a direction. The
  // attribute is absent, not empty: d3 removes it when the callback returns
  // null, and an empty marker-end would still be a marker reference.
  const marker = await page.locator(`${GRAPH_SVG} line`).first().evaluate(l => l.getAttribute('marker-end'));
  expect(marker).toBeNull();

  // The denominator, without which "1 partner" cannot be read.
  await expect(page.locator('#dep-graph-controls')).toContainText(`my ${PAIR_CALLERS.length} callers`);

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.failedRequests)).toEqual([]);
});

test('caller mode says which empty state it is', async ({ page }) => {
  const seen = watch(page);
  await openDeps(page, HUB_ROUTE);

  await pill(page, 'links', 'shared callers').click();

  // HUB's two callers exist and called nothing else, which is a different fact
  // from HUB having no callers. A blank box cannot tell them apart, and the
  // second one is what 313 of mainnet's 348 contracts would show.
  await expect(page.locator('#dep-graph')).toContainText('called nothing else', { timeout: 20_000 });
  await expect(page.locator('#dep-graph')).toContainText('2 callers');
  await expect(page.locator(GRAPH_SVG)).toHaveCount(0);

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.failedRequests)).toEqual([]);
});

test('sizing by a metric changes the nodes, names itself, and keeps links off the bubbles', async ({ page }) => {
  const seen = watch(page);
  await openDeps(page, HUB_ROUTE);
  await waitStable(page);

  // Every node's size, whatever shape it is drawn as. Reading the circles
  // alone would have measured only this fixture's twelve shared packages, and
  // those have 16 importers each by construction: a real change would have
  // looked like no change.
  const sizes = () => page.locator(NODE_G).evaluateAll(gs => gs.map((g) => {
    const c = g.querySelector(':scope > circle');
    if (c) return parseFloat(c.getAttribute('r'));
    const p = g.querySelector(':scope > polygon');
    if (!p) return 0;
    // Points are written as 0,-s s,0 0,s -s,0, and s is the logical size
    // already scaled by 1.28 so a diamond and a circle holding the same value
    // cover the same area. Undo it, so both shapes report one comparable
    // number rather than four sizes where the graph draws two.
    return Math.abs(parseFloat(p.getAttribute('points').split(' ')[0].split(',')[1])) / 1.28;
  }));
  const round = (xs) => new Set(xs.map(v => Math.round(v * 100) / 100));

  // Uniform is the default and costs no request. Two sizes only, and both are
  // constants: the centre, and everything else.
  const before = await sizes();
  expect(before.length).toBeGreaterThan(2);
  expect(round(before).size).toBe(2);

  // HUB imports twelve shared packages and sixty realms import HUB, so
  // "depended on by" is the metric this graph is actually about, and the one
  // that is invisible under any traffic measure: a pure package receives no
  // calls and burns no gas of its own.
  await pill(page, 'size', 'depended on by').click();
  await waitStable(page);

  // HUB has 60 dependents and every consumer realm has none, so a graph that
  // still draws two sizes is a graph the metric did not reach.
  const after = await sizes();
  expect(round(after).size).toBeGreaterThan(round(before).size);
  expect(Math.max(...after)).toBeGreaterThan(Math.max(...before));

  // An area is a comparison, not a reading, so the legend prints the number
  // behind the largest one.
  await expect(page.locator('#dep-graph')).toContainText('size = depended on by');

  // The scale pill only exists once something is scaled by it.
  await expect(pill(page, 'scale', 'log')).toBeVisible();
  await pill(page, 'scale', 'log').click();
  await waitStable(page);
  await expect(page.locator('#dep-graph')).toContainText('(log)');

  // Every link stops outside the node it ends on, rather than at its centre.
  // With uniform 7px dots the overshoot hid under the bubble; sized by a metric
  // a link ran straight through a forty-pixel node and the arrowhead landed
  // inside it.
  //
  // Stated as "no endpoint sits on a node centre", which is exactly what the
  // old behaviour produced and what trimming makes impossible. Checking instead
  // that no endpoint falls inside *any* node would fail on geometry that is
  // correct: a line trimmed to its own node's border can still pass through a
  // neighbour, and that is the layout's business, not this attribute's.
  const onCentres = await page.evaluate(() => {
    const svg = document.querySelector('#dep-graph > svg');
    const centres = [];
    for (const g of svg.querySelectorAll(':scope > g > g > g')) {
      if (!g.querySelector(':scope > circle, :scope > polygon')) continue;
      const m = /translate\(([-\d.]+),([-\d.]+)\)/.exec(g.getAttribute('transform') || '');
      if (m) centres.push([+m[1], +m[2]]);
    }
    let hits = 0, nan = 0;
    for (const line of svg.querySelectorAll('line')) {
      const ends = [
        [+line.getAttribute('x1'), +line.getAttribute('y1')],
        [+line.getAttribute('x2'), +line.getAttribute('y2')],
      ];
      for (const [x, y] of ends) {
        // A NaN coordinate silently drops the line, which is how a trim that
        // divides by a zero-length vector would fail: invisibly.
        if (!Number.isFinite(x) || !Number.isFinite(y)) { nan++; continue; }
        if (centres.some(([cx, cy]) => Math.hypot(x - cx, y - cy) < 0.5)) hits++;
      }
    }
    return { hits, nan, centres: centres.length };
  });
  expect(onCentres.centres).toBeGreaterThan(10);
  expect(onCentres.nan).toBe(0);
  expect(onCentres.hits).toBe(0);

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.failedRequests)).toEqual([]);
});

test('the realm header links to the contracts map centred on this realm', async ({ page }) => {
  const seen = watch(page);
  await page.goto(`/realm/${HUB_ROUTE}?network=alpha`);

  const mapLink = page.locator('a', { hasText: 'view on the map' }).first();
  await expect(mapLink).toBeVisible({ timeout: 20_000 });

  await mapLink.click();
  await page.waitForURL(/\/contracts\?/, { timeout: 20_000 });
  const url = new URL(page.url());
  expect(url.searchParams.get('view')).toBe('orbit');
  expect(url.searchParams.get('focus')).toBe(`gno.land/${HUB_ROUTE}`);

  // And the map actually opens on it, rather than on its own default centre.
  await expect(page.locator('#contract-map')).toContainText(HUB_ROUTE.split('/').slice(-2).join('/'), { timeout: 30_000 });

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.failedRequests)).toEqual([]);
});

test('the activity filter belongs to import mode only', async ({ page }) => {
  const seen = watch(page);
  await openDeps(page, PAIRED_ROUTE);

  // Import mode: the filter narrows the graph to what the chain still runs.
  await expect(pill(page, 'active', 'off')).toBeVisible();

  // Caller mode: every node is there because somebody called it, so the filter
  // has nothing left to remove and showing it would be a control that does
  // nothing. The period pills take its place.
  await pill(page, 'links', 'shared callers').click();
  await expect(pill(page, 'active', 'off')).toHaveCount(0);
  await expect(pill(page, 'window', '24h')).toBeVisible();

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.failedRequests)).toEqual([]);
});

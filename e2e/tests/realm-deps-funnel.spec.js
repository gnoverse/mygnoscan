import { expect, test } from '@playwright/test';

import { HUB, HUB_ROUTE, SHARED_PACKAGES } from '../harness/fixture.mjs';
import { unexpected, watch } from './helpers.js';

// The deps tab drawn as a funnel: who imports this above, what it imports
// below, and one arrow meaning for the whole picture.
//
// The fixture is the shape this is about. r/hub/core is imported by sixty
// realms and imports twelve packages, so the centre has a crowd on both
// sides. Each of those twelve is imported by the hub *and* by fifteen of the
// consumers directly, while the other forty-five reach it only through the
// hub: that gap is what a one-hop reverse walk keeps out and an unbounded one
// drags in.

const GRAPH_SVG = '#dep-graph > svg';
const NODE_G = '#dep-graph > svg > g > g > g';
// The green the funnel paints the row above the centre. Everything below is a
// DEPTH_COLORS entry, and none of them is this.
const UP = '#98c379';

async function openDeps(page, route, query = '') {
  await page.goto(`/realm/${route}?network=alpha&tab=deps${query}`);
  await page.waitForSelector(`${GRAPH_SVG}`, { timeout: 20_000 });
  // The funnel is laid out in one pass, so a single frame is enough: wait for
  // the first node to carry a transform rather than for a simulation to cool.
  await page.waitForFunction(() => {
    const g = document.querySelector('#dep-graph > svg > g > g > g');
    return g && /translate/.test(g.getAttribute('transform') || '');
  }, null, { timeout: 20_000 });
}

// Every drawn node, with the y it was placed at and the colour it was painted.
function nodes(page) {
  return page.locator(NODE_G).evaluateAll(gs => gs.map(g => {
    const shape = g.querySelector(':scope > circle, :scope > polygon');
    const m = /translate\(([-\d.]+),([-\d.]+)\)/.exec(g.getAttribute('transform') || '');
    return {
      title: (g.querySelector(':scope > title') || {}).textContent || '',
      y: m ? Number(m[2]) : NaN,
      x: m ? Number(m[1]) : NaN,
      fill: shape ? shape.getAttribute('fill') : null,
    };
  }));
}

function pill(page, group, label) {
  return page.locator(`#dep-graph-controls div:has(> span:text-is("${group}")) button:text-is("${label}")`);
}

test('the funnel puts what imports this above it and what it imports below', async ({ page }) => {
  const seen = watch(page);
  await openDeps(page, HUB_ROUTE);

  const drawn = await nodes(page);
  const centre = drawn.find(n => n.title.startsWith(HUB));
  expect(centre).toBeTruthy();

  const above = drawn.filter(n => n.fill === UP);
  const below = drawn.filter(n => n !== centre && n.fill !== UP);

  // Sixty realms import the hub; the hub imports twelve packages.
  expect(above.length).toBe(60);
  expect(below.length).toBe(SHARED_PACKAGES);

  // Position carries the direction, which is the whole point: a force layout
  // would put these wherever the physics settled.
  expect(Math.max(...above.map(n => n.y))).toBeLessThan(centre.y);
  expect(Math.min(...below.map(n => n.y))).toBeGreaterThan(centre.y);

  // And it says which is which rather than leaving it to be inferred from the
  // y coordinate, which a reader who has panned cannot see.
  await expect(page.locator('#dep-graph')).toContainText('imports this');
  await expect(page.locator('#dep-graph')).toContainText('imported by it');

  // Every arrow means "imports", both halves. Before the funnel the reverse
  // half was drawn as it arrived from the endpoint, so an arrow from the
  // centre to a dependent asserted the opposite of what that edge says.
  const upEdge = await page.evaluate((hub) => {
    const svg = document.querySelector('#dep-graph > svg');
    const centres = new Map();
    for (const g of svg.querySelectorAll(':scope > g > g > g')) {
      const t = (g.querySelector(':scope > title') || {}).textContent || '';
      const m = /translate\(([-\d.]+),([-\d.]+)\)/.exec(g.getAttribute('transform') || '');
      if (m) centres.set(t.split('\n')[0], { x: +m[1], y: +m[2] });
    }
    const hubAt = centres.get(hub);
    // A line that ends near the hub and starts above it is a dependent's edge.
    for (const line of svg.querySelectorAll('line')) {
      const y1 = +line.getAttribute('y1'), y2 = +line.getAttribute('y2');
      if (y1 < hubAt.y - 20 && Math.abs(y2 - hubAt.y) < 60) {
        return { pointsDown: y2 > y1, hasArrow: !!line.getAttribute('marker-end') };
      }
    }
    return null;
  }, HUB);
  expect(upEdge).not.toBeNull();
  expect(upEdge.pointsDown).toBe(true);
  expect(upEdge.hasArrow).toBe(true);

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.failedRequests)).toEqual([]);
});

test('the reverse walk stops one hop up, so the graph is about this package', async ({ page }) => {
  const seen = watch(page);
  // p/common/util00 is imported by the hub and by fifteen consumers directly.
  // The other forty-five consumers import it only through the hub, and an
  // unbounded reverse walk used to draw all sixty-one.
  await openDeps(page, 'p/common/util00');

  const drawn = await nodes(page);
  const paths = drawn.map(n => n.title.split('\n')[0]);

  expect(paths).toContain('gno.land/p/common/util00');
  expect(paths).toContain(HUB);
  // consumer00 imports util00, util01 and util02: a direct importer.
  expect(paths).toContain('gno.land/r/consumer00/app');
  // consumer01 imports util01, util02 and util03, and reaches util00 only
  // through the hub. Two hops is a fact about the hub, not about util00.
  expect(paths).not.toContain('gno.land/r/consumer01/app');

  // 15 direct consumers, the hub, and util00 itself.
  expect(drawn.length).toBe(17);
  // Nothing sits below: a pure package with no imports of its own has no
  // downward half at all.
  const centre = drawn.find(n => n.title.startsWith('gno.land/p/common/util00'));
  expect(Math.max(...drawn.filter(n => n !== centre).map(n => n.y))).toBeLessThan(centre.y);

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.failedRequests)).toEqual([]);
});

test('clicking a node opens its own graph, drawn the same way', async ({ page }) => {
  const seen = watch(page);
  await openDeps(page, HUB_ROUTE, '&gsize=importers&gscale=log');

  // The link arrived with the graph already sized, rather than being applied
  // after a default paint: the legend names the metric on the first frame.
  await expect(page.locator('#dep-graph')).toContainText('size = depended on by (log)');

  // Click one of the packages the hub imports, which sits in the bottom row.
  const drawn = await nodes(page);
  const centre = drawn.find(n => n.title.startsWith(HUB));
  // By index into the same list, not by a selector on the title: a title
  // carries newlines and a package path, neither of which survives being
  // spelled into a CSS string.
  const below = drawn.map((n, i) => ({ ...n, i })).filter(n => n.y > centre.y);
  const target = below.sort((a, b) => a.x - b.x)[0];
  const targetPath = target.title.split('\n')[0];
  await page.locator(NODE_G).nth(target.i).click();

  await page.waitForURL(/\/realm\/p\/common\//, { timeout: 20_000 });
  const url = new URL(page.url());
  expect(url.pathname).toBe('/' + targetPath.replace('gno.land/', 'realm/'));
  // The tab and every control travel, which is what makes the graph walkable:
  // without them each click lands on the info tab of the next package and the
  // reader has to rebuild the view by hand.
  expect(url.searchParams.get('tab')).toBe('deps');
  expect(url.searchParams.get('gsize')).toBe('importers');
  expect(url.searchParams.get('gscale')).toBe('log');
  expect(url.searchParams.get('network')).toBe('alpha');

  await page.waitForSelector(GRAPH_SVG, { timeout: 20_000 });
  await expect(page.locator('#dep-graph')).toContainText('size = depended on by (log)');

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.failedRequests)).toEqual([]);
});

test('the layout pill switches engines and lands in the URL', async ({ page }) => {
  const seen = watch(page);
  await openDeps(page, HUB_ROUTE);

  await expect(pill(page, 'layout', 'funnel')).toBeVisible();
  await pill(page, 'layout', 'force').click();

  await expect.poll(() => new URL(page.url()).searchParams.get('glayout')).toBe('force');

  // The force view is the old one: no green row, because the colours there are
  // an undirected distance from the centre rather than a side of it.
  await expect.poll(async () => (await nodes(page)).some(n => n.fill === UP)).toBe(false);

  // Shared callers are symmetric, so there is no up and no down to lay out and
  // the control goes away rather than doing nothing.
  await pill(page, 'links', 'shared callers').click();
  await expect(pill(page, 'layout', 'funnel')).toHaveCount(0);

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.failedRequests)).toEqual([]);
});

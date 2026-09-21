import { expect, test } from '@playwright/test';

import { DEPENDENTS, HUB_ROUTE } from '../harness/fixture.mjs';
import { watch } from './helpers.js';

// The dependency graph is the feature the whole tool exists for and the one
// piece of the frontend a backend test can say nothing about: the JSON can be
// perfect and D3 can still draw an empty box.

async function openGraph(page) {
  await page.goto(`/realm/${HUB_ROUTE}?tab=deps`);
  await page.waitForSelector('#dep-graph > svg', { timeout: 20_000 });

  // The force simulation has to cool before any position means anything, and it
  // emits no event when it does.
  //
  // Sampling one node twice was not enough: a slow-drifting layout produces two
  // equal-looking samples while still moving, and the suite then measured a
  // half-settled graph — which showed up as one test failing in the full run
  // and passing on its own. So the signature covers every node position *and*
  // the resulting label visibility, which is what the assertions actually read,
  // and it has to repeat three times running.
  await waitStable(page);
}

// Blocks until the rendered graph stops changing. Reused after zooming, which
// re-runs the same layout work through a transition.
async function waitStable(page) {
  await page.waitForFunction(() => {
    const svg = document.querySelector('#dep-graph > svg');
    const nodes = Array.from(svg.querySelectorAll('g'))
      .filter(g => g.querySelector(':scope > circle, :scope > polygon'));
    if (!nodes.length) return false;

    const signature = nodes.map(n => n.getAttribute('transform')).join('|') + '#' +
      Array.from(svg.querySelectorAll('text')).map(t => t.style.display === 'none' ? '0' : '1').join('');

    window.__stable = signature === window.__signature ? (window.__stable || 0) + 1 : 0;
    window.__signature = signature;
    return window.__stable >= 3;
  }, null, { timeout: 30_000, polling: 300 });
}

// The graph's own SVG, not the legend's. The legend draws a circle and a
// diamond in their own tiny inline <svg> elements inside the same container, so
// `#dep-graph svg` finds one of those first — a selector mistake that reports a
// perfectly good graph as having zero nodes.
const GRAPH_SVG = '#dep-graph > svg';

function measure() {
  const svg = document.querySelector('#dep-graph > svg');
  const texts = Array.from(svg.querySelectorAll('text'));
  const visible = texts.filter(t => t.style.display !== 'none');
  const boxes = visible.map(t => t.getBoundingClientRect());

  let overlapping = 0;
  for (let i = 0; i < boxes.length; i++) {
    for (let j = i + 1; j < boxes.length; j++) {
      const a = boxes[i], b = boxes[j];
      const hit = !(a.right <= b.left || b.right <= a.left || a.bottom <= b.top || b.bottom <= a.top);
      if (hit) { overlapping++; break; }
    }
  }

  return {
    nodes: svg.querySelectorAll('circle, polygon').length,
    labelsTotal: texts.length,
    labelsVisible: visible.length,
    overlapping,
  };
}

test('the graph draws a node per package, not an empty svg', async ({ page }) => {
  const seen = watch(page);
  await openGraph(page);

  const stats = await page.evaluate(measure);
  // The hub, its dependents, and the packages it imports.
  expect(stats.nodes).toBeGreaterThan(DEPENDENTS);
  expect(seen.jsErrors).toEqual([]);
});

test('no two labels overlap', async ({ page }) => {
  await openGraph(page);

  const stats = await page.evaluate(measure);
  // Not "every label is drawn": in a cluster this dense they cannot all fit,
  // and drawing them anyway is the bug (#115). What has to hold is that what
  // *is* drawn is readable.
  expect(stats.overlapping, 'overlapping labels').toBe(0);
  expect(stats.labelsVisible).toBeGreaterThan(0);
});

test('zooming in reveals labels rather than magnifying the pile', async ({ page }) => {
  await openGraph(page);

  const before = await page.evaluate(measure);
  for (let i = 0; i < 3; i++) {
    await page.locator('#dep-graph button', { hasText: '+' }).first().click();
    await page.waitForTimeout(400);
  }
  await waitStable(page);
  const after = await page.evaluate(measure);

  // The labels used to live inside the zoomed group, so text and positions
  // scaled together and overlaps survived any zoom. They must not come back.
  expect(after.overlapping, 'overlapping labels after zooming in').toBe(0);
  expect(before.overlapping).toBe(0);
});

test('a label the declutter dropped comes back on hover', async ({ page }) => {
  await openGraph(page);

  const target = await page.evaluate(() => {
    const svg = document.querySelector('#dep-graph > svg');
    const box = svg.getBoundingClientRect();
    const texts = Array.from(svg.querySelectorAll('text'));
    // Node groups are the ones holding a shape; the layer groups are not.
    const nodes = Array.from(svg.querySelectorAll('g'))
      .filter(g => g.querySelector(':scope > circle, :scope > polygon'));

    for (let i = 0; i < texts.length; i++) {
      if (texts[i].style.display !== 'none') continue;
      const r = nodes[i].getBoundingClientRect();
      const x = r.x + r.width / 2, y = r.y + r.height / 2;
      // Only a node actually on the canvas can be pointed at. The layout puts
      // some outside it, and hovering those is not something to promise.
      if (x > box.left + 20 && x < box.right - 20 && y > box.top + 20 && y < box.bottom - 20) {
        return { index: i, x, y };
      }
    }
    return null;
  });

  expect(target, 'a hidden label on an on-screen node').not.toBeNull();

  await page.mouse.move(target.x, target.y);
  await page.waitForTimeout(300);

  const revealed = await page.evaluate(
    i => document.querySelector('#dep-graph > svg').querySelectorAll('text')[i].style.display !== 'none',
    target.index,
  );
  expect(revealed, 'hovering a node reveals its dropped label').toBe(true);
});

test('the graph svg is present and non-trivial', async ({ page }) => {
  await openGraph(page);
  const svg = page.locator(GRAPH_SVG);
  await expect(svg).toBeVisible();
  expect(await svg.locator('line').count()).toBeGreaterThan(0);
});

// --- The activity filter ---------------------------------------------------
//
// Same control as the contracts map, over a graph that has no call data of its
// own: `dependencies` records what a package imports and knows nothing about
// whether the chain still runs it, so this one asks /api/graph/active for the
// set and filters against it.

// Nodes, not circles. A realm is drawn as a diamond and only a pure package
// gets a circle, so counting circles counts p/ packages — and the whole point
// of this filter is that they are exactly what it removes first. Counting them
// alone reports a working filter as zero nodes.
const GRAPH_NODES = `${GRAPH_SVG} circle, ${GRAPH_SVG} polygon`;

function nodeCount(page) {
  return page.locator(GRAPH_NODES).count();
}

test('the activity filter narrows the graph to what has been called', async ({ page }) => {
  const seen = watch(page);
  await openGraph(page);
  const all = await nodeCount(page);
  expect(all).toBeGreaterThan(10);

  await page.getByRole('button', { name: 'ever', exact: true }).click();
  await page.waitForFunction(
    total => document.querySelectorAll(
      '#dep-graph > svg circle, #dep-graph > svg polygon').length < total,
    all, { timeout: 20_000 });

  const active = await nodeCount(page);
  // The hub is the centre and is never dropped, and two of its sixty
  // dependents are the realms the paired callers use. Everything else in this
  // fixture has never been called, including all twelve p/common packages.
  expect(active).toBeGreaterThan(0);
  expect(active).toBeLessThan(all / 2);
  expect(seen.jsErrors).toEqual([]);
});

test('the graph says how much it is hiding', async ({ page }) => {
  await openGraph(page);
  const all = await nodeCount(page);

  await page.getByRole('button', { name: 'ever', exact: true }).click();
  await expect(page.locator('#dep-graph-controls')).toContainText(`of ${all} shown`);
});

test('following the imports brings the packages behind the active realms back', async ({ page }) => {
  const seen = watch(page);
  await openGraph(page);

  await page.getByRole('button', { name: 'ever', exact: true }).click();
  await page.waitForFunction(
    () => document.querySelectorAll(
      '#dep-graph > svg circle, #dep-graph > svg polygon').length > 0, null, { timeout: 20_000 });
  const direct = await nodeCount(page);

  // The three survivors are all realms, so the graph at this point holds no
  // packages at all. The closure is what brings the twelve p/common packages
  // the hub is built on back.
  await page.getByRole('button', { name: '+ imports', exact: true }).click();
  await page.waitForFunction(
    before => document.querySelectorAll(
      '#dep-graph > svg circle, #dep-graph > svg polygon').length > before,
    direct, { timeout: 20_000 });

  expect(await nodeCount(page)).toBeGreaterThan(direct);
  expect(await page.locator(`${GRAPH_SVG} circle`).count()).toBeGreaterThan(0);
  expect(seen.jsErrors).toEqual([]);
});

test('a window with nothing in it says so instead of drawing one lone dot', async ({ page }) => {
  await openGraph(page);
  // Every fixture row is months old, so nothing at all is active in a day, and
  // the graph falls to the centre alone.
  await page.getByRole('button', { name: '24h', exact: true }).click();
  await expect(page.locator('#dep-graph')).toContainText('activity in the last 24h');

  await page.getByRole('button', { name: 'off', exact: true }).click();
  await page.waitForSelector(GRAPH_SVG, { timeout: 20_000 });
  expect(await nodeCount(page)).toBeGreaterThan(10);
});

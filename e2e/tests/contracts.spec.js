import { expect, test } from '@playwright/test';

import { PAIRED_REALMS } from '../harness/fixture.mjs';
import { unexpected, watch } from './helpers.js';

// The contracts map is a d3 force layout over every deployed package, and like
// the dependency graph it is the kind of thing a backend test cannot vouch
// for: /api/contracts/map can return perfect JSON while the page draws an
// empty box.

async function openMap(page, query = '') {
  await page.goto(`/contracts${query}`);
  await page.waitForSelector('#contract-map svg circle', { timeout: 20_000 });
}

function bubbles(page) {
  return page.locator('#contract-map svg circle');
}

// The force layout has to stop before a hover means anything: a bubble still
// drifting moves out from under the cursor between the hover and the
// assertion, which fires mouseout and clears the highlight.
async function waitForMapSettled(page) {
  await page.waitForFunction(() => {
    const c = document.querySelector('#contract-map svg circle');
    if (!c) return false;
    const now = c.getAttribute('cx');
    const settled = window.__lastCx === now;
    window.__lastCx = now;
    return settled;
  }, null, { timeout: 30_000, polling: 400 });
}

test('the map draws one bubble per deployed contract', async ({ page }) => {
  const seen = watch(page);
  await openMap(page);

  // The fixture deploys well over fifty packages on alpha. Asserting a floor
  // rather than an exact count keeps this from breaking every time the fixture
  // grows a package for some other test's benefit.
  expect(await bubbles(page).count()).toBeGreaterThan(50);

  // Namespace colouring is what makes clusters readable, so a map rendered in
  // one colour is a broken map even though every bubble is present.
  const fills = await bubbles(page).evaluateAll(els =>
    [...new Set(els.map(e => e.getAttribute('fill')))]);
  expect(fills.length).toBeGreaterThan(1);

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.failedRequests)).toEqual([]);
  expect(unexpected(seen.consoleErrors)).toEqual([]);
});

test('shared-caller edges are the default and imports are available', async ({ page }) => {
  const seen = watch(page);
  await openMap(page, '?edges=callers');
  // Two fixture addresses call the same two realms, which is exactly one edge
  // at the default minimum of two addresses in common.
  await expect(page.locator('#contract-map svg line')).toHaveCount(1);

  await page.getByRole('button', { name: 'imports', exact: true }).click();
  await page.waitForFunction(
    () => document.querySelectorAll('#contract-map svg line').length > 50,
    null, { timeout: 20_000 });

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.failedRequests)).toEqual([]);
  expect(unexpected(seen.consoleErrors)).toEqual([]);
});

test('switching the size metric repaints without refetching the map', async ({ page }) => {
  const seen = watch(page);
  await openMap(page);

  const requests = [];
  page.on('request', r => {
    if (r.url().includes('/api/contracts/map')) requests.push(r.url());
  });

  const before = await bubbles(page).first().getAttribute('r');
  await page.getByRole('button', { name: 'storage', exact: true }).click();
  // Polled rather than read once: the new radius is transitioned onto the
  // bubble that is already there, so for the first frames after the click it
  // is still somewhere between the two values.
  await expect.poll(() => bubbles(page).first().getAttribute('r')).not.toBe(before);

  // Every metric already arrived with the nodes, so a metric switch must not
  // go back to the server. That is the whole reason the endpoint returns all
  // of them at once.
  expect(requests).toEqual([]);

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.failedRequests)).toEqual([]);
  expect(unexpected(seen.consoleErrors)).toEqual([]);
});

// --- the repaint moves the map, it does not rebuild it ----------------------
//
// Every control on this page used to empty #contract-map and start over: a new
// SVG, new node objects with no coordinates, and a simulation reseeded from
// d3's spiral. The map you were reading was gone and an unrelated one settled
// in its place over several seconds, which made a filter impossible to read as
// a filter. These four assert the fix from the outside.

// Marks every bubble on screen, so a later count of the survivors says whether
// the repaint moved these elements or replaced them. A property on the DOM
// node, not an attribute, precisely so nothing in the page can set it.
async function markBubbles(page) {
  return page.evaluate(() => {
    const els = document.querySelectorAll('#contract-map svg circle[data-path]');
    els.forEach(c => { c.__marked = true; });
    return els.length;
  });
}

function markedBubbles(page) {
  return page.evaluate(() => [...document.querySelectorAll('#contract-map svg circle[data-path]')]
    .filter(c => c.__marked).length);
}

test('a metric click moves the bubbles that are already on screen', async ({ page }) => {
  const seen = watch(page);
  await openMap(page);
  await waitForMapSettled(page);

  const marked = await markBubbles(page);
  expect(marked).toBeGreaterThan(50);

  await page.getByRole('button', { name: 'storage', exact: true }).click();
  await expect.poll(() => bubbles(page).count()).toBe(marked);

  // The node set did not change, so every bubble must be the same element it
  // was. One survivor short means the map was rebuilt and the layout it had
  // settled into was thrown away with it.
  expect(await markedBubbles(page)).toBe(marked);
  expect(seen.jsErrors).toEqual([]);
});

test('a repaint keeps the positions rather than reseeding them', async ({ page }) => {
  await openMap(page);
  await waitForMapSettled(page);

  const at = () => page.evaluate(() => Object.fromEntries(
    [...document.querySelectorAll('#contract-map svg circle[data-path]')]
      .map(c => [c.getAttribute('data-path'), [+c.getAttribute('cx'), +c.getAttribute('cy')]])));

  const before = await at();
  // Every radius added up, not one bubble's: most of the fixture has never
  // been called, and a contract at the floor of the scale is at the floor of
  // both scales.
  const totalR = () => bubbles(page).evaluateAll(els =>
    Math.round(els.reduce((a, e) => a + parseFloat(e.getAttribute('r')), 0)));
  const wasR = await totalR();
  await page.getByRole('button', { name: 'storage', exact: true }).click();
  // The radius is what the click changes, so it is what says the repaint has
  // happened. Waiting on position alone would pass the instant it was clicked,
  // for the very reason this test exists.
  await expect.poll(totalR).not.toBe(wasR);
  await page.evaluate(() => { window.__lastCx = null; });
  await waitForMapSettled(page);
  const after = await at();

  // A reseeded layout scatters every contract across a canvas nearly a
  // thousand pixels wide, so the bar is deliberately generous: this is
  // "relaxed into the new radii", not "started over somewhere else".
  const moved = Object.keys(before)
    .map(p => Math.hypot(after[p][0] - before[p][0], after[p][1] - before[p][1]));
  expect(Math.max(...moved)).toBeLessThan(120);
});

test('a link-mode refetch lands on the map instead of replacing it', async ({ page }) => {
  const seen = watch(page);
  await openMap(page, '?edges=callers');
  await waitForMapSettled(page);
  const marked = await markBubbles(page);

  // The link mode is one of the two controls that has to go back to the
  // server. The edges change, the contracts do not, so the request must land
  // on the map already on screen rather than on a skeleton that replaced it.
  await page.getByRole('button', { name: 'imports', exact: true }).click();
  await page.waitForFunction(
    () => document.querySelectorAll('#contract-map svg line').length > 50,
    null, { timeout: 20_000 });

  expect(await markedBubbles(page)).toBe(marked);
  expect(seen.jsErrors).toEqual([]);
});

test('a repaint leaves the zoom where the reader put it', async ({ page }) => {
  await openMap(page);
  await waitForMapSettled(page);

  const transform = () => page.locator('#contract-map svg > g').first()
    .getAttribute('transform');
  // Both the zoom buttons and the one-off fit that frames a settled map are
  // animated, so "the transform now" is a value in flight. Two identical reads
  // in a row is the end of it.
  const settledTransform = async () => {
    let last = null;
    await expect.poll(async () => {
      const now = await transform();
      const same = now !== null && now === last;
      last = now;
      return same;
    }, { timeout: 15_000 }).toBe(true);
    return last;
  };

  await settledTransform();
  await page.getByRole('button', { name: '+', exact: true }).click();
  const zoomed = await settledTransform();

  // Panning and zooming is how anything is read on a map of three hundred
  // contracts, and a repaint that resets it makes every control cost the
  // reader their place.
  await page.getByRole('button', { name: 'labels', exact: true }).click();
  await expect.poll(() => bubbles(page).count()).toBeGreaterThan(50);
  expect(await transform()).toBe(zoomed);
});

test('the controls survive a reload, because they live in the URL', async ({ page }) => {
  await openMap(page, '?metric=storage_bytes&edges=imports&window=all');
  await page.reload();
  await page.waitForSelector('#contract-map svg circle');

  // A map worth looking at should be a link someone can send.
  expect(page.url()).toContain('metric=storage_bytes');
  expect(page.url()).toContain('edges=imports');
});

test('clicking a bubble opens that contract', async ({ page }) => {
  await openMap(page);
  // The biggest bubble under the default metric, which is a realm the fixture
  // gives real traffic to.
  const target = await bubbles(page).evaluateAll(els => {
    let best = null, bestR = -1;
    for (const e of els) {
      const r = parseFloat(e.getAttribute('r'));
      if (r > bestR) { bestR = r; best = e; }
    }
    const i = els.indexOf(best);
    return i;
  });
  await bubbles(page).nth(target).click();
  await page.waitForURL(/\/realm\//, { timeout: 20_000 });
  await expect(page.locator('#realm-header')).toBeVisible();
});

test('search dims the misses instead of removing them', async ({ page }) => {
  await openMap(page);
  const total = await bubbles(page).count();

  await page.getByPlaceholder('highlight...').fill(PAIRED_REALMS[0]);
  await page.waitForFunction(
    () => [...document.querySelectorAll('#contract-map svg circle')]
      .some(c => parseFloat(c.getAttribute('fill-opacity')) < 0.1),
    null, { timeout: 10_000 });

  // Same bubbles, different opacity: filtering them out would reflow every
  // cluster on each keystroke.
  expect(await bubbles(page).count()).toBe(total);
  const matched = await bubbles(page).evaluateAll(els =>
    els.filter(e => parseFloat(e.getAttribute('fill-opacity')) === 1).length);
  expect(matched).toBeGreaterThan(0);
});

test('the rankings under the map link into the existing pages', async ({ page }) => {
  const seen = watch(page);
  await openMap(page);

  const rankings = page.locator('#contract-rankings');
  await expect(rankings).toBeVisible();
  await expect(rankings.getByText('newest deploys')).toBeVisible();
  await expect(rankings.getByText('top deployers')).toBeVisible();

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.failedRequests)).toEqual([]);
  expect(unexpected(seen.consoleErrors)).toEqual([]);
});

// Every toggle, clicked, asserted to have done something.
//
// "I click it and nothing happens" was real and had one cause: the control bar
// was not redrawn on a repaint, so a pill went on looking unselected even when
// the map under it had changed. For a filter with a small effect there was
// nothing on screen to say the click had landed at all.
// Only pills that start inactive: clicking the already-selected option of a
// radio-style group is correctly a no-op, and asserting it changes would be
// asserting a bug. 'calls' and 'linear' are the defaults, covered by the
// round-trip test below.
// '+ imports' is not here on purpose: it is inert until the activity filter is
// on, so clicking it from the default state is correctly a no-op. It has its
// own tests below.
const TOGGLES = ['unique callers', 'storage', 'gas', 'depended on by', 'depends on',
  'log', 'realms', 'packages', 'parked', 'active ever', 'linked only',
  'cluster', 'outlines', 'labels'];

for (const name of TOGGLES) {
  test(`the ${name} toggle reflects its own state when clicked`, async ({ page }) => {
    const seen = watch(page);
    await openMap(page);

    const pill = page.getByRole('button', { name, exact: true });
    const before = await pill.evaluate(el => el.style.background);
    await pill.click();
    // The bar is rebuilt on repaint, so re-resolve the button rather than
    // holding the detached one.
    const after = await page.getByRole('button', { name, exact: true })
      .evaluate(el => el.style.background);

    expect(after, `${name} looks identical after being clicked`).not.toBe(before);
    expect(seen.jsErrors).toEqual([]);
  });
}

test('the outlines toggle draws one shape per namespace', async ({ page }) => {
  await openMap(page);
  expect(await page.locator('#contract-map svg path').count()).toBe(0);

  await page.getByRole('button', { name: 'outlines', exact: true }).click();
  await page.waitForFunction(
    () => document.querySelectorAll('#contract-map svg path, #contract-map svg g circle').length > 0,
    null, { timeout: 15_000 });
  // The fixture's namespaces are mostly single-contract, which get a circle
  // rather than a hull: both are shapes, and drawing neither is the failure.
  const shapes = await page.locator('#contract-map svg path').count();
  const hullCircles = await page.evaluate(
    () => document.querySelectorAll('#contract-map svg g:first-child circle').length);
  expect(shapes + hullCircles).toBeGreaterThan(0);
});

test('clicking a legend entry isolates that namespace', async ({ page }) => {
  await openMap(page);
  const before = await bubbles(page).count();

  // The entry carries the metric total it is ranked by, so it is no longer
  // name and count alone.
  await page.locator('#contract-map').getByText(/^common \(\d+\) · \S+$/).click();
  await page.waitForFunction(
    (n) => document.querySelectorAll('#contract-map svg circle').length < n,
    before, { timeout: 15_000 });

  expect(page.url()).toContain('ns=common');
  expect(await bubbles(page).count()).toBeLessThan(before);
});

// Hovers the biggest bubble under the metric on screen: the first ranking row
// names it and the map carries the same data-hl key, so this lands on
// something with area rather than on whichever one-pixel dot comes first in
// the DOM and may have a neighbour on top of it.
async function hoverTopBubble(page) {
  const key = await page.locator('#contract-rankings [data-hl^="realm:"]').first()
    .getAttribute('data-hl');
  await page.locator(`#contract-map svg circle[data-hl="${key}"]`).first().hover({ force: true });
}

// --- the number the picture is drawn from -----------------------------------
//
// Six layouts size their shapes by one of six metrics, and none of them used
// to print it. Sized by gas, which is the state the report arrived in, the
// quantity behind every area on screen appeared nowhere on the page, not even
// on hover: the hover card listed calls and storage only.

test('the treemap prints the value under the name in the cells with room', async ({ page }) => {
  const seen = watch(page);
  await page.goto('/contracts?view=treemap&edges=none&metric=storage_bytes');
  await page.waitForSelector('#contract-map svg [data-path]', { timeout: 20_000 });

  const values = await page.locator('#contract-map svg text.val').allTextContents();
  expect(values.length).toBeGreaterThan(0);
  // Storage is the one metric carrying a unit, so this asserts the metric
  // chose the formatting and not merely that some number was drawn.
  expect(values.every(t => /^[\d.,]+ (B|KB|MB)$/.test(t.trim())), values.join(' | ')).toBe(true);

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.consoleErrors)).toEqual([]);
});

test('the count line says what the areas add up to', async ({ page }) => {
  await page.goto('/contracts?view=treemap&edges=none&metric=gas_used');
  await page.waitForSelector('#contract-map svg [data-path]', { timeout: 20_000 });

  // An area is a share of a whole, and "size = gas" without the whole makes
  // the biggest cell on a quiet chain look like the biggest on a busy one.
  await expect(page.locator('#contract-map').getByText(/size = gas .*total/)).toBeVisible();
});

test('the hover card carries every metric, and marks the one on screen', async ({ page }) => {
  const seen = watch(page);
  await openMap(page, '?metric=gas_used');
  await waitForMapSettled(page);

  await hoverTopBubble(page);
  const tip = page.locator('#contract-map .map-tip');
  await expect(tip).toBeVisible();
  // The metric being drawn, the three the card never used to mention, and the
  // marker that says which of them this picture is sized by.
  for (const label of ['gas:', 'depended on by:', 'depends on:', 'storage:', 'calls:']) {
    await expect(tip).toContainText(label);
  }
  await expect(tip).toContainText('· size');

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.consoleErrors)).toEqual([]);
});

test('a windowed metric says so on the hover card', async ({ page }) => {
  await openMap(page, '?metric=calls&window=30d');
  await waitForMapSettled(page);

  await hoverTopBubble(page);
  const tip = page.locator('#contract-map .map-tip');
  // Two of the six metrics move with the window and four do not, which is the
  // difference the warning line above the map exists for. The card repeats it
  // per row so a number read there cannot be mistaken for an all-time one.
  await expect(tip).toContainText(/calls: [\d,]+ \(30d\)/);
  const windowed = await tip.evaluate(el => (el.textContent.match(/\(30d\)/g) || []).length);
  expect(windowed, 'calls and unique callers, and nothing else').toBe(2);
});

// The rankings must look like the rest of the explorer: the same address
// abbreviation and the same identicon every other page uses.
test('the deployer rankings carry identicons and abbreviated addresses', async ({ page }) => {
  await openMap(page);
  const deployers = page.locator('#contract-rankings > div').last();
  await expect(deployers).toContainText('top deployers');
  expect(await deployers.locator('.identicon').count()).toBeGreaterThan(0);
  await expect(deployers.getByText(/g1\w+…\w+/).first()).toBeVisible();
});

test('the scale switches both ways and says which is active', async ({ page }) => {
  await openMap(page);
  const scale = name => page.getByRole('button', { name, exact: true });
  const isActive = async name => (await scale(name).evaluate(el => el.style.color)) !== '';

  // linear is the default; going to log and back must land where it started,
  // with exactly one of the two reading as selected at each step.
  const linearFirst = await scale('linear').evaluate(el => el.style.background);
  await scale('log').click();
  expect(await scale('log').evaluate(el => el.style.background)).toBe(linearFirst);
  expect(page.url()).toContain('scale=log');

  await scale('linear').click();
  expect(await scale('linear').evaluate(el => el.style.background)).toBe(linearFirst);
  expect(page.url()).toContain('scale=linear');
  expect(await isActive('linear')).toBe(true);
});

// Hovering a bubble lights up its row in the cards below, and hovering the row
// lights up the bubble. Both directions come from the explorer's existing
// data-hl group, so the test is really asserting the map joined it.
test('hovering a bubble highlights its ranking row, and the reverse', async ({ page }) => {
  await openMap(page);
  await waitForMapSettled(page);

  // The biggest bubble under the default metric is the top row of the first
  // card, so the two are guaranteed to be about the same contract.
  const topRow = page.locator('#contract-rankings [data-hl^="realm:"]').first();
  const key = await topRow.getAttribute('data-hl');
  const bubble = page.locator(`#contract-map svg circle[data-hl="${key}"]`);
  await expect(bubble).toHaveCount(1);

  await bubble.hover({ force: true });
  await expect(topRow).toHaveClass(/hl-active/);
  await expect(bubble).toHaveClass(/hl-active/);

  await page.mouse.move(0, 0);
  await expect(topRow).not.toHaveClass(/hl-active/);

  await topRow.hover();
  await expect(bubble).toHaveClass(/hl-active/);
});

test('outlines are named, so a group says which group it is', async ({ page }) => {
  await openMap(page, '?hulls=1');
  await page.waitForFunction(
    () => document.querySelectorAll('#contract-map svg text').length > 0,
    null, { timeout: 20_000 });

  // A hull that only says "these belong together" is half an answer. The
  // colour it shares with the legend stops being legible exactly when there
  // are enough namespaces for outlines to be worth drawing.
  const labels = await page.locator('#contract-map svg text').allTextContents();
  expect(labels.length).toBeGreaterThan(0);
  expect(labels.some(t => t.trim().length > 0)).toBe(true);
});

test('hovering a deployer lights up every contract they published', async ({ page }) => {
  await openMap(page);
  await waitForMapSettled(page);

  const row = page.locator('#contract-rankings [data-hl^="g1"]').first();
  const addr = await row.getAttribute('data-hl');
  const expected = await page.locator(
    `#contract-map svg circle[data-creator="${addr}"]`).count();
  expect(expected, 'fixture has no contracts for the top deployer').toBeGreaterThan(0);

  await row.hover();
  await expect
    .poll(() => page.locator('#contract-map svg circle.hl-active').count())
    .toBe(expected);

  await page.mouse.move(0, 0);
  await expect
    .poll(() => page.locator('#contract-map svg circle.hl-active').count())
    .toBe(0);
});

// --- the alternative layouts ------------------------------------------------
//
// Six views over the same nodes and the same edges. The force map is the only
// one that has to settle before it means anything; the other five are laid out
// once, so "did it draw" is a question that can be asked immediately.
//
// Each of these asserts the same two things, because they are the two ways a
// layout fails: it drew nothing, or it drew shapes that are not contracts and
// so join none of the page's highlight groups.

// The floor is per view because what a view draws is per view. The fixture's
// one shared-caller edge is two contracts however it is rendered, and the
// chord draws one shape per namespace rather than one per contract, so a low
// count there is the correct answer and not a failure.
const VIEWS = [
  ['force', '?view=force&edges=callers', 20],
  ['orbit', '?view=orbit&edges=imports', 20],
  ['packed', '?view=packed&edges=callers', 20],
  ['bundled', '?view=bundled&edges=imports', 20],
  ['chord', '?view=chord&edges=callers', 2],
  ['treemap', '?view=treemap&edges=none', 20],
];

for (const [view, query, floor] of VIEWS) {
  test(`the ${view} view draws contracts that carry their path`, async ({ page }) => {
    const seen = watch(page);
    await page.goto(`/contracts${query}`);
    await page.waitForSelector('#contract-map svg [data-path]', { timeout: 20_000 });

    expect(await page.locator('#contract-map svg [data-path]').count())
      .toBeGreaterThanOrEqual(floor);

    // A view that renders but leaves the search box, the ranking cross-
    // highlight and the deployer highlight inert is half a view.
    if (view !== 'chord') {
      expect(await page.locator('#contract-map svg [data-hl^="realm:"]').count()).toBeGreaterThan(0);
      expect(await page.locator('#contract-map svg [data-creator^="g1"]').count()).toBeGreaterThan(0);
    }

    expect(seen.jsErrors).toEqual([]);
    expect(unexpected(seen.failedRequests)).toEqual([]);
    expect(unexpected(seen.consoleErrors)).toEqual([]);
  });
}

// Zoom in far enough and the map has to say more, not just say it bigger. The
// packing's labels were thresholded on the radius d3.pack computed, so zooming
// scaled the circles and the text together: the same twenty contracts stayed
// named at every scale and the other three hundred never were.
test('zooming into the packing names contracts too small to name at rest', async ({ page }) => {
  const seen = watch(page);
  // Narrow, so the fixture's seventy contracts pack into circles small enough
  // that some of them cannot be named. That is the state mainnet's nine hundred
  // are in at any width, and it is the whole case this behaviour is for.
  await page.setViewportSize({ width: 520, height: 760 });
  await openMap(page, '?view=packed&edges=none');
  await page.waitForSelector('#contract-map svg text.name', { timeout: 20_000 });

  // Labels are located by x, not read as text: the fixture deploys sixty
  // packages all called "app", so a set of names cannot tell a circle that just
  // gained a label from one that already had it. x is the leaf's own layout
  // coordinate and does not move when the group is scaled.
  const labelledAt = () => page.locator('#contract-map svg text.name')
    .evaluateAll(els => els.map(e => e.getAttribute('x')));
  const font = () => page.locator('#contract-map svg text.name').first()
    .evaluate(e => +e.getAttribute('font-size'));
  const before = await labelledAt();
  const fontBefore = await font();
  expect(before.length,
    'every circle is already named, so this asserts nothing').toBeLessThan(
    await page.locator('#contract-map svg circle[data-path]').count());

  // An unnamed contract and the namespace circle around it. Clicking a
  // namespace frames it, which is the map's own gesture for zooming into one,
  // so this asserts what a reader can actually do.
  const pick = await page.evaluate(xs => {
    const leaf = [...document.querySelectorAll('#contract-map svg circle[data-path]')]
      .find(c => !xs.includes(c.getAttribute('cx')));
    if (!leaf) return null;
    const lx = +leaf.getAttribute('cx'), ly = +leaf.getAttribute('cy');
    const ns = [...document.querySelectorAll('#contract-map svg circle:not([data-path])')]
      .filter(c => Math.hypot(+c.getAttribute('cx') - lx, +c.getAttribute('cy') - ly) < +c.getAttribute('r'))
      .sort((a, b) => +a.getAttribute('r') - +b.getAttribute('r'))[0];
    if (!ns) return null;
    const box = ns.getBoundingClientRect();
    // Just inside the top of the namespace ring, where the packing's padding
    // guarantees no leaf is in the way to swallow the click.
    return { at: leaf.getAttribute('cx'), x: box.x + box.width / 2, y: box.y + 3 };
  }, before);
  expect(pick, 'no unnamed contract inside a namespace circle').not.toBeNull();

  await page.mouse.click(pick.x, pick.y);
  await page.waitForTimeout(700);

  const after = await labelledAt();
  expect(after.includes(pick.at),
    `the contract at x=${pick.at} is still unnamed after framing its namespace`).toBe(true);

  // Same size on screen at every scale: the group is scaled by k, so the font
  // has to be divided by it. Text that grew with the circles is what used to
  // make the few labels there were spill out of the bubbles they name.
  const k = await page.evaluate(() => {
    const n = document.querySelector('#contract-map svg');
    return n.__zoom ? n.__zoom.k : 1;
  });
  expect(k).toBeGreaterThan(1.5);
  expect(await font()).toBeCloseTo(fontBefore / k, 2);

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.consoleErrors)).toEqual([]);
});

// A link drawn to a centre runs under the bubble it points at, so its visible
// end is wherever that circle happens to stop and the import arrowhead is
// buried inside the package it is aiming at.
test('an edge starts on a bubble border, not at its centre', async ({ page }) => {
  const seen = watch(page);

  // The packing is deterministic, so its geometry can be asserted exactly:
  // both ends of every edge the hover reveals sit on a contract's outline,
  // one radius out from the centre it belongs to.
  await openMap(page, '?view=packed&edges=imports');
  await page.waitForSelector('#contract-map svg circle[data-path]', { timeout: 20_000 });
  const target = await page.evaluate(() => {
    const c = [...document.querySelectorAll('#contract-map svg circle[data-path]')]
      .sort((a, b) => +b.getAttribute('r') - +a.getAttribute('r'))[0];
    return c.getAttribute('data-path');
  });
  await page.locator(`#contract-map svg circle[data-path="${target}"]`).hover({ force: true });

  const probe = await page.evaluate(() => {
    const circles = [...document.querySelectorAll('#contract-map svg circle[data-path]')]
      .map(c => ({ x: +c.getAttribute('cx'), y: +c.getAttribute('cy'), r: +c.getAttribute('r') }));
    // The first coordinate pair and the last one, whatever command joins them.
    const pts = [...document.querySelectorAll('#contract-map svg g[fill="none"] path')]
      .map(p => p.getAttribute('d'))
      .filter(Boolean)
      .flatMap(d => {
        const n = (d.match(/-?\d+(?:\.\d+)?(?:e[-+]?\d+)?/gi) || []).map(Number);
        return n.length >= 4 ? [[n[0], n[1]], [n[n.length - 2], n[n.length - 1]]] : [];
      });
    return {
      pts: pts.length,
      offBorder: pts.filter(([x, y]) =>
        !circles.some(c => Math.abs(Math.hypot(c.x - x, c.y - y) - c.r) < 0.5)).length,
      onCentre: pts.filter(([x, y]) =>
        circles.some(c => Math.hypot(c.x - x, c.y - y) < 0.5)).length,
    };
  });
  expect(probe.pts, 'the hovered contract has no import edges in the fixture').toBeGreaterThan(0);
  expect(probe.offBorder).toBe(0);
  expect(probe.onCentre).toBe(0);

  // The force map moves, so the exact radius is a moving target; what is fixed
  // is that no end of a link sits on a contract's centre any more.
  await openMap(page, '?view=force&edges=imports');
  await waitForMapSettled(page);
  const ends = await page.evaluate(() => {
    const centres = [...document.querySelectorAll('#contract-map svg g.l-nodes circle')]
      .map(c => [+c.getAttribute('cx'), +c.getAttribute('cy')]);
    const pts = [];
    for (const l of document.querySelectorAll('#contract-map svg g.l-links line')) {
      pts.push([+l.getAttribute('x1'), +l.getAttribute('y1')]);
      pts.push([+l.getAttribute('x2'), +l.getAttribute('y2')]);
    }
    return {
      total: pts.length,
      onCentre: pts.filter(([x, y]) => centres.some(([cx, cy]) => Math.hypot(cx - x, cy - y) < 0.5)).length,
    };
  });
  expect(ends.total).toBeGreaterThan(0);
  expect(ends.onCentre).toBe(0);

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.consoleErrors)).toEqual([]);
});

for (const view of ['orbit', 'packed', 'bundled', 'chord', 'treemap']) {
  test(`the ${view} view pill reflects its own state when clicked`, async ({ page }) => {
    const seen = watch(page);
    await openMap(page);
    const pill = page.getByRole('button', { name: view, exact: true });
    const before = await pill.evaluate(el => el.style.background);
    await pill.click();
    const after = await page.getByRole('button', { name: view, exact: true })
      .evaluate(el => el.style.background);

    expect(after, `${view} looks identical after being clicked`).not.toBe(before);
    expect(page.url()).toContain(`view=${view}`);
    expect(seen.jsErrors).toEqual([]);
  });
}

test('switching the view repaints without refetching', async ({ page }) => {
  await openMap(page);
  const requests = [];
  page.on('request', r => {
    if (r.url().includes('/api/contracts/')) requests.push(r.url());
  });

  // Every layout reads the same nodes and the same edges. Going back to the
  // server for a second rendering of data already in the tab is the thing the
  // endpoint returning all metrics at once exists to avoid.
  await page.getByRole('button', { name: 'treemap', exact: true }).click();
  await page.waitForSelector('#contract-map svg rect[data-path]');
  await page.getByRole('button', { name: 'packed', exact: true }).click();
  await page.waitForSelector('#contract-map svg circle[data-path]');
  expect(requests).toEqual([]);
});

test('the scale pills only appear where the scale reaches', async ({ page }) => {
  await openMap(page);
  // log and linear size a radius, and three of the six layouts draw one. The
  // packing and the treemap lay out from the summed metric instead, so the
  // pill never touched them and the count line answered it anyway.
  await expect(page.getByRole('button', { name: 'log', exact: true })).toBeVisible();

  await page.goto('/contracts?view=treemap&edges=none&metric=gas_used&scale=log');
  await page.waitForSelector('#contract-map svg rect[data-path]', { timeout: 20_000 });
  await expect(page.getByRole('button', { name: 'log', exact: true })).toHaveCount(0);
  await expect(page.locator('#contract-map').getByText(/size = gas/)).not.toContainText('(log)');

  // Still remembered: switching back is one click, not a re-pick.
  await page.getByRole('button', { name: 'force', exact: true }).click();
  await page.waitForSelector('#contract-map svg circle[data-path]', { timeout: 20_000 });
  await expect(page.getByRole('button', { name: 'log', exact: true })).toBeVisible();
  await expect(page.locator('#contract-map').getByText(/size = gas/)).toContainText('(log)');
});

test('the grouping pills only appear where they mean something', async ({ page }) => {
  await openMap(page);
  // cluster and outlines are the force layout's own vocabulary: every other
  // view places a contract by its namespace as a matter of construction, so
  // offering to switch the clustering off would be offering a no-op.
  await expect(page.getByRole('button', { name: 'cluster', exact: true })).toBeVisible();

  await page.getByRole('button', { name: 'treemap', exact: true }).click();
  await page.waitForSelector('#contract-map svg rect[data-path]');
  await expect(page.getByRole('button', { name: 'cluster', exact: true })).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'outlines', exact: true })).toHaveCount(0);
});

test('clicking a contract in the orbit re-centres on it', async ({ page }) => {
  const seen = watch(page);
  // The import graph, because the fixture's hub has sixty dependants and its
  // shared-caller graph has one edge: a ring of one is not a ring.
  await page.goto('/contracts?view=orbit&edges=imports');
  await page.waitForSelector('#contract-map svg circle[data-path]', { timeout: 20_000 });

  // The centre is the largest circle and opens the contract; everything else
  // re-centres, which is the whole interaction.
  const target = page.locator('#contract-map svg circle[data-path]').nth(1);
  const path = await target.getAttribute('data-path');
  await target.click();
  await page.waitForFunction(
    p => window.location.search.includes('focus=') &&
      decodeURIComponent(window.location.search).includes(p),
    path, { timeout: 10_000 });

  await expect(page.getByRole('button', { name: new RegExp(path.replace('gno.land/', '')) }))
    .toBeVisible();
  expect(seen.jsErrors).toEqual([]);
});

test('the search dims the misses in a layout that draws rectangles', async ({ page }) => {
  await page.goto('/contracts?view=treemap&edges=none');
  await page.waitForSelector('#contract-map svg rect[data-path]', { timeout: 20_000 });
  const total = await page.locator('#contract-map svg rect[data-path]').count();

  // Dimming is driven by the data-path attribute rather than the bound datum,
  // precisely so it keeps working when the datum is a treemap node and not a
  // contract.
  await page.getByPlaceholder('highlight...').fill(PAIRED_REALMS[0]);
  await page.waitForFunction(
    () => [...document.querySelectorAll('#contract-map svg rect[data-path]')]
      .some(r => parseFloat(r.getAttribute('fill-opacity')) < 0.1),
    null, { timeout: 10_000 });

  expect(await page.locator('#contract-map svg rect[data-path]').count()).toBe(total);
  const matched = await page.locator('#contract-map svg rect[data-path]').evaluateAll(els =>
    els.filter(e => parseFloat(e.getAttribute('fill-opacity')) === 1).length);
  expect(matched).toBeGreaterThan(0);
});

test('a link-only view says so rather than drawing an empty ring', async ({ page }) => {
  const seen = watch(page);
  // The chord, the orbit and the bundled ring are all about the edges. Told to
  // draw one with links switched off, an empty circle reads as a broken page.
  await page.goto('/contracts?view=chord&edges=none');
  await expect(page.locator('#contract-map')).toContainText('this view is about the links');
  expect(await page.locator('#contract-map svg').count()).toBe(0);
  expect(seen.jsErrors).toEqual([]);
});

test('the view survives a reload, because it lives in the URL', async ({ page }) => {
  await page.goto('/contracts?view=bundled&edges=imports&metric=importers');
  await page.waitForSelector('#contract-map svg circle[data-path]', { timeout: 20_000 });
  await page.reload();
  await page.waitForSelector('#contract-map svg circle[data-path]', { timeout: 20_000 });

  expect(page.url()).toContain('view=bundled');
  await expect(page.getByRole('button', { name: 'bundled', exact: true }))
    .toHaveAttribute('title', /ring/);
});

// --- The activity filter ---------------------------------------------------
//
// The fixture is one busy hub, two realms called by a pair of addresses, and
// roughly seventy contracts nobody has ever touched. That ratio is the whole
// point of the filter, and it is also what makes the import closure worth
// having: the twelve p/common/util packages the hub is built on have no calls
// of their own and never will, because a pure package is reached through the
// realm that imports it.

async function bubbleCount(page) {
  return page.locator('#contract-map svg circle[data-path]').count();
}

test('the activity filter keeps only what has been called', async ({ page }) => {
  const seen = watch(page);
  await openMap(page);
  const all = await bubbleCount(page);

  await page.getByRole('button', { name: 'active ever', exact: true }).click();
  await page.waitForFunction(
    total => document.querySelectorAll('#contract-map svg circle[data-path]').length < total,
    all, { timeout: 15_000 });

  const active = await bubbleCount(page);
  expect(active).toBeGreaterThan(0);
  // Most of the fixture has never been called, so this is a large cut and not
  // a rounding difference. Asserting a ratio rather than a count keeps it from
  // breaking every time the fixture grows a package.
  expect(active).toBeLessThan(all / 2);
  expect(page.url()).toContain('active=1');

  // The map's own count line switches to "N of M" while anything is hidden.
  // It is the only thing on screen that tells a filter which removed nothing
  // apart from one that is broken, and it has to be right on the first paint,
  // not only after a second click.
  await expect(page.locator('#contract-map')).toContainText(`${active} of ${all} contracts`);
  expect(seen.jsErrors).toEqual([]);
});

test('following the imports is what keeps the import view from emptying', async ({ page }) => {
  const seen = watch(page);
  await openMap(page, '?edges=imports&metric=importers&active=1');
  const direct = await bubbleCount(page);

  // No pure package has ever been called, so a direct filter drops every one
  // of them and takes the import edges with them. This is the regression that
  // matters: on mainnet the same filter leaves 15 of 771 edges.
  await page.getByRole('button', { name: '+ imports', exact: true }).click();
  await page.waitForFunction(
    before => document.querySelectorAll('#contract-map svg circle[data-path]').length > before,
    direct, { timeout: 15_000 });

  const withDeps = await bubbleCount(page);
  expect(withDeps).toBeGreaterThan(direct);
  expect(await page.locator('#contract-map svg line').count()).toBeGreaterThan(0);
  expect(page.url()).toContain('deps=1');
  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.failedRequests)).toEqual([]);
});

test('the activity filter and its closure survive a reload', async ({ page }) => {
  await page.goto('/contracts?edges=imports&active=1&deps=1');
  await page.waitForSelector('#contract-map svg circle[data-path]', { timeout: 20_000 });
  const before = await bubbleCount(page);

  await page.reload();
  await page.waitForSelector('#contract-map svg circle[data-path]', { timeout: 20_000 });

  expect(await bubbleCount(page)).toBe(before);
  await expect(page.getByRole('button', { name: '+ imports', exact: true }))
    .toHaveCSS('background-color', /rgb/);
});

test('the filter names the window it reads against, and finds what is in it', async ({ page }) => {
  await openMap(page, '?network=alpha&window=24h');
  await expect(page.getByRole('button', { name: 'active in 24h', exact: true })).toBeVisible();

  // alpha carries the fixture's recent tail, so a one-day window keeps the few
  // contracts that were called inside it and drops the months-old rest. Both
  // halves matter: a filter that kept everything and a filter that kept nothing
  // are equally broken and equally silent.
  const all = await bubbleCount(page);
  await page.getByRole('button', { name: 'active in 24h', exact: true }).click();
  // Polled, not read once: the bubbles from before the click are still on
  // screen while the filtered request is in flight, so a single read sees the
  // unfiltered count and passes a test that proves nothing.
  await expect.poll(() => bubbleCount(page), {
    message: 'the 24h filter should drop the months-old contracts',
  }).toBeLessThan(all);
  expect(await bubbleCount(page)).toBeGreaterThan(0);
});

test('a window with nothing in it says so rather than drawing an empty box', async ({ page }) => {
  // beta has no recent tail, so every row on it is months old and a one-day
  // window genuinely finds nothing.
  await openMap(page, '?network=beta&window=24h');
  await page.getByRole('button', { name: 'active in 24h', exact: true }).click();
  await expect(page.locator('#contract-map')).toContainText('no contracts to show');
});

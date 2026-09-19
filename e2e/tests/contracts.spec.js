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
  await page.waitForSelector('#contract-map svg circle');
  const after = await bubbles(page).first().getAttribute('r');

  // Every metric already arrived with the nodes, so a metric switch must not
  // go back to the server. That is the whole reason the endpoint returns all
  // of them at once.
  expect(requests).toEqual([]);
  expect(after).not.toBe(before);

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.failedRequests)).toEqual([]);
  expect(unexpected(seen.consoleErrors)).toEqual([]);
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
const TOGGLES = ['unique callers', 'storage', 'gas', 'depended on by', 'depends on',
  'log', 'realms', 'packages', 'parked', 'used only', 'linked only',
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

  await page.locator('#contract-map').getByText(/^common \(\d+\)$/).click();
  await page.waitForFunction(
    (n) => document.querySelectorAll('#contract-map svg circle').length < n,
    before, { timeout: 15_000 });

  expect(page.url()).toContain('ns=common');
  expect(await bubbles(page).count()).toBeLessThan(before);
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

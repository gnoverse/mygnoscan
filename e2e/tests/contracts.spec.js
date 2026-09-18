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

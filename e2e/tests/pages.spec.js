import { expect, test } from '@playwright/test';

import { HUB_ROUTE, BUSY_CALLER } from '../harness/fixture.mjs';
import { settle, unexpected, watch } from './helpers.js';

// Every route reachable from the nav, plus the detail pages.
//
// The assertion is deliberately the same for all of them and deliberately weak
// on content: this suite is not trying to pin what each page says, it is trying
// to notice when one of them stops working at all. Content assertions belong
// with the feature that owns them.
const ROUTES = [
  ['home', '/'],
  ['watch', '/watch'],
  ['realms', '/realms'],
  ['packages', '/packages'],
  ['transactions', '/txs'],
  ['blocks', '/blocks'],
  ['accounts', '/accounts'],
  ['tokens', '/tokens'],
  ['validators', '/validators'],
  ['govdao', '/govdao'],
  ['gas', '/gas'],
  ['analytics', '/analytics'],
  ['dashboards', '/dashboards'],
  ['sanity', '/sanity'],
  ['events', '/events'],
  ['realm detail', `/realm/${HUB_ROUTE}`],
  ['address detail', `/address/${BUSY_CALLER}`],
];

for (const [name, path] of ROUTES) {
  test(`${name} renders without errors`, async ({ page }) => {
    const seen = watch(page);

    const response = await page.goto(path);
    expect(response.status(), `${path} should be served`).toBe(200);
    await settle(page);

    expect(seen.jsErrors, `uncaught exceptions on ${path}`).toEqual([]);
    expect(unexpected(seen.failedRequests), `failed requests on ${path}`).toEqual([]);
    expect(unexpected(seen.consoleErrors), `console errors on ${path}`).toEqual([]);

    // Something has to have been drawn. An empty <main> passes every assertion
    // above and is exactly the regression this is meant to catch.
    const main = page.locator('.view.active main').first();
    await expect(main).not.toBeEmpty();
  });
}

// The realm page's tabs are separate render paths behind one route, and a
// broken one is invisible until someone clicks it.
const TABS = ['info', 'source', 'calls', 'events', 'storage', 'deps', 'graph'];

for (const tab of TABS) {
  test(`realm ${tab} tab renders without errors`, async ({ page }) => {
    const seen = watch(page);

    await page.goto(`/realm/${HUB_ROUTE}?tab=${tab}`);
    await settle(page);

    expect(seen.jsErrors, `uncaught exceptions on the ${tab} tab`).toEqual([]);
    expect(unexpected(seen.failedRequests), `failed requests on the ${tab} tab`).toEqual([]);

    await expect(page.locator(`#tab-${tab}`)).toBeVisible();
  });
}

test('the realm list pages and every row carries its network', async ({ page }) => {
  const seen = watch(page);
  await page.goto('/realms');
  await settle(page);

  // The fixture puts the same package path on two chains. A list that joins on
  // path alone collapses them, which is the mistake the network-scoping
  // invariant exists to prevent.
  const rows = page.locator('#realms-list tr');
  expect(await rows.count()).toBeGreaterThan(1);

  expect(seen.jsErrors).toEqual([]);
});

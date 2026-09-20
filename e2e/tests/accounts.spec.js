import { expect, test } from '@playwright/test';

import { settle, unexpected, watch } from './helpers.js';

// The accounts page: two rankings and a population summary.
//
// The harness has no RPC, so no balance is ever swept here and the rich list is
// legitimately empty. That is worth a test of its own, because "nothing swept
// yet" and "this chain has no balances" look identical unless the page says
// which, and it has to say which.

const RICH = {
  network: 'alpha',
  entries: [
    { rank: 1, address: 'g1arpgqtq9q3emx6mfhuz7xuwd6qptcwynedzlk2', amount: '8837132922860ugnot', ugnot: 8837132922860, height: 176792, fetched_at: '2026-09-20T00:13:07Z' },
    { rank: 2, address: 'g1yaaa6rcp4ew5yjzdj4yms596wx2dtrj3a86704', amount: '500ugnot', ugnot: 500, height: 176792, fetched_at: '2026-09-20T00:13:07Z' },
  ],
  coverage: { swept: 400, known: 1327, oldest_fetch: '2026-09-20T00:13:07Z', newest_fetch: '2026-09-20T00:13:07Z' },
};

test('the population summary labels what each number counts', async ({ page }) => {
  const seen = watch(page);

  const response = await page.goto('/accounts');
  expect(response.status()).toBe(200);
  await settle(page);

  const stats = page.locator('#accounts-stats');
  // Deliberately not "total accounts": gno cannot enumerate the auth module, so
  // that number is unavailable at any price and this one would be wearing its
  // name.
  await expect(stats).toContainText('addresses seen');
  await expect(stats).toContainText('active 24h');
  await expect(stats).toContainText('active 7d');
  await expect(stats).toContainText('active 30d');

  expect(seen.jsErrors, 'uncaught exceptions').toEqual([]);
  expect(unexpected(seen.consoleErrors), 'console errors').toEqual([]);
});

test('the balances tab ranks, labels and states its coverage', async ({ page }) => {
  const seen = watch(page);
  await page.route('**/api/accounts/rich*', route =>
    route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(RICH) }));

  await page.goto('/accounts');
  await settle(page);
  await page.click('#accounts-subnav .tab[data-av="balances"]');
  await settle(page);

  const view = page.locator('#accounts-view-balances');

  // The boundary, printed rather than implied. This ranks what has been swept,
  // not a chain's accounts.
  await expect(view).toContainText('400');
  await expect(view).toContainText('1,327');

  await expect(view).toContainText('#1');
  // The registry label travels with the ranking: without it the top of a rich
  // list is infrastructure that reads as whales.
  await expect(view).toContainText('@gpao_oracle');

  // And the tab survives a reload, the same way the packages tabs do.
  await expect(page).toHaveURL(/[?&]av=balances(&|$)/);
  await page.reload();
  await settle(page);
  await expect(page.locator('#accounts-view-balances')).toBeVisible();
  await expect(page.locator('#accounts-view-activity')).toBeHidden();

  expect(seen.jsErrors, 'uncaught exceptions').toEqual([]);
});

test('an empty ranking says nothing has been swept, not that there is nothing', async ({ page }) => {
  const seen = watch(page);

  await page.goto('/accounts?av=balances');
  await settle(page);

  // The real path: this harness configures no RPC, so no sweep ever runs.
  await expect(page.locator('#accounts-view-balances')).toContainText('no balances read yet');

  expect(seen.jsErrors, 'uncaught exceptions').toEqual([]);
  expect(unexpected(seen.failedRequests), 'failed requests').toEqual([]);
});

test('the activity tab still ranks by what an address did', async ({ page }) => {
  const seen = watch(page);

  await page.goto('/accounts');
  await settle(page);

  await expect(page.locator('#accounts-view-activity')).toBeVisible();
  await expect(page.locator('#accounts-list tr').first()).toBeVisible();

  expect(seen.jsErrors, 'uncaught exceptions').toEqual([]);
  expect(unexpected(seen.consoleErrors), 'console errors').toEqual([]);
});

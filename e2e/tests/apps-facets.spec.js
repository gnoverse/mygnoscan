import { expect, test } from '@playwright/test';

import { settle, unexpected, watch } from './helpers.js';

// The facets: three questions the category chips could not answer.
//
// A category says what kind of thing an app is, and there are four of them. The
// questions people actually arrive with are about where an entry came from and
// what it is: did a human write this or did the chain propose it, is there a
// realm behind it, is there a front end. Each is a URL parameter, so any
// combination of them is a link somebody can send.

const chip = (page, name) => page.locator('#apps-content .dash-seg button')
  .filter({ hasText: name });

test('a facet filters the grid, and says how many it would leave', async ({ page }) => {
  const seen = watch(page);

  await page.goto('/apps?network=alpha');
  await settle(page);
  const content = page.locator('#apps-content');

  const all = await content.locator('.app-grid .app-card').count();

  // The chip carries its own count, so a filter that would empty the page says
  // so before it is clicked rather than after.
  const realmChip = chip(page, 'a realm');
  await expect(realmChip).toHaveText(/a realm \d+/);

  await realmChip.click();
  await settle(page);
  await expect(page).toHaveURL(/[?&]has=realm/);

  const filtered = await content.locator('.app-grid .app-card').count();
  expect(filtered).toBeGreaterThan(0);
  expect(filtered).toBeLessThan(all);

  // Everything left is on a chain, and the off-chain half is gone.
  await expect(content).not.toContainText('off chain');

  expect(seen.jsErrors, 'uncaught exceptions').toEqual([]);
  expect(unexpected(seen.consoleErrors), 'console errors').toEqual([]);
});

test('the source facet separates what a human wrote from what the chain proposed', async ({ page }) => {
  await page.goto('/apps?network=alpha');
  await settle(page);
  const content = page.locator('#apps-content');

  await chip(page, 'on awesome-gno').click();
  await settle(page);
  await expect(page).toHaveURL(/[?&]src=community/);
  // A wallet is not a realm and never will be, so it can only come from there.
  await expect(content.getByText('Adena Wallet', { exact: true })).toBeVisible();

  await chip(page, 'auto-discovered').click();
  await settle(page);
  await expect(page).toHaveURL(/[?&]src=discovered/);
  // Nothing a human named: that is what auto-discovered means.
  await expect(content.getByText('Adena Wallet', { exact: true })).toHaveCount(0);
  await expect(content.getByText('gno.land blog', { exact: true })).toHaveCount(0);
});

test('two facets combine, and both survive a reload', async ({ page }) => {
  await page.goto('/apps?network=alpha&src=community&has=realm');
  await settle(page);
  const content = page.locator('#apps-content');

  const on = content.locator('.dash-seg button.on');
  await expect(on.filter({ hasText: 'on awesome-gno' })).toBeVisible();
  await expect(on.filter({ hasText: 'a realm' })).toBeVisible();

  await page.reload();
  await settle(page);
  await expect(content.locator('.dash-seg button.on').filter({ hasText: 'a realm' })).toBeVisible();

  // An empty result says so plainly rather than leaving a blank page.
  const cards = await content.locator('.app-grid .app-card').count();
  if (cards === 0) await expect(content).toContainText('nothing matches these filters');
});

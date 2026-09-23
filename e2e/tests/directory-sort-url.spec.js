// The directory's ordering belongs in the URL, like every other control on
// these pages.
//
// It used to be a variable and nothing else, with two consequences that only
// showed up together: a sorted directory could not be linked, and a reload
// dropped the server ordering back to the default while the page-local order
// restored from the URL, so the table was the newest fifty rows sorted by
// something else.
import { expect, test } from '@playwright/test';

import { settle, unexpected, watch } from './helpers.js';

const symbolsColumn = async (page) =>
  (await page.locator('#packages-list tr td:nth-child(4)').allTextContents())
    .map(t => Number(t.replace(/[^0-9]/g, '')) || 0);

test('a sort can be linked', async ({ page }) => {
  const seen = watch(page);
  await page.goto('/packages?kind=all&sort=symbols');
  await settle(page);

  const nums = await symbolsColumn(page);
  expect(nums.length).toBeGreaterThan(2);
  for (let i = 1; i < nums.length; i++) {
    expect(nums[i]).toBeLessThanOrEqual(nums[i - 1]);
  }
  expect(nums[0]).toBeGreaterThan(0);

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.consoleErrors)).toEqual([]);
});

test('clicking a column writes the sort, and a reload keeps it', async ({ page }) => {
  await page.goto('/packages?kind=all');
  await settle(page);

  await page.locator('#view-packages th[data-sort="symbols"]').click();
  await settle(page);
  expect(new URL(page.url()).searchParams.get('sort')).toBe('symbols');
  const afterClick = await symbolsColumn(page);

  await page.reload();
  await settle(page);
  // The same order, not the default one re-sorted page-locally on top.
  await expect.poll(() => symbolsColumn(page)).toEqual(afterClick);
});

// A hand-edited URL must not reach the API with a key it does not understand,
// and must not produce an ordering nobody asked for that looks deliberate.
test('an unknown sort falls back to the default rather than being passed on', async ({ page }) => {
  const asked = [];
  page.on('request', r => {
    const u = r.url();
    if (u.includes('/api/packages?')) asked.push(new URL(u).searchParams.get('sort'));
  });
  await page.goto('/packages?kind=all&sort=nonsense');
  await settle(page);
  expect(asked.length).toBeGreaterThan(0);
  for (const s of asked) expect(s).toBe('newest');
});

// Each listing keeps its own first view: most recently called for realms,
// newest for packages.
test('each listing keeps its own default', async ({ page }) => {
  const asked = { realms: [], packages: [] };
  page.on('request', r => {
    const u = r.url();
    if (u.includes('/api/realms?')) asked.realms.push(new URL(u).searchParams.get('sort'));
    if (u.includes('/api/packages?')) asked.packages.push(new URL(u).searchParams.get('sort'));
  });
  await page.goto('/realms');
  await settle(page);
  await page.goto('/packages');
  await settle(page);
  expect(asked.realms.length).toBeGreaterThan(0);
  expect(asked.packages.length).toBeGreaterThan(0);
  for (const s of asked.realms) expect(s).toBe('last_call');
  for (const s of asked.packages) expect(s).toBe('newest');
});

// navigate() passes a bare path, so leaving the page drops the sort rather
// than carrying it onto the next listing.
test('a navigation drops the sort', async ({ page }) => {
  await page.goto('/packages?kind=all&sort=symbols');
  await settle(page);
  await page.locator('.rail nav a[href="/realms"], .rail nav a', { hasText: 'realms' }).first().click();
  await settle(page);
  expect(new URL(page.url()).searchParams.get('sort')).toBeNull();
});

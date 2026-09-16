import { expect, test } from '@playwright/test';

import { settle, unexpected, watch } from './helpers.js';

// The home page's leaderboards, asserted on content rather than on absence of
// errors.
//
// pages.spec.js already covers "the route renders and nothing throws", and an
// empty table passes that: the section heading draws, the tbody stays empty,
// and no console error is produced. What these assert is that the panel is
// actually populated, which is the only reason it was added.
test('the home page lists the most active accounts', async ({ page }) => {
  const seen = watch(page);
  await page.goto('/');
  await settle(page);

  const rows = page.locator('#top-accounts tr');
  await expect.poll(() => rows.count(), {
    message: 'the accounts leaderboard should have rows, not just a heading',
  }).toBeGreaterThan(0);

  // The empty-state row is a single cell spanning the table; a real row has one
  // cell per column. Distinguishing them is the point — the empty state is
  // exactly what this test exists to fail on.
  const firstRowCells = await page.locator('#top-accounts tr').first().locator('td').count();
  expect(firstRowCells, 'expected a populated row, got the empty-state placeholder').toBe(4);

  // Every row links to an address, which is what makes the panel a route into
  // /accounts rather than a decorative table.
  const firstCell = page.locator('#top-accounts tr').first().locator('td').first();
  await expect(firstCell).toContainText(/g1/);

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.consoleErrors)).toEqual([]);
});

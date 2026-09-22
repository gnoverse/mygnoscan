// The package directory's facets: which half of it you are looking at, and
// whose packages those are.
//
// A flat list of every deployed path answers "what exists" and stops there.
import { expect, test } from '@playwright/test';

import { settle, unexpected, watch } from './helpers.js';

const FACETS = '#packages-facets';

test('the kind facet switches which half of the directory is listed', async ({ page }) => {
  const seen = watch(page);
  await page.goto('/packages');
  await settle(page);

  const kindRow = page.locator(`${FACETS} .facet-row`).first();
  await expect(kindRow).toContainText('everything');
  await expect(kindRow).toContainText('realms');
  await expect(kindRow).toContainText('pure packages');

  // The route's own default is the pure half, so that chip reads as active
  // before anything is clicked. A control with nothing selected is a control
  // that does not say what you are looking at.
  await expect(kindRow.locator('.facet.active')).toHaveText(/pure packages/);

  const pureRows = await page.locator('#packages-list tr').count();
  await kindRow.getByRole('button', { name: /everything/ }).click();
  await settle(page);
  const allRows = await page.locator('#packages-list tr').count();
  expect(allRows).toBeGreaterThan(pureRows);
  await expect(kindRow.locator('.facet.active')).toHaveText(/everything/);

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.consoleErrors)).toEqual([]);
});

// A filtered view has to be a link, or a reader who filtered has to explain
// which controls to click instead of sending the URL.
test('the facets live in the URL and survive a reload', async ({ page }) => {
  await page.goto('/packages');
  await settle(page);

  await page.locator(`${FACETS} .facet-row`).first().getByRole('button', { name: /everything/ }).click();
  await settle(page);
  await expect(page).toHaveURL(/kind=all/);

  const nsRow = page.locator(`${FACETS} .facet-row`).nth(1);
  await expect(nsRow).toContainText('namespace');
  const firstNs = nsRow.locator('.facet').nth(1); // [0] is "any"
  const nsName = (await firstNs.locator('.facet-name').textContent()).trim();
  await firstNs.click();
  await settle(page);
  await expect(page).toHaveURL(new RegExp('namespace=' + nsName));

  const filtered = await page.locator('#packages-list tr').count();
  expect(filtered).toBeGreaterThan(0);

  await page.reload();
  await settle(page);
  await expect(page.locator('#packages-list tr')).toHaveCount(filtered);
  await expect(nsRow.locator('.facet.active').locator('.facet-name')).toHaveText(nsName);
});

// Picking one facet narrows the other's counts, or the control is describing a
// listing the reader is not looking at. Picking a facet must not narrow
// itself, or there is no way to see what switching would give you.
test('the counts narrow each other but not themselves', async ({ page }) => {
  await page.goto('/packages?kind=all');
  await settle(page);

  const kindRow = page.locator(`${FACETS} .facet-row`).first();
  const nsRow = page.locator(`${FACETS} .facet-row`).nth(1);
  const kindChipsBefore = await kindRow.locator('.facet').count();
  const everythingBefore = await kindRow.getByRole('button', { name: /everything/ }).textContent();

  const firstNs = nsRow.locator('.facet').nth(1);
  const nsName = (await firstNs.locator('.facet-name').textContent()).trim();
  await firstNs.click();
  await settle(page);

  // Every kind is still offered, with counts now describing the namespace.
  await expect(kindRow.locator('.facet')).toHaveCount(kindChipsBefore);
  const everythingAfter = await kindRow.getByRole('button', { name: /everything/ }).textContent();
  expect(everythingAfter).not.toBe(everythingBefore);

  // And the namespace row still lists the others, so the reader can switch.
  expect(await nsRow.locator('.facet').count()).toBeGreaterThan(2);
  await expect(nsRow.locator('.facet.active').locator('.facet-name')).toHaveText(nsName);
});

test('the symbols column ranks the pure half, which every other sort cannot', async ({ page }) => {
  await page.goto('/packages?kind=all');
  await settle(page);

  const header = page.locator('#view-packages th[data-sort="symbols"]');
  await expect(header).toBeVisible();
  await header.click();
  await settle(page);

  const counts = await page.locator('#packages-list tr td:nth-child(4)').allTextContents();
  const nums = counts.map(t => Number(t.replace(/[^0-9]/g, '')) || 0);
  expect(nums.length).toBeGreaterThan(1);
  // Descending, which is what "most symbols" means.
  for (let i = 1; i < nums.length; i++) {
    expect(nums[i]).toBeLessThanOrEqual(nums[i - 1]);
  }
  expect(nums[0]).toBeGreaterThan(0);
});

test('a package with symbols links into its docs', async ({ page }) => {
  await page.goto('/packages?kind=all&sort=symbols');
  await settle(page);
  const cell = page.locator('#packages-list tr td:nth-child(4) a').first();
  await expect(cell).toBeVisible();
  await cell.click();
  await expect(page).toHaveURL(/tab=docs/);
});

// A directory's order belongs to the server, over every row rather than over
// the fifty on screen.
//
// The generic click-to-sort enhancer used to attach to these headers as well,
// and it ran second, so it re-sorted the painted page *ascending* on top of the
// server's descending answer. "Sort by calls" showed the realms with the fewest
// calls, under a header promising the most, and wrote a junk key into the URL
// on the way. This is not about the facets; it was live on every directory.
test('a server-sorted column sorts descending, and only once', async ({ page }) => {
  await page.goto('/realms');
  await settle(page);

  await page.locator('#view-realms th[data-sort="calls"]').click();
  await settle(page);

  const calls = (await page.locator('#realms-list tr td:nth-child(4)').allTextContents())
    .map(t => Number(t.replace(/[^0-9]/g, '')) || 0);
  expect(calls.length).toBeGreaterThan(2);
  for (let i = 1; i < calls.length; i++) {
    expect(calls[i], 'most-called first, not fewest').toBeLessThanOrEqual(calls[i - 1]);
  }

  // And nothing writes a client-sort key for a sort it does not own.
  expect(page.url()).not.toMatch(/[?&]s\./);
});

// The enhancer still owns the tables whose order really is page-local.
test('a table the server does not order is still click-sortable', async ({ page }) => {
  await page.goto('/blocks');
  await settle(page);
  const table = page.locator('#view-blocks table').first();
  const th = table.locator('th').first();
  if (await th.count() === 0) test.skip();
  await expect(th).toHaveClass(/sortable/);
});

import { expect, test } from '@playwright/test';

import {
  STORAGE_DEPLOYER, STORAGE_HOG, STORAGE_HOG_BYTES, STORAGE_PAYER, STORAGE_TOTAL_BYTES,
} from '../harness/fixture.mjs';
import { SUPPLY_CAPACITY_BYTES } from '../harness/fake-indexer.mjs';
import { settle, unexpected, watch } from './helpers.js';

const MB = 1024 * 1024;

// The capacity figure is the page's whole premise, and it is the product of
// three separate reads: the price from a params query, the supply from the
// indexer, the used bytes from SQLite. Any of them silently defaulting would
// still draw a plausible page, so the number is asserted exactly.
test('the disk header states capacity, used and how full the chain is', async ({ page }) => {
  const seen = watch(page);
  await page.goto('/storage?network=alpha');
  await settle(page);

  const stats = page.locator('#storage-content .stats-bar').first();
  await expect(stats).toBeVisible();

  // The fixture's supply divided by 100 ugnot/byte is exactly 100 GiB.
  expect(SUPPLY_CAPACITY_BYTES).toBe(100 * 1024 * 1024 * 1024);
  await expect(stats).toContainText('100 GB');
  // 32 + 6 - 2 + 3 + 0.5 + 0.5 MiB.
  expect(STORAGE_TOTAL_BYTES).toBe(40 * MB);
  await expect(stats).toContainText('40.0 MB');
  await expect(stats).toContainText('100 ugnot/byte');

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.consoleErrors)).toEqual([]);
});

// The map is the feature. It has to draw a full grid every time: a short one
// would mean free space silently vanished rather than being shown as free.
test('the block map draws a full grid of claimed and free cells', async ({ page }) => {
  await page.goto('/storage?network=alpha');
  await settle(page);

  const cells = page.locator('#storage-content .storage-cell');
  await expect(cells).toHaveCount(64 * 24);
  // 40 MiB inside a 64 MiB window leaves a real amount free, so both kinds
  // must be present. Either count at zero means the window maths is wrong.
  const free = page.locator('#storage-content .storage-cell-free');
  expect(await free.count()).toBeGreaterThan(0);
  expect(await free.count()).toBeLessThan(64 * 24);
});

// A path deployed on two chains is two disks. beta holds 7 MiB of the same
// realm; if the grouping joined on path alone the alpha figure would be 39 MiB.
test('storage is scoped to the selected chain', async ({ page }) => {
  await page.goto('/storage?network=alpha&group=realm');
  await settle(page);
  const row = page.locator('#storage-content table tbody tr').filter({ hasText: 'r/hog/vault' }).first();
  expect(STORAGE_HOG_BYTES).toBe(32 * MB);
  await expect(row).toContainText('32.0 MB');
  expect(STORAGE_HOG).toBe('gno.land/r/hog/vault');

  await page.goto('/storage?network=beta&group=realm');
  await settle(page);
  const betaRow = page.locator('#storage-content table tbody tr').filter({ hasText: 'r/hog/vault' }).first();
  await expect(betaRow).toContainText('7.00 MB');
});

// Grouping by payer is a different question from grouping by deployer, and the
// answers differ in the fixture on purpose: the account that grew r/hub/core
// deployed nothing at all.
test('the payer grouping attributes bytes to whoever the chain charged', async ({ page }) => {
  await page.goto('/storage?network=alpha&group=payer');
  await settle(page);

  // Addresses are abbreviated in table cells, so match the visible prefix.
  const table = page.locator('#storage-content table');
  await expect(table).toContainText(STORAGE_PAYER.slice(0, 8));
  // The realm's 32 MiB is charged to its deployer, who paid at MsgAddPackage.
  await expect(table).toContainText(STORAGE_DEPLOYER.slice(0, 8));
  // The orphan event has no call, no deploy and no run behind it. Naming it as
  // unattributed is the point; picking an address for it would be the bug.
  await expect(table).toContainText('unattributed');

  // Payer and deployer are different questions, and the fixture makes them
  // disagree: the account that grew r/hub/core deployed nothing at all.
  await page.goto('/storage?network=alpha&group=deployer');
  await settle(page);
  const byDeployer = page.locator('#storage-content table');
  await expect(byDeployer).toContainText(STORAGE_DEPLOYER.slice(0, 8));
  await expect(byDeployer).not.toContainText(STORAGE_PAYER.slice(0, 8));
});

// The controls are the page's state, and the state is in the URL so a reading
// can be linked to.
test('group, order and window round-trip through the URL', async ({ page }) => {
  await page.goto('/storage?network=alpha');
  await settle(page);

  await page.getByRole('button', { name: 'namespace', exact: true }).click();
  await settle(page);
  await expect(page).toHaveURL(/group=namespace/);

  await page.getByRole('button', { name: 'size', exact: true }).click();
  await settle(page);
  await expect(page).toHaveURL(/order=size/);

  // Reloading the deep link has to land on the same reading, not the default.
  await page.reload();
  await settle(page);
  await expect(page).toHaveURL(/order=size/);
  await expect(page.locator('#storage-content table')).toBeVisible();
});

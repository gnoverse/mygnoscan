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

// The cross-highlight is the answer to "which blocks are this name, and which
// name is this block": one group is drawn three times on the page and hovering
// any of them has to light the other two. Asserted through the classes rather
// than a screenshot, because what can silently break is the wiring — a run that
// never registered, or an index left over from the previous render.
test('hovering a name lights its blocks, its row and the readout', async ({ page }) => {
  const seen = watch(page);
  await page.goto('/storage?network=alpha&group=namespace');
  await settle(page);

  const chip = page.locator('#storage-content .storage-name[data-skey="hog"]');
  await expect(chip).toBeVisible();
  await chip.hover();

  // Every lit cell is the hovered group's, and there is at least one: the
  // fixture's hog holds 32 of the 40 MiB, so it is most of the grid.
  const lit = page.locator('#storage-content .storage-cell.hl-match');
  expect(await lit.count()).toBeGreaterThan(0);
  expect(await page.locator('#storage-content .storage-cell.hl-match:not([data-skey="hog"])').count()).toBe(0);
  // And the rest of the map has receded, which is the half a class on the
  // matches alone cannot do.
  await expect(page.locator('#storage-content.hl-on')).toHaveCount(1);

  await expect(page.locator('#storage-content tbody tr.hl-match')).toHaveCount(1);
  await expect(page.locator('#storage-content tbody tr.hl-match')).toContainText('hog');
  await expect(page.locator('#storage-content .storage-readout')).toContainText('hog');
  await expect(page.locator('#storage-content .storage-readout')).toContainText('32.0 MB');

  // Leaving clears it: a highlight that survives the cursor is worse than none,
  // because the next hover then lights two groups at once.
  await page.locator('#storage-content .section-title').first().hover();
  await expect(page.locator('#storage-content.hl-on')).toHaveCount(0);
  expect(await page.locator('#storage-content .hl-match').count()).toBe(0);

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.consoleErrors)).toEqual([]);
});

// The other direction, and the reason the readout exists: a cell is six pixels
// square and nothing on the page says what it is until you hover it.
test('hovering a block names it and lights its legend chip', async ({ page }) => {
  await page.goto('/storage?network=alpha&group=namespace');
  await settle(page);

  const cell = page.locator('#storage-content .storage-cell[data-skey]').first();
  const key = await cell.getAttribute('data-skey');
  await cell.hover();

  await expect(page.locator('#storage-content .storage-readout')).toContainText(key);
  await expect(page.locator(`#storage-content .storage-name[data-skey="${key}"].hl-match`)).toHaveCount(1);
});

// Share of capacity was a column here and it was unreadable: at 300,000 to 1
// every row rendered as an exponent. The replacement has to be the running
// total, and the running total has to land on what is in use.
test('the table runs a cumulative share instead of an unreadable capacity share', async ({ page }) => {
  await page.goto('/storage?network=alpha&group=realm');
  await settle(page);

  const head = page.locator('#storage-content table thead');
  await expect(head).toContainText('cumulative');
  await expect(head).not.toContainText('share of capacity');
  // No exponent anywhere in the table: that notation is exactly what made the
  // old column unreadable, and nothing else here should produce one.
  await expect(page.locator('#storage-content table tbody')).not.toContainText('e-');

  // Summed down the rows, signed, the column ends on everything in use.
  const last = page.locator('#storage-content table tbody tr').last();
  await expect(last.locator('td').nth(3)).toContainText('100%');
});

// Wherever this site lists addresses it shows the identicon beside the text and
// names the account when it can. The legend is a listing of addresses whenever
// the grouping is payer or deployer, and it used to be the one that forgot.
test('address groupings carry the identicon and the name in the legend', async ({ page }) => {
  await page.goto('/storage?network=alpha&group=payer');
  await settle(page);

  const chip = page.locator(`#storage-content .storage-name[data-skey="${STORAGE_PAYER}"]`);
  await expect(chip).toBeVisible();
  await expect(chip.locator('.identicon')).toHaveCount(1);
  // Abbreviated, not raw: forty characters of bech32 is not a legend entry.
  await expect(chip).not.toContainText(STORAGE_PAYER);
  await expect(chip).toContainText(STORAGE_PAYER.slice(0, 8));

  await chip.hover();
  await expect(page.locator('#storage-content .storage-readout .identicon')).toHaveCount(1);
});

// The ruler has to be able to say both things: the log scale is the only one
// that fits 40 MiB and 100 GiB on one axis, and it is also the one that looks
// like the chain is a third full. The toggle is the answer to that, so it has
// to survive a reload like every other control on this page.
test('the ruler scale toggles between log and max capacity, and stays in the URL', async ({ page }) => {
  const seen = watch(page);
  await page.goto('/storage?network=alpha');
  await settle(page);

  const ruler = page.locator('#storage-content .section').filter({ hasText: 'used against capacity' });
  await expect(ruler).toContainText('log scale, one tick per factor of 1024');

  await page.getByRole('button', { name: 'max capacity', exact: true }).click();
  await settle(page);
  await expect(page).toHaveURL(/scale=linear/);
  await expect(ruler).toContainText('to scale: the whole bar is 100 GB');
  await expect(ruler).not.toContainText('log scale, one tick');

  await page.reload();
  await settle(page);
  await expect(ruler).toContainText('to scale: the whole bar is 100 GB');

  // And back, which has to clear the parameter rather than leave scale=log in
  // every link copied off this page.
  await page.getByRole('button', { name: 'log', exact: true }).click();
  await settle(page);
  await expect(page).not.toHaveURL(/scale=/);
  await expect(ruler).toContainText('log scale, one tick per factor of 1024');

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.consoleErrors)).toEqual([]);
});

// The point of the second scale is that the fill is invisibly small, so the one
// thing that would break it is a fill drawn at some readable width anyway.
test('the max capacity scale draws the fill to scale, on a two-pixel floor', async ({ page }) => {
  await page.goto('/storage?network=alpha&scale=linear');
  await settle(page);

  const track = page.locator('#storage-content .storage-ruler-track').first();
  const fill = page.locator('#storage-content .storage-ruler-fill').first();
  const trackBox = await track.boundingBox();
  const fillBox = await fill.boundingBox();

  // 40 MiB of 100 GiB is 0.039%, which is well under a pixel on any width this
  // suite runs at, so the floor is what should be showing.
  expect(STORAGE_TOTAL_BYTES / SUPPLY_CAPACITY_BYTES).toBeLessThan(0.001);
  expect(fillBox.width).toBeGreaterThan(0);
  expect(fillBox.width).toBeLessThanOrEqual(4);
  expect(fillBox.width / trackBox.width).toBeLessThan(0.01);

  // And the caption says so, with the measurement it actually took.
  await expect(page.locator('#storage-content .section').filter({ hasText: 'used against capacity' }))
    .toContainText(/of one pixel, so the fill is drawn at a floor of two/);

  // The log scale, by contrast, fills a real part of the bar: that is the whole
  // reason it needs the caption it carries.
  await page.goto('/storage?network=alpha');
  await settle(page);
  const logFill = await page.locator('#storage-content .storage-ruler-fill').first().boundingBox();
  const logTrack = await page.locator('#storage-content .storage-ruler-track').first().boundingBox();
  expect(logFill.width / logTrack.width).toBeGreaterThan(0.1);
});

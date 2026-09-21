import { expect, test } from '@playwright/test';

import { settle, unexpected, watch } from './helpers.js';

// The per-validator page.
//
// /validators already rendered the set; what it could not do is show one
// validator over time, because the table is a snapshot and a validator's story
// is a history.

const DETAIL = {
  network: 'alpha',
  in_set: true,
  validator: {
    address: 'g1val_a', name: 'val-a', voting_power: 60, spof: false,
    missed_100: 0, missed_24h: 3, avg_block_ms: 3300,
    blocks: 400, txs: 12, share: 0.6666, last_block_time: '2026-09-20T00:50:00Z',
  },
  blocks: [{ height: 105, time: '2026-09-20T00:50:00Z', num_txs: 2 }],
  shares: [{ day: '2026-09-19', blocks: 300, shares: { g1val_a: 0.5 } }],
};

test('a validator page shows its history and says what the share series is', async ({ page }) => {
  const seen = watch(page);
  await page.route('**/api/validator/g1val_a*', route =>
    route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(DETAIL) }));

  const response = await page.goto('/validator/g1val_a');
  expect(response.status()).toBe(200);
  await settle(page);

  const detail = page.locator('#validator-detail-content');
  await expect(detail).toContainText('blocks proposed');
  await expect(detail).toContainText('recent blocks proposed');
  await expect(detail).toContainText('consensus address g1val_a');

  // No historical validator set is stored anywhere, so the share series is the
  // observable half of one and the page has to say so rather than presenting it
  // as a voting-power timeline.
  await expect(detail).toContainText('no historical validator set is recorded');

  expect(seen.jsErrors, 'uncaught exceptions').toEqual([]);
  expect(unexpected(seen.consoleErrors), 'console errors').toEqual([]);
});

test('a validator that has left the set says so', async ({ page }) => {
  const seen = watch(page);
  await page.route('**/api/validator/g1val_a*', route =>
    route.fulfill({
      status: 200, contentType: 'application/json',
      body: JSON.stringify({ ...DETAIL, in_set: false }),
    }));

  await page.goto('/validator/g1val_a');
  await settle(page);

  // An address with history but no place in the current set would otherwise
  // read as an active validator.
  await expect(page.locator('#validator-detail-content')).toContainText('not in the current set');

  expect(seen.jsErrors, 'uncaught exceptions').toEqual([]);
});

test('the set table links through to each validator', async ({ page }) => {
  const seen = watch(page);

  await page.goto('/validators');
  await settle(page);

  // The link is on the proposer rows, which carry the consensus address: the
  // registration table below uses operator addresses and nothing maps the two.
  const history = page.locator('#validators-content').getByText('history', { exact: true }).first();
  await expect(history).toBeVisible();
  await history.click();
  await expect(page).toHaveURL(/\/validator\/g1/);

  expect(seen.jsErrors, 'uncaught exceptions').toEqual([]);
});

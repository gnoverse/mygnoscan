import { expect, test } from '@playwright/test';

import { settle, unexpected, watch } from './helpers.js';

// The per-validator liveness sparkline.
//
// The share column beside it answers "whose blocks are these historically",
// which is a different question and cannot go stale: a validator that stopped
// proposing an hour ago carries exactly the share it always did. So the thing
// worth asserting is that the bars reflect *recent* blocks per validator, not
// merely that some bars were drawn.
test('each validator row carries a recent-liveness sparkline', async ({ page }) => {
  const seen = watch(page);
  await page.goto('/validators');
  await settle(page);

  const sparks = page.locator('.spark');
  await expect.poll(() => sparks.count(), {
    message: 'every validator row should carry a sparkline',
  }).toBeGreaterThan(0);

  // One bar per recent block, and at least one of them filled for a validator
  // that appears in the proposer ranking at all — an all-empty sparkline on a
  // validator with blocks to its name would mean the bars are not keyed to the
  // right address.
  const first = sparks.first();
  const bars = await first.locator('i').count();
  expect(bars, 'the sparkline should have one bar per recent block').toBeGreaterThan(0);
  expect(bars, 'and no more than the window it claims to cover').toBeLessThanOrEqual(20);

  const filled = await first.locator('i.hit').count();
  expect(filled, 'the top proposer should have proposed at least one recent block').toBeGreaterThan(0);

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.consoleErrors)).toEqual([]);
});

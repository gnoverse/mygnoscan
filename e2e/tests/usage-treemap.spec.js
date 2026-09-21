import { expect, test } from '@playwright/test';

import {
  USAGE_BID_CALLS, USAGE_CALLERS, USAGE_EXPORTED, USAGE_MESSAGES, USAGE_ROUTE,
} from '../harness/fixture.mjs';
import { unexpected, watch } from './helpers.js';

// The treemap over the calls tab's aggregates: what this realm is used for, and
// who uses it, as areas rather than as two sorted tables.
//
// The fixture realm exports four functions and has been called on two of them.
// Bid is 6 calls from 3 addresses with one failure; Claim is 1 call; Render and
// Withdraw have never been called at all. One caller signs a transaction
// carrying two Bids, which is what makes messages and transactions differ.

const CALLED = ['Bid', 'Claim'];
const NEVER_CALLED = USAGE_EXPORTED.filter(f => !CALLED.includes(f));
const MOUNT = '#usage-treemap';

async function openCalls(page) {
  await page.goto(`/realm/${USAGE_ROUTE}?network=alpha&tab=calls`);
  await page.waitForSelector(`${MOUNT} svg`, { timeout: 20_000 });
}

function cellLabels(page) {
  return page.locator(`${MOUNT} svg text`).allTextContents();
}

test('the treemap names the functions that carry the traffic', async ({ page }) => {
  const seen = watch(page);
  await openCalls(page);

  const labels = await cellLabels(page);
  for (const fn of CALLED) expect(labels).toContain(fn);
  // A function with no calls has no area, so it cannot be a cell here without
  // the one encoding this chart has saying something false.
  for (const fn of NEVER_CALLED) expect(labels).not.toContain(fn);

  // Area is calls, and the total is printed because an area is a comparison
  // rather than a reading.
  await expect(page.locator(MOUNT)).toContainText(`area is calls, ${USAGE_BID_CALLS + 1} in total`);

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.failedRequests)).toEqual([]);
});

test('what the realm exports and nobody has called sits beside the treemap, not inside it', async ({ page }) => {
  const seen = watch(page);
  await openCalls(page);

  const strip = page.locator(MOUNT);
  await expect(strip).toContainText(
    `${NEVER_CALLED.length} of ${USAGE_EXPORTED.length} exported functions have never been called`);
  for (const fn of NEVER_CALLED) {
    await expect(page.locator(`${MOUNT} .fnpill.uncalled`, { hasText: fn })).toHaveCount(1);
  }

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.failedRequests)).toEqual([]);
});

test('"never called" disappears under a filter, because then it only means "not in this slice"', async ({ page }) => {
  const seen = watch(page);
  await openCalls(page);
  await expect(page.locator(MOUNT)).toContainText('never been called');

  // Narrow to a week. The fixture's calls are older than that, so this is the
  // case where the claim would be most wrong: every function is "never called"
  // inside an empty window.
  await page.locator('.usage-filters button', { hasText: '7d' }).first().click();
  await page.waitForFunction(
    () => !document.querySelector('#usage-treemap')?.textContent.includes('never been called'),
    null, { timeout: 15_000 },
  );

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.failedRequests)).toEqual([]);
});

test('the caller view is sized by messages, not by transactions', async ({ page }) => {
  const seen = watch(page);
  await openCalls(page);

  await page.locator(`${MOUNT} button`, { hasText: 'callers' }).first().click();
  await page.waitForSelector(`${MOUNT} svg text`, { timeout: 15_000 });

  // A multicall is several MsgCalls inside one signed transaction. Sizing by
  // transactions would draw one cell for work that happened twice, and the two
  // totals differ in this fixture precisely so that cannot pass unnoticed.
  const txs = Object.values(USAGE_CALLERS).reduce((a, [, t]) => a + t, 0);
  expect(USAGE_MESSAGES).not.toBe(txs);
  await expect(page.locator(MOUNT)).toContainText(`area is messages, ${USAGE_MESSAGES} in total`);

  // Every caller is a cell, under whatever name the site knows it by.
  const labels = await cellLabels(page);
  expect(labels.filter(l => l.includes('g1rumble') || l.includes('...')).length)
    .toBeGreaterThanOrEqual(Object.keys(USAGE_CALLERS).length - 1);

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.failedRequests)).toEqual([]);
});

test('clicking a cell narrows the whole tab to it', async ({ page }) => {
  const seen = watch(page);
  await openCalls(page);

  // The Bid cell is the largest, so its label is the one certain to be drawn.
  await page.locator(`${MOUNT} svg text`, { hasText: /^Bid$/ }).first().click();

  // The filter chip is the tab's own record of what it is showing, and the
  // summary is recomputed server-side over the filtered set rather than over
  // the page.
  await expect(page.locator('.usage-chip')).toContainText('Bid', { timeout: 15_000 });
  await expect(page.locator(MOUNT)).toContainText(`area is calls, ${USAGE_BID_CALLS} in total`);

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.failedRequests)).toEqual([]);
});

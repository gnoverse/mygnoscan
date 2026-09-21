import { expect, test } from '@playwright/test';

import { HUB_ROUTE } from '../harness/fixture.mjs';
import {
  LIFECYCLE_ENABLED_HEIGHT, LIFECYCLE_SUBMITTED_HEIGHT,
} from '../harness/fake-indexer.mjs';
import { settle, unexpected, watch } from './helpers.js';

// The inert lifecycle used to be a tab of its own on the realm page. For
// almost every path its entire content was one status badge, so it now lives
// in info. Three things have to hold for that fold to be finished rather than
// half-done: the tab is gone, its content is in info, and the old deep link
// still lands somewhere sensible.

const TABS = ['info', 'docs', 'source', 'calls', 'events', 'storage', 'defi', 'deps'];

// The page prints heights through fmtNum, which is toLocaleString.
const height = n => n.toLocaleString('en-US');

test('the realm page has no inert tab, and draws its lifecycle in info', async ({ page }) => {
  const seen = watch(page);
  await page.goto(`/realm/${HUB_ROUTE}?network=alpha`);
  await settle(page);

  // By data-tab rather than by text: the strip now appends a count to a tab
  // that has one, so "which tabs, in which order" is no longer the same
  // question as "what do they read".
  expect(await page.locator('#realm-tabs .tab').evaluateAll(
    els => els.map(e => e.dataset.tab))).toEqual(TABS);
  expect(await page.locator('#tab-inert').count()).toBe(0);

  const info = page.locator('#tab-info');
  await expect(info).toHaveClass(/active/);
  await expect(info).toContainText('submission history');
  const rows = info.locator('.section-title', { hasText: 'submission history' })
    .locator('xpath=following-sibling::table[1]/tbody/tr');
  await expect(rows).toHaveCount(2);
  // Oldest first: the submission, then the approval that names the height it
  // was parked at. Newest first would put the approval above the thing it
  // approved.
  await expect(rows.nth(0)).toContainText('submitted');
  await expect(rows.nth(0)).toContainText(height(LIFECYCLE_SUBMITTED_HEIGHT));
  await expect(rows.nth(1)).toContainText('enabled');
  await expect(rows.nth(1)).toContainText(height(LIFECYCLE_ENABLED_HEIGHT));

  // No RPC here, so vm/qpkgmeta_json cannot answer and the handler reports
  // "absent". That is a failed read, not a package the chain has never heard
  // of, so the info table must not grow a submission row claiming otherwise.
  await expect(info.locator('> table td').filter({ hasText: /^submission$/ })).toHaveCount(0);

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.consoleErrors)).toEqual([]);
});

test('?tab=inert lands on info and drops the dead parameter', async ({ page }) => {
  const seen = watch(page);
  await page.goto(`/realm/${HUB_ROUTE}?network=alpha&tab=inert`);
  await settle(page);

  await expect(page.locator('#tab-info')).toHaveClass(/active/);
  await expect(page.locator('#realm-tabs .tab.active')).toHaveText('info');
  expect(new URL(page.url()).searchParams.get('tab')).toBeNull();
  // Dropping the tab must not take the network with it: the page is still
  // reading alpha, and a URL that lost it would silently fall back.
  expect(new URL(page.url()).searchParams.get('network')).toBe('alpha');

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.consoleErrors)).toEqual([]);
});

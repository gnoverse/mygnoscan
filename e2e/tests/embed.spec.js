import { expect, test } from '@playwright/test';

import { HUB_ROUTE } from '../harness/fixture.mjs';
import { settle, unexpected, watch } from './helpers.js';

// Embed mode: ?embed=1 strips the chrome so a host app can iframe one piece of
// content rather than a whole page-in-a-page.
test('embed mode hides the chrome and keeps the content', async ({ page }) => {
  const seen = watch(page);
  await page.goto(`/realm/${HUB_ROUTE}?tab=graph&embed=1`);
  await settle(page);

  await expect(page.locator('header')).toBeHidden();
  await expect(page.locator('footer')).toBeHidden();

  // The point of the mode is what remains, not what is gone: a blank iframe
  // would satisfy the two assertions above.
  await expect(page.locator('#dep-graph > svg')).toBeVisible();

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.consoleErrors)).toEqual([]);
});

// Without the flag nothing changes. Worth pinning because the class is applied
// from an inline script before the body exists, which is easy to get wrong in
// the direction of applying always.
test('the chrome is present without the flag', async ({ page }) => {
  await page.goto(`/realm/${HUB_ROUTE}?tab=graph`);
  await settle(page);

  await expect(page.locator('header')).toBeVisible();
  await expect(page.locator('footer')).toBeVisible();
});

// The class has to be on the document element before first paint. Applying it
// from the SPA's own routing would let the header render and then vanish, which
// inside an iframe reads as a layout glitch on every navigation.
test('embed mode applies before the page renders', async ({ page }) => {
  // Checked at document-start, before any of the app's own code has run.
  await page.goto('/?embed=1', { waitUntil: 'commit' });
  const early = await page.evaluate(() => document.documentElement.classList.contains('embed'));
  expect(early, 'the embed class should be set before the app boots').toBe(true);
});

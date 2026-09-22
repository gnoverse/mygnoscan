import { expect, test } from '@playwright/test';

import { HUB_ROUTE } from '../harness/fixture.mjs';
import { settle, unexpected, watch } from './helpers.js';

// The state tab reads a realm's live object graph over RPC, and this harness
// has no RPC (see EXPECTED_FAILURES in helpers.js). That makes it exactly the
// case worth pinning here: the tab has to fail *legibly*.
//
// A blank panel is the failure mode that matters. The tab is the slowest in the
// app even when it works, so "nothing rendered" is indistinguishable from
// "still loading" to a reader, and an unexplained blank is how a working tab
// gets reported as broken.

test('the state tab is reachable, and says so when it cannot read state', async ({ page }) => {
  const seen = watch(page);

  await page.goto(`/realm/${HUB_ROUTE}?network=alpha`);
  await settle(page);

  const tab = page.locator('#realm-tabs .tab[data-tab="state"]');
  await expect(tab).toBeVisible();

  await tab.click();
  await expect(page).toHaveURL(/[?&]tab=state(&|$)/);
  await expect(page.locator('#tab-state')).toHaveClass(/active/);

  // Something explanatory, not an empty box.
  const panel = page.locator('#tab-state');
  await expect(panel).not.toBeEmpty();
  await expect(panel).toContainText(/No state|reading state|declares no state|select a specific network/i);

  // The RPC failure is expected here; an uncaught exception is not.
  expect(unexpected(seen.failedRequests)).toEqual([]);
  expect(seen.jsErrors).toEqual([]);
});

// A deep link straight to the tab must load it, not silently fall back to info.
// The tab is loaded on demand (see tabLoaders), which is exactly the wiring
// that forgets deep links.
test('a deep link to ?tab=state opens the state tab', async ({ page }) => {
  const seen = watch(page);

  await page.goto(`/realm/${HUB_ROUTE}?network=alpha&tab=state`);
  await settle(page);

  await expect(page.locator('#tab-state')).toHaveClass(/active/);
  await expect(page.locator('#realm-tabs .tab[data-tab="state"]')).toHaveClass(/active/);
  await expect(page.locator('#tab-info')).not.toHaveClass(/active/);

  expect(seen.jsErrors).toEqual([]);
});

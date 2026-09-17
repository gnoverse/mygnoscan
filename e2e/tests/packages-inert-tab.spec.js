import { expect, test } from '@playwright/test';

import { settle, watch } from './helpers.js';

// This harness has no RPC configured (see EXPECTED_FAILURES in helpers.js),
// so the inert queue/history fetch always fails here — same as the address
// balance fetch does. That failure is expected and out of scope for these
// tests, which are only about the tab's URL/DOM state; only uncaught
// exceptions are asserted against.

// The "inert queue" subtab used to be pure client state: switching to it
// changed nothing in the URL, so a reload — or a link sent to someone else —
// always landed back on "all packages". switchPackagesView now mirrors that
// choice into ?pv=inert, the same way dashSubnav mirrors its section.
test('switching to the inert queue tab is reflected in the URL and survives a reload', async ({ page }) => {
  const seen = watch(page);

  await page.goto('/packages');
  await settle(page);

  await page.click('#packages-subnav .tab[data-pv="inert"]');
  await settle(page);

  await expect(page).toHaveURL(/[?&]pv=inert(&|$)/);
  await expect(page.locator('#packages-view-inert')).toBeVisible();
  await expect(page.locator('#packages-view-all')).toBeHidden();
  await expect(page.locator('#packages-subnav .tab[data-pv="inert"]')).toHaveClass(/active/);

  await page.reload();
  await settle(page);

  await expect(page).toHaveURL(/[?&]pv=inert(&|$)/);
  await expect(page.locator('#packages-view-inert')).toBeVisible();
  await expect(page.locator('#packages-view-all')).toBeHidden();
  await expect(page.locator('#packages-subnav .tab[data-pv="inert"]')).toHaveClass(/active/);

  // Switching back to "all packages" removes the param rather than leaving a
  // stale ?pv=inert behind.
  await page.click('#packages-subnav .tab[data-pv="all"]');
  await settle(page);
  await expect(page).not.toHaveURL(/[?&]pv=inert(&|$)/);
  await expect(page.locator('#packages-view-all')).toBeVisible();

  expect(seen.jsErrors, 'uncaught exceptions').toEqual([]);
});

test('a deep link straight to ?pv=inert lands on the inert tab without loading the main list first', async ({ page }) => {
  const seen = watch(page);

  await page.goto('/packages?pv=inert');
  await settle(page);

  await expect(page.locator('#packages-view-inert')).toBeVisible();
  await expect(page.locator('#packages-view-all')).toBeHidden();
  await expect(page.locator('#packages-subnav .tab[data-pv="inert"]')).toHaveClass(/active/);

  expect(seen.jsErrors, 'uncaught exceptions').toEqual([]);
});

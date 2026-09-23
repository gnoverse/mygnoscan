import { expect, test } from '@playwright/test';

import { settle, unexpected, watch } from './helpers.js';

// The developer section: code search, versions, and the generated API list.
//
// The fixture harness seeds package source, so search is one of the few
// RPC-free things on this page and is genuinely exercised here.

test('the developer section is in the rail and code search runs', async ({ page }) => {
  const seen = watch(page);

  await page.goto('/developer?network=alpha');
  await settle(page);

  await expect(page.locator('#nav-developer')).toBeVisible();
  await expect(page.locator('#view-developer')).toBeVisible();

  // Search for something the fixture's source actually contains. `package` is
  // the one token every .gno file has, which makes it the safe probe: a
  // fixture-specific symbol would make this test about the fixture.
  await page.fill('#code-q', 'package');
  await page.press('#code-q', 'Enter');
  await expect(page.locator('#code-results')).toContainText(/indexed files|no match|index is empty/);

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.failedRequests)).toEqual([]);
});

// The query belongs in the URL: a search someone cannot send to a colleague is
// half a feature.
test('a code search is a shareable URL and survives a reload', async ({ page }) => {
  const seen = watch(page);

  await page.goto('/developer?network=alpha');
  await settle(page);
  await page.fill('#code-q', 'package');
  await page.press('#code-q', 'Enter');

  await expect(page).toHaveURL(/[?&]q=package(&|$)/);

  await page.reload();
  await settle(page);
  await expect(page.locator('#code-q')).toHaveValue('package');

  expect(seen.jsErrors).toEqual([]);
});

// The API reference is generated from the route table, so this doubles as a
// check that the recorder ran: an empty list means RegisterRoutes stopped
// recording and the page would silently show nothing.
test('the api page lists the routes the server actually serves', async ({ page }) => {
  const seen = watch(page);

  await page.goto('/developer/api?network=alpha');
  await settle(page);

  const content = page.locator('#devapi-content');
  await expect(content).toContainText('/api/stats');
  await expect(content).toContainText('/api/code/search');
  await expect(content).toContainText(/\d+ ENDPOINTS|\d+ endpoints/i);

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.failedRequests)).toEqual([]);
});

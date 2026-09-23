import { expect, test } from '@playwright/test';

import { APP_BUSY } from '../harness/fixture.mjs';
import { settle, unexpected, watch } from './helpers.js';

// The app hub: what is built on gno.land, as something you look at.
//
// The chain proposes and curation corrects, so what is worth asserting is the
// seams between those layers, and the two orderings the whole page is arranged
// around:
//
//   every card has a picture       -> the thing that makes it a hub, not a list
//   the website leads              -> the app first, the realm second
//   each sentence says its source  -> a curated one and a generated one differ
//   the ask is in the footer       -> show the work before asking for a favour

const card = (page, name) => page.locator('.app-card')
  .filter({ has: page.getByText(name, { exact: true }) });

test('the grid is pictures, ranked by what people use', async ({ page }) => {
  const seen = watch(page);

  const res = await page.goto('/apps?network=alpha');
  expect(res.status()).toBe(200);
  await settle(page);
  const content = page.locator('#apps-content');

  const cards = content.locator('.app-grid .app-card');
  expect(await cards.count()).toBeGreaterThan(8);

  // Every card carries something in the picture slot, a capture or the initial
  // that stands in for one. An empty box would reflow the grid around it, which
  // is the failure a reader sees before any other.
  for (const c of await cards.all()) {
    await expect(c.locator('.app-shot')).toHaveCount(1);
  }

  // The busiest realm in the fixture leads, because reach is what the ranking
  // is for. `app` and `shop` have one and two callers and must not.
  await expect(cards.first().locator('.app-name')).toHaveText('gno.land blog');

  expect(seen.jsErrors, 'uncaught exceptions').toEqual([]);
  expect(unexpected(seen.consoleErrors), 'console errors').toEqual([]);
});

// The ordering the page is arranged around. Somebody sent here wants the thing
// itself; the realm page is what they want next, and only if they are the kind
// of person who wants it.
test('the website is the first link and the realm the second', async ({ page }) => {
  await page.goto('/apps?network=alpha');
  await settle(page);

  const kourt = card(page, 'Kourt');
  const links = kourt.locator('.app-actions a, .app-actions .app-launch');
  await expect(links.first()).toHaveAttribute('href', /kourt\.xyz/);
  await expect(kourt.locator('.app-actions')).toContainText('realm');

  // A realm with no website opens the realm instead, rather than showing a
  // dead "open" that goes nowhere.
  const blog = card(page, 'gno.land blog');
  await expect(blog.locator('.app-actions')).toContainText('open realm');
});

// A wallet and a chess server are not realms and never will be, so an indexer
// is structurally blind to them. A hub that could not show them would be
// answering the question with the subset it happens to be able to index.
test('apps that are not on a chain are in the same grid', async ({ page }) => {
  await page.goto('/apps?network=alpha');
  await settle(page);

  const adena = card(page, 'Adena Wallet');
  await expect(adena).toBeVisible();
  await expect(adena).toContainText('off chain');
  await expect(adena.locator('.app-launch')).toHaveAttribute('href', /adena\.app/);
  // Nothing on a chain to count, so it claims nothing about one.
  await expect(adena).not.toContainText('calls');
});

// Three different claims, and a reader who cannot tell them apart has to trust
// all of them equally or none.
test('a description says where it came from', async ({ page }) => {
  await page.goto('/apps?network=alpha');
  await settle(page);
  const content = page.locator('#apps-content');

  // Curated: somebody wrote this sentence in a merged pull request.
  await expect(card(page, 'gno.land blog').locator('.app-prov'))
    .toHaveAttribute('title', /this explorer.s own registry/);
  // Community: awesome-gno wrote it.
  await expect(card(page, 'Adena Wallet').locator('.app-prov'))
    .toHaveAttribute('title', /the community/);
  // And a realm nobody has described says so, rather than showing a blank.
  await expect(content).toContainText('no description yet');
});

// Moved to the footer on purpose. Asking a visitor for a pull request before
// they have seen anything is asking a favour of someone who does not yet know
// what this is.
test('the ask is at the bottom, and the skip list is readable', async ({ page }) => {
  await page.goto('/apps?network=alpha');
  await settle(page);
  const content = page.locator('#apps-content');

  const foot = content.locator('.app-foot');
  await expect(foot).toContainText('something missing, wrong, or here that should not be?');
  await expect(foot.getByRole('link', { name: 'awesome-gno' })).toBeVisible();
  await expect(foot.getByRole('link', { name: 'moderation.toml' })).toBeVisible();

  // Below the grid, not above it: the last card must come before the ask.
  const gridBox = await content.locator('.app-grid').boundingBox();
  const footBox = await foot.boundingBox();
  expect(footBox.y).toBeGreaterThan(gridBox.y + gridBox.height - 1);

  // And it says how the list was built, so the ranking is not a black box.
  await expect(foot).toContainText(/found on chain/);
  await expect(foot).toContainText(/realms deployed/);
});

test('the window is in the URL and survives a reload', async ({ page }) => {
  await page.goto('/apps?network=alpha');
  await settle(page);
  const content = page.locator('#apps-content');

  await content.getByRole('button', { name: '7d', exact: true }).click();
  await expect(page).toHaveURL(/[?&]window=7d/);
  await settle(page);
  await page.reload();
  await settle(page);
  await expect(content.locator('.dash-seg button.on').filter({ hasText: '7d' })).toBeVisible();
});

// The endpoint apart from the page.
test('the endpoint reports how the list was assembled', async ({ request }) => {
  const res = await request.get('/api/apps?network=alpha');
  expect(res.status()).toBe(200);
  const body = await res.json();

  expect(body.apps.length).toBeGreaterThan(8);
  expect(body.discovered).toBeGreaterThan(0);
  expect(body.off_chain).toBeGreaterThan(0);
  expect(Array.isArray(body.moderation)).toBe(true);

  const busy = body.apps.find(a => a.path === APP_BUSY);
  expect(busy, 'the busiest fixture realm is missing').toBeTruthy();
  expect(busy.name_from).toBe('curated');
  expect(busy.callers_window).toBeGreaterThan(0);

  // Ranked, and the order the page draws is the order the server sent.
  const scored = body.apps.filter(a => a.via === 'discovered').map(a => a.score);
  for (let i = 1; i < scored.length; i++) {
    expect(scored[i]).toBeLessThanOrEqual(scored[i - 1]);
  }
});

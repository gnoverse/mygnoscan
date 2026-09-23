import { expect, test } from '@playwright/test';

import { APP_BUSY, APP_ELSEWHERE, APP_QUIET, FRESH_REALM } from '../harness/fixture.mjs';
import { settle, unexpected, watch } from './helpers.js';

// The app hub: the directory as something you look at.
//
// The assertions worth having are not "a grid rendered". They are the three
// states a card can be in, which are three different facts about an app and
// are the way this page would most easily start lying:
//
//   deployed and used   -> numbers
//   deployed, no calls  -> "deployed, never called"
//   not on this chain   -> "not deployed on <chain>"
//
// Collapsing any two of those into one is worse than showing nothing, because
// a directory is read by people who cannot check it against the chain.

// By the name element rather than by the card's text: several blurbs mention
// another app by name, so `hasText` matches three cards for "GovDAO".
const card = (page, name) => page.locator('.app-card')
  .filter({ has: page.getByText(name, { exact: true }) });

test('a card says which of the three states an app is in', async ({ page }) => {
  const seen = watch(page);

  const response = await page.goto('/apps?network=alpha');
  expect(response.status()).toBe(200);
  await settle(page);

  const content = page.locator('#apps-content');
  await expect(content.locator('.app-grid')).toBeVisible();

  // Deployed and called by six different people in the fixture.
  const busy = card(page, 'gno.land blog');
  await expect(busy).toBeVisible();
  await expect(busy.locator('.app-stats')).toContainText('6 calls');
  await expect(busy.locator('.app-stats')).toContainText('6 people');

  // Deployed here, never called. Not the same as absent, and not a zero.
  await expect(card(page, 'wugnot').locator('.app-stats'))
    .toContainText('deployed, never called');

  // Listed in the directory, not deployed on this chain. The description is
  // still there: what an app is for is true on every chain.
  const elsewhere = card(page, 'GovDAO');
  await expect(elsewhere.locator('.app-stats')).toContainText('not deployed on alpha');
  await expect(elsewhere.locator('.app-desc')).toContainText('proposals, votes');

  expect(seen.jsErrors, 'uncaught exceptions').toEqual([]);
  expect(unexpected(seen.consoleErrors), 'console errors').toEqual([]);
});

// Without a chain there is nothing honest to say about usage, because the same
// path is a different deployment on each one. The descriptions must survive
// that: someone who came to find out what Boards2 is should not have to pick a
// chain first.
test('with no network selected the blurbs stay and the numbers go', async ({ page }) => {
  const seen = watch(page);
  await page.goto('/apps');
  await settle(page);

  const content = page.locator('#apps-content');
  await expect(content).toContainText('Boards2');
  await expect(content).not.toContainText('never called');
  await expect(content).not.toContainText('not deployed on');

  // Said once, about the page, rather than stamped on all ten cards: the
  // reason is the same for every one of them and it is not a fact about any
  // app. The cards carry no usage line at all here.
  await expect(content).toContainText('usage is per chain');
  await expect(content.locator('.app-card .app-stats')).toHaveCount(0);

  // And it is one click, not an instruction to go and find the selector. The
  // harness configures no mainnet, so the offer names the first chain.
  await content.getByText('see which of these are live on alpha').click();
  await settle(page);
  await expect(page.locator('#network-select')).toHaveValue('alpha');
  await expect(content.locator('.app-card .app-stats').first()).toContainText('calls');

  expect(seen.jsErrors, 'uncaught exceptions').toEqual([]);
});

test('the category filter and the view toggle are in the URL', async ({ page }) => {
  await page.goto('/apps?network=alpha');
  await settle(page);

  const content = page.locator('#apps-content');
  await content.getByRole('button', { name: 'governance', exact: true }).click();
  await expect(page).toHaveURL(/[?&]cat=governance/);
  await expect(content.locator('.app-card .app-name')).toHaveText(['GovDAO', 'Params']);

  // A filtered view is a link, so a reload has to land on the same page. This
  // is the site's settled convention and the reason it exists: the way this
  // page gets shared is someone pasting their filter combination.
  await page.reload();
  await settle(page);
  await expect(content.locator('.app-card .app-name')).toHaveText(['GovDAO', 'Params']);

  await content.getByRole('button', { name: 'table', exact: true }).click();
  await expect(page).toHaveURL(/[?&]view=table/);
  await expect(content.locator('.app-card')).toHaveCount(0);
  // `.first()`: the candidates block below is a table too, and the filter is
  // deliberately not applied to it.
  const dense = content.locator('table').first();
  await expect(dense).toBeVisible();
  await expect(dense).toContainText('GovDAO');
  await expect(dense).not.toContainText('wugnot');
});

// The point of the candidates block: the directory is curated and therefore
// always behind the chain, and a page that hides that reads as "these are all
// the apps there are".
test('the busiest realm nobody described is offered, and never auto-listed', async ({ page }) => {
  const seen = watch(page);
  await page.goto('/apps?network=alpha');
  await settle(page);

  const content = page.locator('#apps-content');
  await expect(content).toContainText('busy, and not described yet');

  // The busiest realm inside the default window that no registry entry
  // mentions. Ranked by calls in that window, not all time: a realm that was
  // busy last year is not what a directory is missing.
  const row = content.locator('tr', { hasText: FRESH_REALM });
  await expect(row).toBeVisible();
  // Offered as a suggestion, not promoted: it has no card.
  await expect(card(page, FRESH_REALM)).toHaveCount(0);

  // And it links to the realm, so acting on the suggestion starts by looking
  // at the thing.
  await row.getByText(FRESH_REALM, { exact: true }).click();
  await expect(page).toHaveURL(/\/realm\/r\/fresh\/app/);

  expect(seen.jsErrors, 'uncaught exceptions').toEqual([]);
});

// The seeded paths have to be the registry's own, or the fixture is testing
// nothing: an unlisted path produces no card at all. This fails loudly if
// someone edits apps.json and drops one of them.
test('the fixture seeds paths the registry actually lists', async ({ request }) => {
  const res = await request.get('/api/registry/apps?network=alpha');
  expect(res.status()).toBe(200);
  const body = await res.json();
  const listed = new Set(body.apps.map(a => a.path));
  for (const path of [APP_BUSY, APP_QUIET, APP_ELSEWHERE]) {
    expect(listed, `${path} must be in pkg/registry/data/apps.json`).toContain(path);
  }
  expect(body.stats[APP_BUSY].deployed).toBe(true);
  expect(body.stats[APP_QUIET].calls).toBe(0);
  expect(body.stats[APP_ELSEWHERE]).toBeUndefined();
});

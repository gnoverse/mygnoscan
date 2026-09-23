import { expect, test } from '@playwright/test';

import { APP_BUSY } from '../harness/fixture.mjs';
import { settle, unexpected, watch } from './helpers.js';

// The ecosystem view: the community's list, and the gap between it and ours.
//
// What is worth asserting here is not "a list rendered". It is the three things
// that make this more than a mirror of somebody else's README, each of which is
// a way the page would quietly stop being true:
//
//   off-chain entries are present   -> the half an indexer cannot see
//   a realm entry joins to the chain -> the two lists are actually connected
//   the gap is shown, both ways      -> the part a reader can act on
//
// Plus the provenance line, because a snapshot that reads as live is the one
// way this section could mislead without anything being visibly wrong.

test('the view is in the URL and survives a reload', async ({ page }) => {
  const seen = watch(page);

  await page.goto('/apps?network=alpha');
  await settle(page);
  const content = page.locator('#apps-content');

  await content.getByRole('button', { name: 'ecosystem', exact: true }).click();
  await expect(page).toHaveURL(/[?&]view=ecosystem/);
  await settle(page);
  await expect(content.locator('.eco-grid').first()).toBeVisible();

  // A view is a link, the site's settled convention: the way a page like this
  // gets shared is somebody pasting the URL they were looking at.
  await page.reload();
  await settle(page);
  await expect(content.locator('.eco-grid').first()).toBeVisible();
  await expect(content.locator('.app-card')).toHaveCount(0);

  expect(seen.jsErrors, 'uncaught exceptions').toEqual([]);
  expect(unexpected(seen.consoleErrors), 'console errors').toEqual([]);
});

// The reason this view exists. A wallet, an editor extension and a language
// server are not realms, so no amount of indexing will ever surface them, and
// a directory that answers "what is built on gno.land" with only the realms it
// can index gives a confidently incomplete answer.
test('it carries the projects that are not realms at all', async ({ page }) => {
  await page.goto('/apps?network=alpha&view=ecosystem');
  await settle(page);
  const content = page.locator('#apps-content');

  await expect(content).toContainText('Adena Wallet');
  await expect(content).toContainText('Gno Extension for VS Code');
  await expect(content).toContainText('Tools');

  // None of those has a realm path, so none of them claims a chain state.
  const adena = content.locator('.eco-item').filter({ hasText: 'Adena Wallet' });
  await expect(adena).not.toContainText('on chain');
  await expect(adena).not.toContainText('not deployed');
});

// The join. An entry whose front end lives off chain still names its realm in
// an inline link, and lifting that out is what connects the community's list to
// an index at all.
test('an entry that names a realm links to it and says what the chain knows', async ({ page }) => {
  await page.goto('/apps?network=alpha&view=ecosystem');
  await settle(page);
  const content = page.locator('#apps-content');

  const kourt = content.locator('.eco-item').filter({ hasText: 'Kourt' }).first();
  await expect(kourt).toContainText('on chain');
  // Not seeded on the fixture chain, and that is a real answer rather than a
  // zero: "not deployed here" and "deployed and unused" are different facts.
  await expect(kourt).toContainText('not deployed on alpha');

  await kourt.getByText(/^on chain/).click();
  await expect(page).toHaveURL(/\/realm\/r\//);
});

// The loop closed. Gnoswap is on both lists: named by its GitHub repo on
// theirs, by a realm path on ours, and matched on nothing but the name. Drawing
// that link is what stops this page reading as two directories printed one
// after the other.
test('a project on both lists is linked across', async ({ page }) => {
  await page.goto('/apps?network=alpha&view=ecosystem');
  await settle(page);
  const content = page.locator('#apps-content');

  const gnoswap = content.locator('.eco-item').filter({ hasText: 'Gnoswap' }).first();
  await expect(gnoswap).toContainText('described here');
  await gnoswap.getByText(/^described here/).click();
  await expect(page).toHaveURL(/\/realm\/r\/gnoswap\/router/);
});

// A snapshot that reads as live is the failure that leaves no trace: the page
// looks right, and is a month behind.
test('it says it is a snapshot, dated, at a commit', async ({ page }) => {
  await page.goto('/apps?view=ecosystem');
  await settle(page);
  const content = page.locator('#apps-content');

  await expect(content).toContainText('a snapshot, not a live read');
  await expect(content).toContainText(/read from [0-9a-f]{7} on \d{4}-\d{2}-\d{2}/);
  const commit = content.locator('a[href*="/commit/"]').first();
  await expect(commit).toHaveAttribute('href', /gnoverse\/awesome-gno\/commit\/[0-9a-f]{40}/);
});

// The archive is the community's own judgement that something is finished. A
// grid that mixed a retired testnet in with a live app would be giving a reader
// no way to tell, and the page is the only thing that could have told them.
test('retired entries are marked and sorted last', async ({ page }) => {
  await page.goto('/apps?view=ecosystem');
  await settle(page);
  const content = page.locator('#apps-content');

  const titles = await content.locator('.eco-sec .section-title').allTextContents();
  expect(titles.length).toBeGreaterThan(3);
  expect(titles[titles.length - 1]).toContain('Archive');
  expect(titles[titles.length - 1]).toContain('archived');
  await expect(content.locator('.eco-sec').last().locator('.eco-off').first()).toBeVisible();
});

// The invitation, and the only part of this page that grows somebody else's
// directory rather than ours. "Contribute to awesome-gno" is a sentence nobody
// acts on; a ranked list with the line to paste and the button that opens the
// editor is a task.
test('the gap is shown both ways, with the line to paste', async ({ page }) => {
  const seen = watch(page);
  await page.goto('/apps?network=alpha&view=ecosystem');
  await settle(page);
  const content = page.locator('#apps-content');

  await expect(content).toContainText('described here, not on the community list');

  // APP_BUSY is in the directory, is not named on the community list, and is
  // the busiest such entry on the fixture chain, so it ranks first.
  const row = content.locator('tr', { hasText: 'gno.land blog' });
  await expect(row).toBeVisible();
  await expect(row).toContainText('6 / 6');
  // The suggested line is awesome-gno's own bullet shape, one sentence, ready
  // to paste into the editor the link beside it opens.
  await expect(row.locator('.eco-bullet')).toContainText('- [gno.land blog](https://gno.land/r/gnoland/blog) - ');
  await expect(row.locator('a[href*="awesome-gno/edit"]')).toBeVisible();

  // Both directions: the community names realms this directory has not
  // described either, and that is the cheaper gap to close.
  await expect(content).toContainText('on the community list, not described here');

  // And the standing call to action, which is two links and not a plea.
  await expect(content.getByRole('link', { name: /edit README/ })).toBeVisible();
  await expect(content.getByRole('link', { name: /contributing guide/ })).toBeVisible();

  expect(seen.jsErrors, 'uncaught exceptions').toEqual([]);
  expect(unexpected(seen.consoleErrors), 'console errors').toEqual([]);
});

// Someone who lands on /apps and cannot find the wallet they came for must be
// told where it is, on the page they landed on. Behind the tab is too late:
// they have already concluded the ecosystem is ten apps wide.
test('the directory itself points at the community list', async ({ page }) => {
  await page.goto('/apps?network=alpha');
  await settle(page);
  const content = page.locator('#apps-content');

  const jump = content.getByText(/projects on awesome-gno/);
  await expect(jump).toBeVisible();
  await jump.click();
  await expect(page).toHaveURL(/[?&]view=ecosystem/);
  await settle(page);
  await expect(content.locator('.eco-grid').first()).toBeVisible();
});

// The endpoint, checked apart from the page: the two lists overlap in a handful
// of entries out of eighty, so a zero on either side of the cross-check means
// the matching broke rather than that the lists agree.
test('the endpoint cross-checks in both directions', async ({ request }) => {
  const res = await request.get('/api/registry/awesome?network=alpha');
  expect(res.status()).toBe(200);
  const body = await res.json();

  expect(body.entries).toBeGreaterThan(40);
  expect(body.commit).toMatch(/^[0-9a-f]{40}$/);
  expect(body.missing_from_awesome.length).toBeGreaterThan(0);
  expect(Object.keys(body.in_directory).length).toBeGreaterThan(0);

  const missing = body.missing_from_awesome.map(a => a.path);
  expect(missing).toContain(APP_BUSY);
  // A path cannot be matched and missing at once; a page built on both would
  // print the same realm under "listed" and "not listed".
  for (const path of missing) expect(body.in_directory[path]).toBeUndefined();
  expect(body.stats[APP_BUSY].calls).toBe(6);
});

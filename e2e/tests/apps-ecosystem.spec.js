import { expect, test } from '@playwright/test';

import { APP_BUSY } from '../harness/fixture.mjs';
import { settle, unexpected, watch } from './helpers.js';

// The ecosystem view: what is built on gno.land and is not a realm.
//
// The question this page exists to answer is "show me the apps", and the
// assertions worth having are the ways it would answer it wrongly rather than
// visibly break:
//
//   a picture per app        -> the thing that makes it a hub and not a bibliography
//   only things with a page  -> an SDK in a grid of pictures is a grey hole
//   a verified address       -> a confident screenshot of a dead host is worse than none
//   a gated screenshot proxy -> an unbounded ?url= is an open proxy
//
// The fixture's shot stub stands in for gnoshot, so these run without a browser
// farm; what it cannot check is how the real captures look, which is what
// `make screenshots` is for.

const card = (page, name) => page.locator('.app-card')
  .filter({ has: page.getByText(name, { exact: true }) });

test('the view is in the URL and survives a reload', async ({ page }) => {
  const seen = watch(page);

  await page.goto('/apps?network=alpha');
  await settle(page);
  const content = page.locator('#apps-content');

  await content.getByRole('button', { name: 'ecosystem', exact: true }).click();
  await expect(page).toHaveURL(/[?&]view=ecosystem/);
  await settle(page);
  await expect(content.locator('.app-grid')).toBeVisible();

  await page.reload();
  await settle(page);
  await expect(content.locator('.app-grid')).toBeVisible();

  expect(seen.jsErrors, 'uncaught exceptions').toEqual([]);
  expect(unexpected(seen.consoleErrors), 'console errors').toEqual([]);
});

// The reason this view exists, and the reason it is a grid of pictures. A
// wallet, a DEX and a playground are not realms, so no amount of indexing will
// ever surface them, and an explorer that answers "what is built here" out of
// its own index gives a confidently incomplete answer.
test('every app is a card with a picture and a way in', async ({ page }) => {
  await page.goto('/apps?network=alpha&view=ecosystem');
  await settle(page);
  const content = page.locator('#apps-content');

  const cards = content.locator('.app-grid .app-card');
  expect(await cards.count()).toBeGreaterThan(8);

  // Every card carries something in the picture slot: a capture, or the
  // initial that stands in for one. A card with an empty box would reflow the
  // grid around it, which is the failure a reader sees before any other.
  for (const c of await cards.all()) {
    await expect(c.locator('.app-shot')).toHaveCount(1);
  }

  const adena = card(page, 'Adena Wallet');
  await expect(adena).toBeVisible();
  await expect(adena.locator('.app-launch')).toHaveAttribute('href', /adena\.app/);
  // Off chain, so it claims nothing about a chain.
  await expect(adena).not.toContainText('not deployed');
  await expect(adena.locator('.app-cat')).toHaveCount(0);
});

// A realm and an off-chain app are both apps and must not be told apart by
// accident, but the one that is on a chain says so and links there.
test('a realm in the list is marked and links to its page', async ({ page }) => {
  await page.goto('/apps?network=alpha&view=ecosystem');
  await settle(page);
  const content = page.locator('#apps-content');

  const kourt = content.locator('.app-card').filter({ hasText: 'Kourt' }).first();
  await expect(kourt.locator('.app-cat')).toHaveText('on chain');
  await kourt.getByText('realm', { exact: true }).click();
  await expect(page).toHaveURL(/\/realm\/r\//);
});

// An SDK, a language server and a conference talk have no screenshot. Drawing
// them was the previous version of this page and it buried the apps under six
// screens of link list.
test('what has nothing to photograph is counted, not drawn', async ({ page }) => {
  await page.goto('/apps?view=ecosystem');
  await settle(page);
  const content = page.locator('#apps-content');

  await expect(content).toContainText(/more entries have nothing to photograph/);
  // Named and counted, so a reader knows what they are not being shown and how
  // much of it there is.
  await expect(content).toContainText(/sdks & clients \d+/);
  // And not rendered: these have no card.
  await expect(card(page, 'tm2-js-client')).toHaveCount(0);
  await expect(card(page, 'gnopls')).toHaveCount(0);
});

// A snapshot that reads as live is the failure that leaves no trace: the page
// looks right and is a month behind.
test('it says it is a snapshot, dated, at a commit', async ({ page }) => {
  await page.goto('/apps?view=ecosystem');
  await settle(page);
  const content = page.locator('#apps-content');

  await expect(content).toContainText('a snapshot, not a live read');
  await expect(content).toContainText(/read from [0-9a-f]{7} on \d{4}-\d{2}-\d{2}/);
  await expect(content.locator('a[href*="/commit/"]').first())
    .toHaveAttribute('href', /gnoverse\/awesome-gno\/commit\/[0-9a-f]{40}/);
});

// The invitation, and the only part of this page that grows somebody else's
// directory rather than ours.
test('the gap is shown both ways, with the line to paste', async ({ page }) => {
  const seen = watch(page);
  await page.goto('/apps?network=alpha&view=ecosystem');
  await settle(page);
  const content = page.locator('#apps-content');

  await expect(content).toContainText('described here, not on the community list');
  const row = content.locator('tr', { hasText: 'gno.land blog' });
  await expect(row).toContainText('6 / 6');
  await expect(row.locator('.eco-bullet'))
    .toContainText('- [gno.land blog](https://gno.land/r/gnoland/blog) - ');
  await expect(row.locator('a[href*="awesome-gno/edit"]')).toBeVisible();

  await expect(content).toContainText('on the community list, not described here');
  await expect(content.getByRole('link', { name: /edit README/ })).toBeVisible();

  expect(seen.jsErrors, 'uncaught exceptions').toEqual([]);
  expect(unexpected(seen.consoleErrors), 'console errors').toEqual([]);
});

test('the directory itself points at the community list', async ({ page }) => {
  await page.goto('/apps?network=alpha');
  await settle(page);
  const content = page.locator('#apps-content');

  const jump = content.getByText(/projects on awesome-gno/);
  await jump.click();
  await expect(page).toHaveURL(/[?&]view=ecosystem/);
  await settle(page);
  await expect(content.locator('.app-grid')).toBeVisible();
});

// The gate, and the reason this endpoint can exist at all. An unbounded ?url=
// in front of a capture service is an open proxy and a way to spend someone
// else's CPU on headless Chrome, so the check is against the exact strings a
// human merged, not against their hosts.
test('the screenshot proxy serves the listed sites and nothing else', async ({ request }) => {
  const listed = await (await request.get('/api/registry/awesome')).json();
  const site = listed.apps.find(a => a.site).site;

  const ok = await request.get('/api/shot/site?url=' + encodeURIComponent(site) + '&size=thumb');
  expect(ok.status()).toBe(200);

  for (const [name, url] of [
    ['a host nobody listed', 'https://evil.example/'],
    // The same host as a listed app, a different page. A host-level check would
    // wave this through, which is the whole reason the gate is exact.
    ['another page on a listed host', new URL('/something-else', site).toString()],
    ['a file url', 'file:///etc/passwd'],
    ['nothing at all', ''],
  ]) {
    const res = await request.get('/api/shot/site?url=' + encodeURIComponent(url));
    expect(res.status(), name).toBe(400);
  }
});

// The endpoint apart from the page: the two lists overlap in a handful of
// entries out of eighty, so a zero on either side of the cross-check means the
// matching broke rather than that the lists agree.
test('the endpoint resolves apps and cross-checks both ways', async ({ request }) => {
  const res = await request.get('/api/registry/awesome?network=alpha');
  expect(res.status()).toBe(200);
  const body = await res.json();

  expect(body.entries).toBeGreaterThan(40);
  expect(body.commit).toMatch(/^[0-9a-f]{40}$/);
  expect(body.apps.length).toBeGreaterThan(8);
  expect(body.others.length).toBeGreaterThan(0);

  for (const a of body.apps) {
    // An app is exactly a thing with somewhere to go. Anything else in here is
    // a card the page cannot draw.
    expect(Boolean(a.site || a.path), `${a.name} is an app with no page`).toBe(true);
    if (a.site) {
      expect(a.site, `${a.name}`).toMatch(/^https?:\/\//);
      // Where the address came from is not the same claim as the address, and
      // the card labels the difference.
      expect(['listed', 'repo-homepage']).toContain(a.site_from);
    }
  }

  expect(body.missing_from_awesome.map(a => a.path)).toContain(APP_BUSY);
  expect(Object.keys(body.in_directory).length).toBeGreaterThan(0);
  for (const p of body.missing_from_awesome.map(a => a.path)) {
    expect(body.in_directory[p]).toBeUndefined();
  }
});

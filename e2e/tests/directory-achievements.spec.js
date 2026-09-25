// The directory section and the achievement catalog behind it.
//
// Nothing here is checkable from Go: the catalog is served as JSON and the
// whole feature is what the frontend does with it. The two failures worth
// catching are a badge grid that renders empty because the rollup never ran,
// and a filter that narrows nothing because the chip writes a parameter the
// server does not read.
//
// The badge table is rebuilt on a timer, and the harness starts the binary with
// `-achievement-interval 1s` because the fixture is seeded after startup. Every
// test that needs a badge therefore waits for the rebuild rather than assuming
// it has happened: without that this file is a coin flip on a loaded machine.
import { expect, test } from '@playwright/test';

import { BUSY_CALLER } from '../harness/fixture.mjs';
import { settle, unexpected, watch } from './helpers.js';

// waitForBadges polls the API until the rollup has produced something, and
// fails loudly rather than letting an empty grid read as a rendering bug.
async function waitForBadges(page) {
  await expect.poll(async () => {
    const res = await page.request.get('/api/achievements');
    if (!res.ok()) return 0;
    const body = await res.json();
    return (body.achievements || []).reduce((n, a) => n + (a.holders || 0), 0);
  }, {
    message: 'the achievement rollup never awarded a badge; is -achievement-interval set on the harness?',
    timeout: 20000,
  }).toBeGreaterThan(0);
}

test('the directory lists the whole catalog, grouped', async ({ page }) => {
  const seen = watch(page);
  await waitForBadges(page);
  await page.goto('/directory');
  await settle(page);

  const cards = page.locator('#directory-content .ach');
  await expect(cards.first()).toBeVisible();
  // Every group heading the catalog defines has to produce a grid; a badge in
  // a group nothing renders is invisible and nothing else would say so.
  const grids = await page.locator('#directory-content .ach-grid').count();
  expect(grids).toBeGreaterThan(1);
  expect(await cards.count()).toBeGreaterThan(10);

  // The `how` line is the reason the catalog exists. A card without one is a
  // trophy, which is the thing this feature is deliberately not.
  await expect(page.locator('#directory-content .ach-how').first()).toBeVisible();

  expect(unexpected(seen.consoleErrors)).toEqual([]);
});

test('a badge on the directory links to the people who hold it', async ({ page }) => {
  await waitForBadges(page);
  await page.goto('/directory');
  await settle(page);

  await page.locator('#directory-content .ach a:text-is("who has it →")').first().click();
  await settle(page);

  await expect(page).toHaveURL(/\/directory\/people\?has=[a-z-]+/);
  await expect(page.locator('#view-people')).toHaveClass(/active/);
  // The chip for the badge that was clicked comes back lit, so the reader can
  // see and undo what narrowed the list.
  await expect(page.locator('#people-content .ach-chip.on')).toHaveCount(1);
});

test('the people list narrows when a badge chip is toggled', async ({ page }) => {
  const seen = watch(page);
  await waitForBadges(page);
  await page.goto('/directory/people');
  await settle(page);

  const rows = page.locator('#people-content table tbody tr');
  const before = await rows.count();
  expect(before).toBeGreaterThan(0);

  // first-realm is the narrowest badge the fixture produces that more than
  // nobody holds: everybody who signed anything has first-tx.
  const chip = page.locator('#people-content .ach-chip', { hasText: 'Published a realm' });
  await chip.click();
  await settle(page);

  await expect(page).toHaveURL(/has=first-realm/);
  const after = await rows.count();
  expect(after, 'filtering on a badge did not narrow the list').toBeLessThan(before);
  expect(after).toBeGreaterThan(0);

  // And it is undoable from the page rather than only from the URL.
  await page.locator('#people-content .ach-chip', { hasText: 'clear 1 filter' }).click();
  await settle(page);
  await expect(page).not.toHaveURL(/has=/);
  expect(unexpected(seen.consoleErrors)).toEqual([]);
});

test('a filtered people list is a link somebody else can open', async ({ page }) => {
  await waitForBadges(page);
  await page.goto('/directory/people?has=first-realm&sort=oldest');
  await settle(page);

  await expect(page.locator('#people-content .ach-chip.on')).toHaveCount(1);
  await expect(page.locator('#people-content select')).toHaveValue('oldest');
  await expect(page.locator('#people-content table tbody tr').first()).toBeVisible();
});

test('teams says what it is waiting on, not just that it is coming', async ({ page }) => {
  const seen = watch(page);
  await page.goto('/directory/teams');
  await settle(page);

  await expect(page.locator('#teams-content')).toContainText('coming soon');
  await expect(page.locator('#teams-content .card li')).not.toHaveCount(0);
  expect(unexpected(seen.consoleErrors)).toEqual([]);
});

test('an address page has an achievements tab with earned and locked badges', async ({ page }) => {
  const seen = watch(page);
  await waitForBadges(page);
  await page.goto(`/address/${BUSY_CALLER}`);
  await settle(page);

  // The activity pane is what this page has always shown, and it stays first.
  await expect(page.locator('#address-detail-content .tabs .tab.active')).toHaveText(/activity/);

  await page.locator('#address-detail-content .tab', { hasText: 'achievements' }).click();
  await settle(page);

  await expect(page).toHaveURL(/tab=achievements/);
  const grid = page.locator('#address-detail-content .ach-grid');
  await expect(grid.first()).toBeVisible();
  // Both halves: a busy caller has earned some, and nobody has earned all of
  // them, so the locked half has to be drawn rather than filtered out.
  await expect(page.locator('#address-detail-content .ach:not(.ach-locked)').first()).toBeVisible();
  await expect(page.locator('#address-detail-content .ach-locked').first()).toBeVisible();
  // A badge that cannot be clicked through to its transaction is a claim.
  await expect(page.locator('#address-detail-content .ach .ach-got').first()).toBeVisible();

  expect(unexpected(seen.consoleErrors)).toEqual([]);
});

// The split put everything below the stats bar into an activity pane, and the
// two things that are passed the container rather than appending to it were
// easy to miss: the per-hour chart and the delegated-keys table. The chart
// rendered *under* the achievements grid, which is exactly what a tab is for
// preventing; the keys table is identity and has to be visible from both.
test('the activity chart stays in the activity pane', async ({ page }) => {
  await waitForBadges(page);
  await page.goto(`/address/${BUSY_CALLER}`);
  await settle(page);
  const chart = page.locator('#address-detail-content canvas');
  await expect(chart.first()).toBeVisible();

  await page.locator('#address-detail-content .tab', { hasText: 'achievements' }).click();
  await settle(page);
  await expect(page.locator('#address-detail-content .ach-grid').first()).toBeVisible();
  await expect(chart.first(), 'the activity chart leaked into the achievements tab').toBeHidden();
});

test('the achievements tab survives a reload, and the activity pane comes back', async ({ page }) => {
  await waitForBadges(page);
  await page.goto(`/address/${BUSY_CALLER}?tab=achievements`);
  await settle(page);
  await expect(page.locator('#address-detail-content .ach-grid').first()).toBeVisible();

  await page.locator('#address-detail-content .tab', { hasText: 'activity' }).click();
  await settle(page);
  await expect(page).not.toHaveURL(/tab=/);
  await expect(page.locator('#address-detail-content table').first()).toBeVisible();
});

test('the unknown-badge case is a 400, not an empty list', async ({ page }) => {
  // An unknown slug matching nothing would read as "nobody has done this",
  // which is a different and wrong answer from "there is no such badge".
  const res = await page.request.get('/api/directory/people?has=not-a-badge');
  expect(res.status()).toBe(400);
});

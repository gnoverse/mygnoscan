import { expect, test } from '@playwright/test';

import { settle, unexpected, watch } from './helpers.js';

// The rail now has two levels, and the second one is described twice: once
// as static HTML in the rail, once as the NAV table in the script.
// TestRailMatchesNavTable keeps the two lists identical; what it cannot
// check is that the *behaviour* hanging off them works, which is all of
// this file.

// Landing on a child page has to say, in the rail, which section you are in.
// Without it a reader who deep-links to /storage sees a rail with the gas
// section folded exactly as it was and nothing marking where they are.
test('a child page highlights itself and its section', async ({ page }) => {
  await page.goto('/storage');
  await settle(page);

  await expect(page.locator('#nav-storage')).toHaveClass(/active/);
  await expect(page.locator('#nav-gas')).toHaveClass(/in-section/);
  // And nothing else is claiming to be the current page.
  await expect(page.locator('.rail nav a.active')).toHaveCount(1);
});

// validators used to be a top-level rail entry, one row below govdao. It is
// a child of blocks now, which is what puts it in the blocks strip and
// lights blocks behind it instead of nothing.
test('validators is a child of the blocks section', async ({ page }) => {
  await page.goto('/validators');
  await settle(page);

  await expect(page.locator('#nav-validators')).toHaveClass(/active/);
  await expect(page.locator('.nav-kids', { has: page.locator('#nav-validators') }))
    .toHaveCount(1);
  await expect(page.locator('#nav-blocks')).toHaveClass(/in-section/);
  await expect(page.locator('.rail nav a.active')).toHaveCount(1);
});

// The section strip is the only way to the children when the rail is
// collapsed, so it has to be on every page of a section — including the
// eight whose markup is static HTML and whose loaders never touch <main>.
test('every page in a section carries the section strip', async ({ page }) => {
  for (const [path, active, siblings] of [
    ['/gas', 'summary', ['by realm', 'by user', 'top txs', 'storage']],
    ['/storage', 'storage', ['summary', 'by realm']],
    ['/govdao', 'overview', ['proposals', 'voters', 'options', 'params']],
    ['/packages', 'packages', ['map', 'realms', 'apps']],
    ['/txs', 'txs', ['blocks', 'events', 'validators', 'sanity']],
    ['/validators', 'validators', ['blocks', 'txs', 'events', 'sanity']],
    ['/sanity', 'sanity', ['blocks', 'txs', 'events', 'validators']],
    ['/defi', 'overview', ['accounts', 'coins', 'grc20']],
    ['/accounts', 'accounts', ['overview', 'coins', 'grc20']],
    ['/', 'overview', ['analytics', 'dashboards']],
  ]) {
    await page.goto(path);
    await settle(page);
    const bar = page.locator('.view.active main > .pagenav');
    await expect(bar, `${path} has no section strip`).toBeVisible();
    await expect(bar.locator('a.active')).toHaveText(active);
    for (const s of siblings) {
      await expect(bar.locator(`a:text-is("${s}")`), `${path} strip is missing ${s}`).toHaveCount(1);
    }
  }
});

// The strip is built by route(), not by a loader, so a second navigation
// into the same view must not leave two of them.
test('navigating within a section leaves exactly one strip', async ({ page }) => {
  await page.goto('/gas');
  await settle(page);
  await page.locator('.view.active main > .pagenav a:text-is("by realm")').click();
  await settle(page);
  await expect(page).toHaveURL(/\/gas\/realms$/);
  await expect(page.locator('#view-gas-realms main > .pagenav')).toHaveCount(1);
  await expect(page.locator('#nav-gas-realms')).toHaveClass(/active/);
});

// /params is where the page shipped and what other pages link to;
// /govdao/params is where the nav puts it. Both have to land on the same
// view with the same rail entry lit, and neither may redirect the other.
test('the params page answers on both of its paths', async ({ page }) => {
  for (const path of ['/params', '/govdao/params']) {
    const seen = watch(page);
    await page.goto(path);
    await settle(page);
    await expect(page).toHaveURL(new RegExp(`${path.replace('/', '\\/')}$`));
    await expect(page.locator('#view-params')).toHaveClass(/active/);
    await expect(page.locator('#nav-params')).toHaveClass(/active/);
    expect(unexpected(seen.consoleErrors), `console errors on ${path}`).toEqual([]);
  }
});

// A section header used to carry a twisty, which made the whole row read as
// a disclosure header and hid the fact that the label is itself a link to a
// real page. The twisty is gone, so what has to hold now is that clicking
// the label navigates, that no twisty is left rendered, and that no
// section's children are ever folded away.
test('a section header is a link, with no twisty and nothing folded', async ({ page }) => {
  await page.goto('/accounts');
  await settle(page);

  await expect(page.locator('.rail nav .nav-twisty')).toHaveCount(0);
  // Every section has its children showing, however many sections there are.
  const groups = await page.locator('.rail nav .nav-group').count();
  expect(groups).toBeGreaterThan(0);
  const kids = page.locator('.rail nav .nav-kids');
  await expect(kids).toHaveCount(groups);
  for (let i = 0; i < groups; i++) await expect(kids.nth(i)).toBeVisible();

  await page.locator('#nav-gas').click();
  await settle(page);
  await expect(page).toHaveURL(/\/gas$/);
  await expect(page.locator('#nav-gas')).toHaveClass(/active/);
});

// /tokens was the URL when the bank figures and the GRC20 ledger were one
// page. It has to keep answering, and it has to land on the grc20 view with
// that pill lit rather than on a page that no longer exists.
test('the old /tokens url lands on grc20', async ({ page }) => {
  const seen = watch(page);
  await page.goto('/tokens');
  await settle(page);

  await expect(page.locator('#view-grc20')).toHaveClass(/active/);
  await expect(page.locator('#nav-grc20')).toHaveClass(/active/);
  await expect(page.locator('#nav-defi')).toHaveClass(/in-section/);
  await expect(page.locator('.view.active main > .pagenav a.active')).toHaveText('grc20');
  expect(unexpected(seen.consoleErrors)).toEqual([]);
});

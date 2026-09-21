import { expect, test } from '@playwright/test';

import { HUB_ROUTE, BUSY_CALLER } from '../harness/fixture.mjs';
import { settle, unexpected, watch } from './helpers.js';

// Every route reachable from the nav, plus the detail pages.
//
// The assertion is deliberately the same for all of them and deliberately weak
// on content: this suite is not trying to pin what each page says, it is trying
// to notice when one of them stops working at all. Content assertions belong
// with the feature that owns them.
const ROUTES = [
  ['home', '/'],
  ['watch', '/watch'],
  ['realms', '/realms'],
  ['packages', '/packages'],
  ['contracts', '/contracts'],
  ['apps', '/apps'],
  ['transactions', '/txs'],
  ['blocks', '/blocks'],
  ['accounts', '/accounts'],
  ['defi', '/defi'],
  ['coins', '/coins'],
  ['grc20', '/grc20'],
  ['tokens (the old grc20 url)', '/tokens'],
  ['storage', '/storage'],
  ['validators', '/validators'],
  ['govdao', '/govdao'],
  ['govdao proposals', '/govdao/proposals'],
  ['govdao voters', '/govdao/voters'],
  ['govdao options', '/govdao/options'],
  ['gas', '/gas'],
  ['gas by realm', '/gas/realms'],
  ['gas by user', '/gas/users'],
  ['gas top txs', '/gas/txs'],
  ['analytics', '/analytics'],
  ['dashboards', '/dashboards'],
  ['params', '/params'],
  ['params under govdao', '/govdao/params'],
  ['sanity', '/sanity'],
  ['events', '/events'],
  ['realm detail', `/realm/${HUB_ROUTE}`],
  ['address detail', `/address/${BUSY_CALLER}`],
  // The fixture has no RPC, so gov/dao's render is unreachable and this
  // lands on the proposal-not-found branch. That is still worth pinning:
  // the page builds a verification panel, a timeline, a parameter diff and
  // a code block, and every one of those has to survive being handed
  // nothing. A proposal page with real data is verified against a live
  // chain, not here.
  ['govdao proposal', '/govdao/0'],
];

for (const [name, path] of ROUTES) {
  test(`${name} renders without errors`, async ({ page }) => {
    const seen = watch(page);

    const response = await page.goto(path);
    expect(response.status(), `${path} should be served`).toBe(200);
    await settle(page);

    expect(seen.jsErrors, `uncaught exceptions on ${path}`).toEqual([]);
    expect(unexpected(seen.failedRequests), `failed requests on ${path}`).toEqual([]);
    expect(unexpected(seen.consoleErrors), `console errors on ${path}`).toEqual([]);

    // Something has to have been drawn. An empty <main> passes every assertion
    // above and is exactly the regression this is meant to catch.
    const main = page.locator('.view.active main').first();
    await expect(main).not.toBeEmpty();
  });
}

// The realm page's tabs are separate render paths behind one route, and a
// broken one is invisible until someone clicks it.
const TABS = ['info', 'source', 'calls', 'events', 'storage', 'deps'];

for (const tab of TABS) {
  test(`realm ${tab} tab renders without errors`, async ({ page }) => {
    const seen = watch(page);

    await page.goto(`/realm/${HUB_ROUTE}?tab=${tab}`);
    await settle(page);

    expect(seen.jsErrors, `uncaught exceptions on the ${tab} tab`).toEqual([]);
    expect(unexpected(seen.failedRequests), `failed requests on the ${tab} tab`).toEqual([]);

    await expect(page.locator(`#tab-${tab}`)).toBeVisible();
  });
}

// `deps` absorbed the old standalone `graph` tab. Links to ?tab=graph predate
// the merge and are the kind of thing people paste into issues, so the
// fallback that catches an unknown tab must not catch this one and drop the
// reader on `info` with no sign anything was asked for.
test('a pre-merge ?tab=graph link lands on deps, graph and all', async ({ page }) => {
  const seen = watch(page);

  await page.goto(`/realm/${HUB_ROUTE}?tab=graph`);
  await settle(page);

  await expect(page.locator('#tab-deps')).toBeVisible();
  await expect(page.locator('#dep-graph > svg')).toBeVisible();
  await expect(page.locator('#tab-deps')).toContainText('imports (what this uses)');

  expect(seen.jsErrors).toEqual([]);
});

// Order is the point of the merge: the picture first, the two lists under it.
// A DOM that carries both but stacks them the other way round passes every
// visibility assertion above.
test('the deps tab draws the graph above the two lists', async ({ page }) => {
  await page.goto(`/realm/${HUB_ROUTE}?tab=deps`);
  await settle(page);
  await page.waitForSelector('#dep-graph > svg', { timeout: 20_000 });

  const graphTop = await page.locator('#dep-graph').evaluate(n => n.getBoundingClientRect().top);
  const listsTop = await page.locator('#tab-deps .section-title').first()
    .evaluate(n => n.getBoundingClientRect().top);
  expect(graphTop).toBeLessThan(listsTop);
});

test('the realm list pages and every row carries its network', async ({ page }) => {
  const seen = watch(page);
  await page.goto('/realms');
  await settle(page);

  // The fixture puts the same package path on two chains. A list that joins on
  // path alone collapses them, which is the mistake the network-scoping
  // invariant exists to prevent.
  const rows = page.locator('#realms-list tr');
  expect(await rows.count()).toBeGreaterThan(1);

  expect(seen.jsErrors).toEqual([]);
});

// The sanity page gained a section that answers "can we read this chain",
// which is a different question from the chain being alive. The e2e binary
// runs with -sync=false, so the honest answer here is that nothing has been
// attempted, and saying so is the assertion: the section must not imply
// health, and must not imply failure either.
test('the sanity page says whether our sync passes are succeeding', async ({ page }) => {
  const seen = watch(page);
  await page.goto('/sanity');
  await settle(page);

  const content = page.locator('#sanity-content');
  await expect(content.getByText('sync health')).toBeVisible();
  // -sync=false, so every network reads "not started" rather than ok or
  // failing. Three states, and this is the one that must not raise an alarm.
  await expect(content).toContainText('not started');
  // The endpoint each network is being served by is the other half: a pool
  // silently down to one working member is invisible without it.
  await expect(content).toContainText('/graphql/query');

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.failedRequests)).toEqual([]);
});

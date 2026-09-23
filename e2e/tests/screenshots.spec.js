import { mkdirSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import { test } from '@playwright/test';

import { HUB_ROUTE, BUSY_CALLER, LIBRARY_ROUTE } from '../harness/fixture.mjs';
import { settle } from './helpers.js';

// Screenshots for review, captured against the seeded fixture.
//
// Distinct in purpose from scripts/screenshots.sh, which renders a real
// database snapshot for the README: those want a chain with enough history to
// look like something. These want to be *reproducible*, so a reviewer can see
// what a frontend change did to every page without checking the branch out.
// Same reason the suite exists at all — the JSON can be perfect and the page
// still blank.
//
// Skipped unless asked for: capturing on every run would slow the suite and
// write files nobody looked at.
const OUT = join(dirname(fileURLToPath(import.meta.url)), '..', '..', 'docs', 'images', 'review');

const PAGES = [
  ['home', '/'],
  ['realms', '/realms'],
  // The same page with every capture refused, which is the state a reviewer
  // cannot otherwise see: it is what a reader gets while the cache is cold, if
  // the capture service is down, or on a chain nothing has photographed yet.
  ['realms-cold', '/realms'],
  ['packages', '/packages'],
  ['contracts', '/contracts'],
  // One per layout: a change to any of the six is invisible in a single shot
  // of whichever one happens to be the default.
  ['contracts-orbit', '/contracts?view=orbit&edges=imports&flow=0'],
  ['contracts-packed', '/contracts?view=packed'],
  ['contracts-bundled', '/contracts?view=bundled&edges=imports&metric=importers&flow=0'],
  ['contracts-chord', '/contracts?view=chord&edges=imports'],
  // Sized by calls rather than storage: the fixture's storage figures are all
  // zero, so a shot of that metric cannot show whether the cells print their
  // value at all, which is half of what this view now draws.
  ['contracts-treemap', '/contracts?view=treemap&edges=none&metric=calls'],
  // The activity filter, in the view it exists for: the import graph with the
  // closure on. Without '+ imports' this is a handful of dots, which is the
  // regression a shot of the default view would never catch.
  ['contracts-active', '/contracts?view=bundled&edges=imports&metric=importers&flow=0&active=1&deps=1'],
  // The hub in all three of its shapes: no chain picked (descriptions only,
  // no numbers, off-chain apps only) and a chain picked (the ranked grid).
  ['apps', '/apps'],
  ['apps-network', '/apps?network=alpha'],
  ['transactions', '/txs'],
  ['blocks', '/blocks'],
  ['accounts', '/accounts'],
  ['defi', '/defi'],
  ['coins', '/coins'],
  ['grc20', '/grc20'],
  ['validators', '/validators'],
  ['analytics', '/analytics'],
  ['gas', '/gas'],
  // The three list pages the gas summary now links to rather than stacking.
  ['gas-realms', '/gas/realms'],
  ['gas-users', '/gas/users'],
  ['gas-txs', '/gas/txs'],
  ['govdao', '/govdao'],
  ['govdao-proposals', '/govdao/proposals'],
  ['govdao-voters', '/govdao/voters'],
  ['govdao-options', '/govdao/options'],
  ['params', '/params'],
  ['sanity', '/sanity'],
  // The rail folded to icons: the section children are gone there, and the
  // .pagenav strip is the only way through to them.
  ['rail-collapsed', '/gas'],
  // Two of the four groupings: they differ in what the map is made of, not
  // just in its colours, and a shot of one would not show a change to the
  // payer attribution at all.
  ['storage', '/storage?network=alpha&group=namespace'],
  ['storage-payer', '/storage?network=alpha&group=payer&order=size'],
  ['realm-detail', `/realm/${HUB_ROUTE}`],
  ['realm-deps', `/realm/${HUB_ROUTE}?tab=deps`],
  ['realm-source', `/realm/${HUB_ROUTE}?tab=source`],
  // The two-column layout: a package with enough declarations to need a symbol
  // outline beside them. `realm-docs` above is the one-column case.
  ['realm-docs-outline', `/realm/${LIBRARY_ROUTE}?tab=docs`],
  // The docs tab, and the same tab arrived at from symbol search: ?sym= opens
  // the collapse if it has to and highlights the declaration, which is the
  // half of symbol search that is not a JSON response.
  ['realm-docs', `/realm/${HUB_ROUTE}?tab=docs`],
  ['realm-docs-symbol', `/realm/${HUB_ROUTE}?tab=docs&sym=Render`],
  // The three tabs that carry a chart. Pinned to a network because the
  // storage tab refuses the all-chains case by design, and a shot of that
  // refusal would show none of what changed here.
  // The defi tab in both graph modes. The two are different pictures of the
  // same legs, and a shot of whichever happens to be the default would not
  // show a change to the other at all.
  ['realm-defi', `/realm/${HUB_ROUTE}?network=alpha&tab=defi`],
  ['realm-defi-detail', `/realm/${HUB_ROUTE}?network=alpha&tab=defi&flow=detail`],
  ['realm-storage', `/realm/${HUB_ROUTE}?network=alpha&tab=storage`],
  ['realm-calls', `/realm/${HUB_ROUTE}?network=alpha&tab=calls`],
  ['realm-events', `/realm/${HUB_ROUTE}?network=alpha&tab=events`],
  ['address-detail', `/address/${BUSY_CALLER}`],
];

test.describe('screenshots', () => {
  test.skip(!process.env.SCREENSHOTS, 'set SCREENSHOTS=1 to capture');

  test.beforeAll(() => {
    mkdirSync(OUT, { recursive: true });
  });

  for (const [name, path] of PAGES) {
    test(`capture ${name}`, async ({ page }) => {
      await page.setViewportSize({ width: 1400, height: 900 });
      if (name === 'realms-cold') {
        await page.route('**/api/shot**', route => route.fulfill({ status: 503, body: '' }));
      }
      if (name === 'rail-collapsed') {
        await page.goto('/');
        await page.evaluate(() => localStorage.setItem('mygnoscan-rail', 'collapsed'));
      }
      await page.goto(path);
      await settle(page);
      // The dependency graph runs a force simulation that keeps moving after
      // the network goes quiet, so settle() is not enough for that one page.
      // Both of these run a force simulation that keeps moving after the
      // network goes quiet, so settle() is not enough for either.
      if (path.includes('tab=deps') || path.includes('tab=defi')) {
        await page.waitForTimeout(2500);
      }
      await page.screenshot({ path: join(OUT, `${name}.png`), fullPage: true });
      if (name === 'rail-collapsed') {
        await page.evaluate(() => localStorage.removeItem('mygnoscan-rail'));
      }
    });
  }
});

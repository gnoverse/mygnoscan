import { mkdirSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import { test } from '@playwright/test';

import { HUB_ROUTE, BUSY_CALLER } from '../harness/fixture.mjs';
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
  ['packages', '/packages'],
  ['transactions', '/txs'],
  ['blocks', '/blocks'],
  ['accounts', '/accounts'],
  ['validators', '/validators'],
  ['analytics', '/analytics'],
  ['gas', '/gas'],
  ['realm-detail', `/realm/${HUB_ROUTE}`],
  ['realm-graph', `/realm/${HUB_ROUTE}?tab=graph`],
  ['realm-source', `/realm/${HUB_ROUTE}?tab=source`],
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
      await page.goto(path);
      await settle(page);
      // The dependency graph runs a force simulation that keeps moving after
      // the network goes quiet, so settle() is not enough for that one page.
      if (path.includes('tab=graph')) {
        await page.waitForTimeout(2500);
      }
      await page.screenshot({ path: join(OUT, `${name}.png`), fullPage: true });
    });
  }
});

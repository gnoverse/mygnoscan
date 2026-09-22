// The docs tab's symbol outline.
//
// A package can declare a lot: on mainnet r/gnoswap/staker has 444 symbols
// across 25 types, which is an unnavigable wall on one page. Below a threshold
// the whole table fits on a screen and a rail would be a second copy of what
// the reader can already see, so the layout is conditional and both halves of
// that condition are worth pinning.
import { expect, test } from '@playwright/test';

import {
  HUB_ROUTE, LIBRARY_ROUTE, LIBRARY_EXPORTED_SYMBOLS, LIBRARY_UNEXPORTED_SYMBOLS,
} from '../harness/fixture.mjs';
import { settle, unexpected, watch } from './helpers.js';

const OUTLINE = '#tab-docs .docs-outline';

test('a large package gets an outline, grouped the way godoc groups', async ({ page }) => {
  const seen = watch(page);
  await page.goto(`/realm/${LIBRARY_ROUTE}?tab=docs`);
  await settle(page);

  const outline = page.locator(OUTLINE);
  await expect(outline).toBeVisible();

  const groups = await outline.locator('.outline-group').allTextContents();
  // godoc's own ordering, which is what a reader of Go documentation expects.
  expect(groups.map(g => g.trim().split(/\s+/)[0])).toEqual(
    ['constants', 'variables', 'types', 'functions']);

  // One entry per visible declaration, methods included.
  await expect(outline.locator('a')).toHaveCount(LIBRARY_EXPORTED_SYMBOLS);

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.consoleErrors)).toEqual([]);
});

// A method is only reachable through its type, so the outline nests it under
// the type rather than listing it as a peer of the top-level functions.
test('methods are nested under their type and qualified by it', async ({ page }) => {
  await page.goto(`/realm/${LIBRARY_ROUTE}?tab=docs`);
  await settle(page);

  const outline = page.locator(OUTLINE);
  // Alphabetical within the group and each type's methods under it, which is
  // the order the analyzer sorts into rather than the order the file declares:
  // Iterator sorts before Tree, and the methods follow their own type.
  await expect(outline.locator('a.outline-method')).toHaveText(
    ['Iterator.Next', 'Iterator.Reset', 'Tree.Get', 'Tree.Set', 'Tree.Size']);
  // Qualified, because a bare name collides across types and with a top-level
  // function: Get is on Tree only, Reset on Iterator only.
  await expect(outline.locator('a', { hasText: 'Iterator.Reset' })).toHaveCount(1);
});

test('clicking an outline entry moves to that declaration', async ({ page }) => {
  await page.goto(`/realm/${LIBRARY_ROUTE}?tab=docs`);
  await settle(page);

  const target = page.locator(`${OUTLINE} a`, { hasText: 'Validate' }).first();
  await target.click();
  await page.waitForTimeout(600); // smooth scroll

  const card = page.locator('#tab-docs [data-sym="Validate"]');
  await expect(card).toBeVisible();
  await expect(card).toBeInViewport();
});

// The outline is drawn from the same filtered lists the body is, so an entry
// can never point at a card that is not on the page.
test('the outline follows the unexported toggle', async ({ page }) => {
  await page.goto(`/realm/${LIBRARY_ROUTE}?tab=docs`);
  await settle(page);

  const outline = page.locator(OUTLINE);
  await expect(outline.locator('a')).toHaveCount(LIBRARY_EXPORTED_SYMBOLS);
  await expect(outline.locator('a', { hasText: 'unexportedHelper' })).toHaveCount(0);

  await page.locator('#tab-docs a', { hasText: /show \d+ unexported/ }).click();
  await expect(outline.locator('a')).toHaveCount(
    LIBRARY_EXPORTED_SYMBOLS + LIBRARY_UNEXPORTED_SYMBOLS);
  const hidden = outline.locator('a', { hasText: 'unexportedHelper' });
  await expect(hidden).toHaveCount(1);
  await expect(hidden).toHaveClass(/outline-unexported/);

  // And it points at a card that is now really there.
  await hidden.click();
  await expect(page.locator('#tab-docs [data-sym="unexportedHelper"]')).toBeVisible();
});

// Below the threshold there is nothing to navigate, and a rail beside a
// three-line table is noise.
test('a small package gets no outline', async ({ page }) => {
  await page.goto(`/realm/${HUB_ROUTE}?tab=docs`);
  await settle(page);
  await expect(page.locator('#tab-docs [data-sym]').first()).toBeVisible();
  await expect(page.locator(OUTLINE)).toHaveCount(0);
});

// Symbol search lands here with ?sym=, and the card it names has to be found
// inside the two-column layout as well as outside it.
test('a deep link still finds its declaration in the two-column layout', async ({ page }) => {
  await page.goto(`/realm/${LIBRARY_ROUTE}?tab=docs&sym=Merge`);
  await settle(page);
  await expect(page.locator(OUTLINE)).toBeVisible();
  const card = page.locator('#tab-docs [data-sym="Merge"]');
  await expect(card).toBeVisible();
  await expect(card).toHaveCSS('background-color', /rgba?\(/);
});

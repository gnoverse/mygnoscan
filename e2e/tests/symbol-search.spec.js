// Symbol search: finding a package by what it declares, not by what its path
// spells.
//
// The search box used to match a path, a name or a creator, which means the
// only way to find `IterateByOffset` was to already know which package has it.
import { expect, test } from '@playwright/test';

import { HUB, HUB_ROUTE } from '../harness/fixture.mjs';
import { settle } from './helpers.js';

// The fixture's packages all declare `func Render(path string) string`, so
// Render is the one symbol guaranteed to be in the index.
const SYMBOL = 'Render';

// Opening a package's docs tab is what puts it in the index, as well as the
// background pass: a package deployed a minute ago is not in the corpus walk
// yet, and the first person to care is the one looking at it.
async function indexHub(page) {
  await page.goto(`/realm/${HUB_ROUTE}?tab=docs`);
  await settle(page);
}

test('the api finds a declaration by name', async ({ page, request }) => {
  await indexHub(page);

  const res = await request.get(`/api/symbols/search?q=${SYMBOL}`);
  expect(res.status()).toBe(200);
  const body = await res.json();
  expect(Array.isArray(body.symbols)).toBe(true);
  expect(body.symbols.length).toBeGreaterThan(0);

  const hit = body.symbols.find(s => s.path === HUB);
  expect(hit, `a ${SYMBOL} declared by ${HUB}`).toBeTruthy();
  // A row has to be renderable from one request.
  expect(hit.kind).toBe('func');
  expect(hit.signature).toContain('func Render');
  expect(hit.display).toBe(SYMBOL);
});

test('a wildcard is a literal, not a request for every symbol on the chain', async ({ page, request }) => {
  await indexHub(page);
  const res = await request.get('/api/symbols/search?q=%25'); // "%"
  expect(res.status()).toBe(200);
  expect((await res.json()).symbols).toEqual([]);
});

test('the search box offers symbols, and clicking one lands on the declaration', async ({ page }) => {
  await indexHub(page);

  await page.goto('/');
  await settle(page);
  await page.locator('#search-input').fill(SYMBOL);

  const results = page.locator('#search-results');
  await expect(results.locator('.search-section-label', { hasText: 'symbols' })).toBeVisible({ timeout: 10_000 });

  // Symbols before packages: somebody typing an identifier wants the
  // declaration, and a path that merely contains the string is the weaker
  // answer of the two.
  const labels = await results.locator('.search-section-label').allTextContents();
  const symIdx = labels.indexOf('symbols');
  const pkgIdx = labels.indexOf('packages');
  expect(symIdx).toBeGreaterThanOrEqual(0);
  if (pkgIdx >= 0) expect(symIdx).toBeLessThan(pkgIdx);

  const row = results.locator('.search-result').filter({ hasText: SYMBOL }).first();
  await row.click();

  // Straight to the declaration, not to the realm's overview: landing there
  // and making the reader find it again is most of why this was worth having.
  await expect(page).toHaveURL(/tab=docs/);
  await expect(page).toHaveURL(new RegExp('sym=' + SYMBOL));
  await settle(page);
  await expect(page.locator(`#tab-docs [data-sym="${SYMBOL}"]`)).toBeVisible();
});

test('a deep link to a symbol highlights it on the docs tab', async ({ page }) => {
  await indexHub(page);
  await page.goto(`/realm/${HUB_ROUTE}?tab=docs&sym=${SYMBOL}`);
  await settle(page);
  const card = page.locator(`#tab-docs [data-sym="${SYMBOL}"]`);
  await expect(card).toBeVisible();
  // Highlighted, or the reader has to find it on the page anyway.
  await expect(card).toHaveCSS('background-color', /rgba?\(/);
});

test('the index reports what it covers', async ({ page, request }) => {
  await indexHub(page);
  const res = await request.get('/api/symbols/status');
  expect(res.status()).toBe(200);
  const st = await res.json();
  expect(st.packages).toBeGreaterThan(0);
  expect(st.symbols).toBeGreaterThan(0);
});

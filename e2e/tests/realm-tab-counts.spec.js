import { expect, test } from '@playwright/test';

import { HUB, HUB_ROUTE } from '../harness/fixture.mjs';
import { settle, unexpected, watch } from './helpers.js';

// Seven bare words told a reader nothing about which tabs had anything in them,
// so finding out cost a click and a fetch per tab. The counts are the fix, and
// the thing that goes wrong with counts is that they quietly stop updating: they
// are drawn once and then either never refreshed or refreshed with the previous
// page's numbers. Hence the second test.

// The count is read out of the strip rather than asserted as a literal, and
// checked against the API the page itself rendered from. A literal here would
// pass against a strip showing the right number for the wrong realm.
const countOf = async (page, name) => {
  const el = page.locator(`#realm-tabs .tab[data-tab="${name}"] .tab-count`);
  if (await el.count() === 0) return null;
  return Number((await el.innerText()).replace(/[(),]/g, ''));
};

test('the tab strip counts what is behind each tab', async ({ page }) => {
  const seen = watch(page);
  await page.goto(`/realm/${HUB_ROUTE}?network=alpha`);
  await settle(page);

  const detail = await (await page.request.get(
    `/api/realm/${HUB_ROUTE}?network=alpha`)).json();

  // Guard the assertions below against passing vacuously: a fixture realm with
  // no calls would make "count equals call_count" true of a strip showing
  // nothing at all.
  expect(detail.call_count).toBeGreaterThan(0);
  expect(detail.files.length).toBeGreaterThan(0);

  // The four the detail payload already carries, so they need no request of
  // their own and are on screen before any tab is opened.
  expect(await countOf(page, 'calls')).toBe(detail.call_count);
  expect(await countOf(page, 'source')).toBe(detail.files.length);
  expect(await countOf(page, 'deps')).toBe(
    detail.imports.length + detail.dependents.length);
  expect(await countOf(page, 'docs')).toBeGreaterThan(0);

  // info is a description, not a list. A number beside it would be a number of
  // what.
  expect(await countOf(page, 'info')).toBeNull();

  // storage arrives late, from the indexer, through setTabCount. Its count is
  // storage events, so it matches the rows the tab draws.
  const storage = await (await page.request.get(
    `/api/storage/${HUB_ROUTE}?network=alpha`)).json();
  const entries = (storage.storage || {}).entries || [];
  expect(entries.length).toBeGreaterThan(0);
  expect(await countOf(page, 'storage')).toBe(entries.length);

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.consoleErrors)).toEqual([]);
});

test('a tab with nothing in it shows no count at all', async ({ page }) => {
  const seen = watch(page);
  // A dependent realm: deployed, never called, and nothing imports it.
  await page.goto('/realm/r/consumer42/app?network=alpha');
  await settle(page);

  const detail = await (await page.request.get(
    '/api/realm/r/consumer42/app?network=alpha')).json();
  expect(detail.call_count).toBe(0);

  // Not "(0)". Nine parenthesised zeros on a fresh realm is noise, and absence
  // already says "nothing here" more quietly than a number does.
  expect(await countOf(page, 'calls')).toBeNull();
  await expect(page.locator('#realm-tabs .tab[data-tab="calls"]')).toHaveText('calls');

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.consoleErrors)).toEqual([]);
});

test('the counts follow the realm, and do not stick from the previous one', async ({ page }) => {
  const seen = watch(page);
  await page.goto(`/realm/${HUB_ROUTE}?network=alpha`);
  await settle(page);
  const busy = await countOf(page, 'deps');
  expect(busy).toBeGreaterThan(1);

  // The hub has sixty dependents; a leaf realm has none of them. A strip that
  // is drawn once and never rebuilt, or one whose late counts land on whatever
  // page is on screen when the fetch resolves, fails right here.
  await page.goto('/realm/r/consumer42/app?network=alpha');
  await settle(page);
  expect(await countOf(page, 'deps')).toBeLessThan(busy);

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.consoleErrors)).toEqual([]);
});

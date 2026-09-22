import { expect, test } from '@playwright/test';

import { HUB_ROUTE, USAGE_BID_CALLS, USAGE_MESSAGES, USAGE_ROUTE } from '../harness/fixture.mjs';
import { TAB_EVENTS } from '../harness/fake-indexer.mjs';
import { settle, unexpected, watch } from './helpers.js';

// A filter the reader set is part of what they are looking at, so it has to be
// in the URL: otherwise a reload throws it away and a pasted link sends the
// recipient the unfiltered page, which is the one view the sender was
// deliberately not looking at.
//
// What each test here pins is the round trip, not the parameter name: set the
// filter, reload, assert the *rows* came back narrowed. A test that only
// checked the URL would pass against a page that writes the parameter and
// never reads it, which is half a feature and the half nobody notices.

// The site-wide table enhancement keys its filter and its sort on the table's
// header row. Spelled out here rather than recomputed from the page: this key
// is the contract every link somebody already sent depends on, and a test that
// derived it would agree with whatever the code does today and notice nothing.
const REALMS_KEY = 'path-creator-calls-gas-used-last-call-added';

const n = (v) => Number(v.replace(/[^0-9]/g, ''));

async function stat(page, label) {
  return page.locator('#tab-calls .stats-bar .stat', { hasText: label })
    .first().locator('.value').innerText();
}

// Visible rows, which is what the client-side table filter actually changes:
// it hides rows rather than removing them, so a count of `tr` would not move.
function shown(page, selector) {
  return page.locator(selector).evaluateAll(
    rows => rows.filter(tr => tr.style.display !== 'none').length);
}

// Forces the two-render case, which is the one that broke.
//
// apiSWR caches in sessionStorage, which survives a reload, so the second visit
// to a page paints cached rows and then replaces them with fresh ones. Restoring
// on the first paint and not the second leaves the full table under a filter box
// that says it is narrowed. On localhost the fresh response usually lands inside
// enhanceTables' 100ms debounce and the two renders collapse into one, so this
// passes by luck; delaying the fresh fetch past the debounce makes the failure
// deterministic instead of "green on my machine, red in the suite".
async function delayFresh(page, pattern, ms = 500) {
  await page.route(pattern, async route => {
    await new Promise(r => setTimeout(r, ms));
    await route.continue();
  });
}

test('a table filter survives a reload and travels in the link', async ({ page }) => {
  const seen = watch(page);
  await page.goto('/realms?network=alpha');
  await settle(page);

  const input = page.locator('#view-realms .table-filter').first();
  await expect(input).toHaveAttribute('data-param', `f.${REALMS_KEY}`);

  const before = await shown(page, '#view-realms table tbody tr');
  await input.fill('consumer0');
  const narrowed = await shown(page, '#view-realms table tbody tr');
  expect(narrowed).toBeGreaterThan(0);
  expect(narrowed).toBeLessThan(before);

  expect(new URL(page.url()).searchParams.get(`f.${REALMS_KEY}`)).toBe('consumer0');

  // The round trip. Reloading the URL the page just wrote has to land on the
  // narrowed table, with the box still saying what it is narrowed to, and it has
  // to still be narrowed once the fresh rows land on top of the cached ones.
  await delayFresh(page, '**/api/realms*');
  await page.reload();
  await settle(page);
  await expect(page.locator('#view-realms .table-filter').first()).toHaveValue('consumer0');
  await expect.poll(() => shown(page, '#view-realms table tbody tr')).toBe(narrowed);

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.consoleErrors)).toEqual([]);
});

test('a sorted column survives a reload, named rather than numbered', async ({ page }) => {
  const seen = watch(page);
  await page.goto('/realms?network=alpha');
  await settle(page);

  const firstCell = () => page.locator('#view-realms table tbody tr').first().locator('td').first().innerText();
  const header = page.locator('#view-realms table thead th', { hasText: 'calls' }).first();

  // One click, not two: a server-sorted column starts descending now, so that
  // its page-local order agrees with the order its loader just fetched. What
  // this test is about is the round trip through the URL, which is the same
  // either way.
  await header.click();                       // descending
  expect(new URL(page.url()).searchParams.get(`s.${REALMS_KEY}`)).toBe('calls:desc');
  const top = await firstCell();

  await delayFresh(page, '**/api/realms*');
  await page.reload();
  await settle(page);
  await expect.poll(firstCell).toBe(top);
  await expect(page.locator('#view-realms table thead th', { hasText: 'calls' }).first())
    .toHaveClass(/sort-desc/);

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.consoleErrors)).toEqual([]);
});

test('/txs carries its type and status filters, in words rather than wire names', async ({ page }) => {
  const seen = watch(page);
  await page.goto('/txs?network=alpha');
  await settle(page);

  await page.locator('#txs-filter button:text-is("deploy")').click();
  await settle(page);
  await page.locator('#txs-filter button:text-is("fail")').click();
  await settle(page);

  const params = new URL(page.url()).searchParams;
  expect(params.get('type')).toBe('deploy');
  expect(params.get('status')).toBe('fail');

  // Reloading has to reach the *server* with the same filter: these are applied
  // in SQL, so a restored-looking filter bar over unfiltered rows is the
  // failure this catches.
  const asked = [];
  page.on('request', req => { if (req.url().includes('/api/txs?')) asked.push(req.url()); });
  await page.reload();
  await settle(page);

  expect(asked.length).toBeGreaterThan(0);
  expect(asked[asked.length - 1]).toContain('type=MsgAddPackage');
  expect(asked[asked.length - 1]).toContain('success=false');

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.consoleErrors)).toEqual([]);
});

test('the realm calls tab narrows from the URL, and a narrowing writes it back', async ({ page }) => {
  const seen = watch(page);
  await page.goto(`/realm/${USAGE_ROUTE}?network=alpha&tab=calls`);
  await settle(page);

  const bidRow = page.locator('#tab-calls .usage-functions tbody tr', { hasText: 'Bid' }).first();
  await bidRow.locator('.usage-only').click();
  await settle(page);
  expect(new URL(page.url()).searchParams.get('func')).toBe('Bid');

  // These aggregates are computed in SQL over the whole history, so this is
  // the assertion that the filter reached the server and not just the chip.
  await page.reload();
  await settle(page);
  await expect(page.locator('#tab-calls .usage-chip')).toContainText('Bid');
  expect(n(await stat(page, 'messages'))).toBe(USAGE_BID_CALLS);

  // Clearing it takes the parameter out again, rather than leaving a link that
  // says "filtered" over an unfiltered page.
  await page.locator('#tab-calls .usage-chip-x').click();
  await settle(page);
  expect(new URL(page.url()).searchParams.has('func')).toBe(false);
  expect(n(await stat(page, 'messages'))).toBe(USAGE_MESSAGES);

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.consoleErrors)).toEqual([]);
});

test('the realm events storage toggle survives a reload', async ({ page }) => {
  const seen = watch(page);
  await page.goto(`/realm/${HUB_ROUTE}?network=alpha&tab=events`);
  await settle(page);

  const rows = page.locator('#tab-events table tbody tr');
  await expect(rows).toHaveCount(TAB_EVENTS.length);

  const toggle = page.locator('#tab-events label', { hasText: 'storage events' }).locator('input[type=checkbox]');
  await toggle.check();
  expect(new URL(page.url()).searchParams.get('storage')).toBe('1');

  await page.reload();
  await settle(page);
  await expect(page.locator('#tab-events label', { hasText: 'storage events' })
    .locator('input[type=checkbox]')).toBeChecked();
  await expect(page.locator('#tab-events table tbody tr')).toHaveCount(TAB_EVENTS.length * 2);

  expect(unexpected(seen.failedRequests)).toEqual([]);
  expect(seen.jsErrors).toEqual([]);
});

test('/blocks keeps the with-transactions filter', async ({ page }) => {
  const seen = watch(page);
  await page.goto('/blocks?network=alpha');
  await settle(page);

  // Against the page's own "with txs" figure rather than against a smaller
  // number than the unfiltered count: every block the fixture seeds carries a
  // transaction, so "it removed rows" is not assertable here and "it kept
  // exactly the ones it says it kept" is.
  const withTxs = Number((await page.locator('#blocks-stats .stat', { hasText: 'with txs' })
    .locator('.value').innerText()).replace(/[^0-9]/g, ''));
  expect(withTxs).toBeGreaterThan(0);

  await page.locator('#blocks-filter-txs').check();
  await expect(page.locator('#blocks-list tr')).toHaveCount(withTxs);
  expect(new URL(page.url()).searchParams.get('txs')).toBe('1');

  await page.reload();
  await settle(page);
  await expect(page.locator('#blocks-filter-txs')).toBeChecked();
  await expect(page.locator('#blocks-list tr')).toHaveCount(withTxs);

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.consoleErrors)).toEqual([]);
});

// Navigating away drops every one of these, because navigate() pushes a bare
// path. Without that, a filter set on one realm would follow the reader to the
// next one and quietly narrow a page they never filtered.
test('a navigation drops the filters rather than carrying them', async ({ page }) => {
  await page.goto('/realms?network=alpha');
  await settle(page);
  await page.locator('#view-realms .table-filter').first().fill('consumer0');
  expect(page.url()).toContain(`f.${REALMS_KEY}`);

  await page.locator('#nav-blocks').click();
  await settle(page);
  expect(page.url()).not.toContain(`f.${REALMS_KEY}`);
});

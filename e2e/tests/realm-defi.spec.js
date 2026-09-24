import { expect, test } from '@playwright/test';

import { GRC20_BALANCE, GRC20_IN, GRC20_OUT, HUB_ADDRESS, HUB_ROUTE } from '../harness/fixture.mjs';
import { TAB_NET_UGNOT, TAB_TRANSFERS } from '../harness/fake-indexer.mjs';
import { settle, unexpected, watch } from './helpers.js';

// The realm page could say who called a realm, what it emitted and what it
// costs to store, and nothing about money. These cover the tab that answers it:
// that it is loaded on demand rather than with the page, that the native
// reconstruction is signed correctly, that the GRC20 half reads the ledger by
// the realm's own derived address, and that a failed balance read is reported as
// unknown rather than as zero.
//
// No RPC is configured in this harness, so every live balance read fails. That
// is the interesting half here: the tab has to say so rather than print 0 GNOT,
// which is the failure mode that turns a broken read into a confident lie.

const gnot = ugnot => (ugnot / 1000000).toString();

test('the defi tab is not fetched until it is opened', async ({ page }) => {
  const seen = watch(page);
  const defiCalls = [];
  page.on('request', req => {
    if (req.url().includes('/api/realm/defi/')) defiCalls.push(req.url());
  });

  await page.goto(`/realm/${HUB_ROUTE}?network=alpha`);
  await settle(page);
  // Reconstructing a balance means every transfer leg the address ever had.
  // Paying that on a page load to fill a tab most readers never open is the
  // trade this avoids.
  expect(defiCalls).toEqual([]);

  await page.locator('#realm-tabs .tab[data-tab="defi"]').click();
  await settle(page);
  expect(defiCalls.length).toBe(1);

  // And not again on a second open: the tab remembers the render it was filled
  // for.
  await page.locator('#realm-tabs .tab[data-tab="info"]').click();
  await page.locator('#realm-tabs .tab[data-tab="defi"]').click();
  await settle(page);
  expect(defiCalls.length).toBe(1);

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.consoleErrors)).toEqual([]);
});

test('the native half reconstructs the balance from transfer events', async ({ page }) => {
  const seen = watch(page);
  await page.goto(`/realm/${HUB_ROUTE}?network=alpha&tab=defi`);
  await settle(page);

  const defi = page.locator('#tab-defi');
  await expect(defi).toHaveClass(/active/);

  // One row per transfer leg, in and out.
  await expect(defi.locator('#defi-flows tbody tr')).toHaveCount(TAB_TRANSFERS.length);

  // Signs, which is the thing a reconstruction gets wrong silently: the fixture
  // both receives and spends, so a reader that ignored direction would land on
  // the gross figure instead of the net one.
  const outgoing = TAB_TRANSFERS.filter(t => t.from === HUB_ADDRESS);
  expect(outgoing.length).toBeGreaterThan(0);
  for (const t of outgoing) {
    await expect(defi).toContainText(`-${gnot(t.amount)} GNOT`);
  }

  // No RPC here, so the live read failed. The tab must say the figures are the
  // reconstruction alone rather than print a confident zero.
  await expect(defi).toContainText('unknown');
  await expect(defi).toContainText(`${gnot(TAB_NET_UGNOT)} GNOT`);

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.consoleErrors)).toEqual([]);
});

test('the GRC20 half reads the ledger by the realm’s derived address', async ({ page }) => {
  const seen = watch(page);
  await page.goto(`/realm/${HUB_ROUTE}?network=alpha&tab=defi`);
  await settle(page);

  const defi = page.locator('#tab-defi');
  await expect(defi).toContainText('GRC20 positions');
  const row = defi.locator('#defi-tokens tbody tr').filter({ hasText: 'hubcoin' });
  await expect(row).toHaveCount(1);
  // Balance, received and sent, kept apart: "holds 0" and "never touched it"
  // are different states and their sum is not.
  await expect(row).toContainText(String(GRC20_BALANCE.toLocaleString('en-US')));
  await expect(row).toContainText(String(GRC20_IN.toLocaleString('en-US')));
  await expect(row).toContainText(String(GRC20_OUT.toLocaleString('en-US')));

  // The caveat travels with the figures: this ledger only ever saw what the
  // syncer walked, so a position is a floor.
  await expect(defi).toContainText('floors rather than balances');

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.consoleErrors)).toEqual([]);
});

// The legs the GRC20 curves are drawn from. The positions table says what the
// realm holds and the charts say when it moved; neither names a counterparty,
// which on a treasury is the question straight after "is it growing".
test('the GRC20 transfers table names every leg, including the mint', async ({ page }) => {
  const seen = watch(page);
  await page.goto(`/realm/${HUB_ROUTE}?network=alpha&tab=defi`);
  await settle(page);

  const rows = page.locator('#defi-token-flows tbody tr');
  await expect(rows).toHaveCount(4);

  // Direction, signed against the realm's own account rather than reported raw.
  const defi = page.locator('#tab-defi');
  await expect(defi.locator('#defi-token-flows')).toContainText('+400,000');
  await expect(defi.locator('#defi-token-flows')).toContainText('+250,000');
  await expect(defi.locator('#defi-token-flows')).toContainText('-150,000');

  // A leg with no sender is a mint, and saying so is the point: an empty
  // counterparty cell reads as a value the indexer failed to report, when in
  // fact there is no other end.
  const mint = rows.filter({ hasText: 'mint' });
  await expect(mint).toHaveCount(1);
  await expect(mint).toContainText('+100,000');

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.consoleErrors)).toEqual([]);
});

test('a realm with no money says so instead of drawing an empty chart', async ({ page }) => {
  const seen = watch(page);
  await page.goto('/realm/r/consumer42/app?network=alpha&tab=defi');
  await settle(page);

  const defi = page.locator('#tab-defi');
  await expect(defi).toContainText('no coin has ever moved through this realm');
  // The two derived accounts are still reported: they exist whether or not
  // anything was ever sent to them.
  await expect(defi).toContainText('held');

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.consoleErrors)).toEqual([]);
});

// The GRC20 half used to cap at 500 across every token a realm holds, with no
// offset and no total, so a realm sitting exactly on the cap was
// indistinguishable from one whose history is 500 long. Both r/gnoswap/pool and
// r/gnoswap/router do sit on it.
test('the GRC20 transfers page, and the response says how many there are', async ({ request }) => {
  const whole = await request.get(`/api/realm/defi/${HUB_ROUTE}?network=alpha`);
  expect(whole.status()).toBe(200);
  const all = await whole.json();
  const total = all.token_flows_total;
  expect(total).toBeGreaterThan(1);
  expect(all.token_flows).toHaveLength(total);
  expect(all.token_flows_shown).toBe(total);
  expect(all.token_flows_offset).toBe(0);

  // One row at a time, walked to the end: every leg seen exactly once and in
  // the same order the unpaged answer gave them.
  const walked = [];
  for (let offset = 0; offset < total; offset++) {
    const res = await request.get(
      `/api/realm/defi/${HUB_ROUTE}?network=alpha&token_flows_limit=1&token_flows_offset=${offset}`);
    expect(res.status()).toBe(200);
    const page = await res.json();
    expect(page.token_flows_total).toBe(total);
    expect(page.token_flows_offset).toBe(offset);
    expect(page.token_flows_shown).toBe(1);
    walked.push(page.token_flows[0].tx_hash);
  }
  expect(walked).toEqual(all.token_flows.map(f => f.tx_hash));

  // Past the end is an empty page at a clamped offset, not an error and not a
  // wrapped-around first page: a reader adding what it holds to what it was
  // given has to be able to stop.
  const over = await request.get(
    `/api/realm/defi/${HUB_ROUTE}?network=alpha&token_flows_offset=9999`);
  const tail = await over.json();
  expect(tail.token_flows).toEqual([]);
  expect(tail.token_flows_offset).toBe(total);
});

test('a truncated GRC20 table offers to load the rest', async ({ page }) => {
  const seen = watch(page);
  // The fixture cannot reach the 5,000-row page the frontend asks for, so the
  // truncation is stubbed: what is under test is that the page believes
  // token_flows_total over the length of the array it was handed.
  await page.route('**/api/realm/defi/**', async route => {
    const res = await route.fetch();
    const body = await res.json();
    body.token_flows_total = body.token_flows.length + 42;
    await route.fulfill({ response: res, json: body });
  });

  await page.goto(`/realm/${HUB_ROUTE}?network=alpha&tab=defi`);
  await settle(page);

  const note = page.locator('#tab-defi .detail-chart-note', { hasText: 'across every token above' });
  await expect(note).toContainText('most recent of');
  await expect(note.locator('button.pager-btn')).toContainText('load 42 older');

  expect(seen.jsErrors, 'uncaught exceptions').toEqual([]);
  expect(unexpected(seen.consoleErrors), 'console errors').toEqual([]);
});

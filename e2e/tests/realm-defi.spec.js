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

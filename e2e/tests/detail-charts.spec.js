import { expect, test } from '@playwright/test';

import {
  BUSY_CALLER, GRC20_BALANCE, GRC20_IN, GRC20_OUT, HUB_ROUTE,
} from '../harness/fixture.mjs';
import {
  TAB_EVENTS, TAB_GAS, TAB_GAS_FEE, TAB_GAS_USED, TAB_NET_BYTES, TAB_NET_FEE,
  TAB_DEPOSITED_FEE, TAB_REFUNDED_FEE, TAB_STORAGE,
} from '../harness/fake-indexer.mjs';
import { settle, unexpected, watch } from './helpers.js';

// Reads a rendered chart back out of Chart.js rather than out of the DOM.
//
// A canvas has no accessible content, so the only honest way to assert what
// was drawn is to ask the library what it was handed. This also fails loudly
// when the CDN is unreachable: `Chart` undefined means no chart was drawn at
// all, which is a real result and not something to skip past.
async function charts(page, container) {
  return page.evaluate((sel) => {
    if (typeof Chart === 'undefined') return null;
    return [...document.querySelectorAll(sel + ' canvas')].map(c => {
      const chart = Chart.getChart(c);
      return {
        title: chart.options.plugins.title.text,
        labels: chart.data.labels,
        datasets: chart.data.datasets.map(d => ({
          label: d.label,
          data: d.data,
          total: d.data.reduce((s, v) => s + (v || 0), 0),
          last: d.data[d.data.length - 1],
        })),
      };
    });
  }, container.startsWith('#') ? container : '#tab-' + container);
}

const series = (chart, label) => chart.datasets.find(d => d.label === label);

// One bucket per minute across the span the fixture's storage events cover.
// Derived rather than written down, so adding an event to the fixture moves
// the expectation with it.
const heights = TAB_STORAGE.map(e => e.height);
const STORAGE_BUCKETS = Math.max(...heights) - Math.min(...heights) + 1;

test('the storage tab draws an evolution curve that lands on the net figure', async ({ page }) => {
  const seen = watch(page);
  await page.goto(`/realm/${HUB_ROUTE}?network=alpha&tab=storage`);
  await settle(page);

  const drawn = await charts(page, 'storage');
  expect(drawn, 'Chart.js did not load, so nothing was drawn').not.toBeNull();
  expect(drawn.length).toBe(2);

  const evo = drawn[0];
  expect(evo.title).toContain('storage evolution');
  // The whole point of the chart: the running total ends at the realm's
  // current size. A cumulative sum over the payload in the order it arrives
  // (newest first) would end somewhere else entirely.
  expect(series(evo, 'net bytes held').last).toBe(TAB_NET_BYTES);
  expect(series(evo, 'bytes added').total).toBe(
    TAB_STORAGE.filter(e => e.bytes > 0).reduce((s, e) => s + e.bytes, 0));
  expect(series(evo, 'bytes released').total).toBe(
    TAB_STORAGE.filter(e => e.bytes < 0).reduce((s, e) => s + e.bytes, 0));
  // Gap-free buckets, not one bar per event: five events spread over 29
  // minutes must draw 29 slots, or the axis says they happened back to back.
  expect(evo.labels.length).toBe(STORAGE_BUCKETS);

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.consoleErrors)).toEqual([]);
});

test('the gas chart keeps failed transactions and totals the fees paid', async ({ page }) => {
  await page.goto(`/realm/${HUB_ROUTE}?network=alpha&tab=storage`);
  await settle(page);

  const gas = (await charts(page, 'storage'))[1];
  expect(gas.title).toContain('gas burned');
  const failed = TAB_GAS.filter(e => !e.success).reduce((s, e) => s + e.used, 0);
  expect(series(gas, 'gas used, failed').total).toBe(failed);
  expect(series(gas, 'gas used').total).toBe(TAB_GAS_USED - failed);
  expect(series(gas, 'fees paid, cumulative (ugnot)').last).toBe(TAB_GAS_FEE);
});

// The regression #255 fixed one layer down, and the one it left behind here:
// an unlock's fee_amount is a refund and arrives positive, so the total row
// used to add money returned to money paid.
test('the storage fee total is net of refunds, not deposits plus refunds', async ({ page }) => {
  await page.goto(`/realm/${HUB_ROUTE}?network=alpha&tab=storage`);
  await settle(page);

  const total = page.locator('#tab-storage tr.total-row').first();
  await expect(total).toContainText(TAB_NET_FEE.toLocaleString('en-US') + ' ugnot');
  await expect(total).not.toContainText((TAB_DEPOSITED_FEE + TAB_REFUNDED_FEE).toLocaleString('en-US'));
  await expect(total).toContainText(TAB_NET_BYTES.toLocaleString('en-US'));

  // And the reader can reach that number from the stats bar, which used to
  // print the gross deposit and nothing to subtract from it.
  const stats = page.locator('#tab-storage .stats-bar').first();
  await expect(stats).toContainText('refunded');
  await expect(stats).toContainText('net fee');
});

test('the calls tab charts activity split by function', async ({ page }) => {
  await page.goto(`/realm/${HUB_ROUTE}?network=alpha&tab=calls`);
  await settle(page);

  const drawn = await charts(page, 'calls');
  expect(drawn.length).toBe(1);
  expect(drawn[0].title).toContain('activity');
  // r/hub/core is called as Render, Write, Clear and Poke by the fixture, and
  // one series per function is the reason to draw this rather than a count.
  const labels = drawn[0].datasets.map(d => d.label);
  expect(labels).toContain('Render');
  // The deploy is a MsgAddPackage and so not in the feed the chart is drawn
  // from, but it is where the history starts, so it is added back — on an
  // unfiltered first page only.
  expect(labels).toContain('deploy');
  expect(series(drawn[0], 'deploy').total).toBe(1);
});

// ...and taken away again as soon as a filter is on, because then every other
// bar on the chart is something that matched that filter and the deploy is not.
test('a filtered calls chart drops the deploy bar', async ({ page }) => {
  await page.goto(`/realm/${HUB_ROUTE}?network=alpha&tab=calls`);
  await settle(page);
  await page.locator('#tab-calls .dash-seg button:text-is("calls")').click();
  await settle(page);

  const drawn = await charts(page, 'calls');
  expect(drawn.length).toBe(1);
  expect(drawn[0].datasets.map(d => d.label)).not.toContain('deploy');
});

test('the events tab charts the event mix', async ({ page }) => {
  await page.goto(`/realm/${HUB_ROUTE}?network=alpha&tab=events`);
  await settle(page);

  const drawn = await charts(page, 'events');
  expect(drawn.length).toBe(1);
  expect(drawn[0].title).toContain('events');
  for (const type of new Set(TAB_EVENTS.map(e => e.type))) {
    expect(series(drawn[0], type).total).toBe(TAB_EVENTS.filter(e => e.type === type).length);
  }
  // The fixture posts a storage deposit alongside every one of those, and it
  // must not become a series: the chain writes one per state-changing
  // transaction, so charting it draws the transaction count a second time and
  // outranks every real event type on a realm with few of them.
  expect(drawn[0].datasets.map(d => d.label)).not.toContain('StorageDepositEvent');
});

// The account page ranks who and what in three tables and said nothing about
// when: "active for a week two months ago" and "active every day since" drew
// the same page.
test('the address page charts activity by message type', async ({ page }) => {
  const seen = watch(page);
  await page.goto(`/address/${BUSY_CALLER}?network=alpha`);
  await settle(page);

  const drawn = await charts(page, '#address-detail-content');
  expect(drawn.length).toBe(1);
  expect(drawn[0].title).toContain('activity');
  // The fixture has this caller making MsgCalls, MsgRuns and BankMsgSends, and
  // all three have to be their own series: one merged "transactions" bar is
  // what the stats row above already says.
  const labels = drawn[0].datasets.map(d => d.label);
  expect(labels).toContain('MsgCall');
  expect(labels).toContain('BankMsgSend');
  expect(labels).toContain('MsgRun');

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.consoleErrors)).toEqual([]);
});

// The money side gets the same treatment as the bytes side, in two halves: the
// native curve over the banker's transfer legs, and one curve per GRC20 position
// because two tokens share no unit and a single stacked chart would draw a total
// nobody holds.
test('the defi tab draws a native curve and one per GRC20 position', async ({ page }) => {
  const seen = watch(page);
  await page.goto(`/realm/${HUB_ROUTE}?network=alpha&tab=defi`);
  await settle(page);

  const drawn = await charts(page, 'defi');
  expect(drawn, 'Chart.js did not load, so nothing was drawn').not.toBeNull();
  expect(drawn.length).toBe(2);

  expect(drawn[0].title).toContain('native balance');

  const token = drawn[1];
  expect(token.title).toContain('hubcoin balance');
  // Direction, which is the thing a reconstruction gets wrong silently: the
  // fixture both receives and spends this token, so a chart that ignored the
  // sign of a leg would stack the gross figure on both bars.
  expect(series(token, 'received').total).toBe(GRC20_IN);
  expect(series(token, 'sent').total).toBe(-GRC20_OUT);
  // Anchored on the position's balance rather than summed forward from zero
  // over the legs on screen: the ledger page is capped across every token, and
  // a curve that ended anywhere but where the table stands would be a lie the
  // reader has no way to catch.
  expect(series(token, 'balance held').last).toBe(GRC20_BALANCE);

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.consoleErrors)).toEqual([]);
});

// Charts hold a resize observer and a canvas. Leaving the old ones registered
// is invisible on screen and unbounded: every visit to a realm page would add
// four more live instances drawing into nodes no longer in the document.
test('leaving a realm page destroys the charts it drew', async ({ page }) => {
  await page.goto(`/realm/${HUB_ROUTE}?network=alpha&tab=storage`);
  await settle(page);
  const onRealm = await page.evaluate(() => _detailCharts.length);
  expect(onRealm).toBeGreaterThan(0);

  await page.goto('/realms?network=alpha');
  await settle(page);
  expect(await page.evaluate(() => _detailCharts.length)).toBe(0);

  // And coming back does not double them.
  await page.goto(`/realm/${HUB_ROUTE}?network=alpha&tab=storage`);
  await settle(page);
  expect(await page.evaluate(() => _detailCharts.length)).toBe(onRealm);
});

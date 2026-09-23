import { expect, test } from '@playwright/test';

import { HUB_ROUTE } from '../harness/fixture.mjs';
import { TAB_FUNDER, TAB_PAYEE, TAB_TRANSFERS } from '../harness/fake-indexer.mjs';
import { settle, unexpected, watch } from './helpers.js';

// The money graph on the defi tab.
//
// The table beside it answers "what happened" and cannot answer "who". What
// these assert is the part that would be wrong invisibly: the sign. `amount`
// on a flow is written from the realm's point of view, and a counterparty's
// own profit and loss is its negation. Both conventions produce a plausible
// leaderboard, and the wrong one prints the biggest loser as the biggest
// winner.
//
// In the fixture the funder paid 10.5 GNOT in and took nothing out, and the
// payee took 1.5 GNOT out and paid nothing in. So the funder is down and the
// payee is up, on a realm whose own balance went up.

const gnot = ugnot => (ugnot / 1000000).toString();
// The page abbreviates an address to its first eight characters, an ellipsis
// and its last four, so the whole string never appears in the DOM. Both fixture
// addresses end in the same four zeroes, which is why the prefix is what
// identifies a row.
const shown = addr => addr.slice(0, 8);

const FUNDED = TAB_TRANSFERS
  .filter(t => t.from === TAB_FUNDER).reduce((s, t) => s + t.amount, 0);
const PAID = TAB_TRANSFERS
  .filter(t => t.to === TAB_PAYEE).reduce((s, t) => s + t.amount, 0);

async function openDefi(page) {
  await page.goto(`/realm/${HUB_ROUTE}?network=alpha&tab=defi`);
  await settle(page);
  await expect(page.locator('#tab-defi')).toHaveClass(/active/);
}

test('the counterparty table nets from the counterparty’s side, not the realm’s', async ({ page }) => {
  const seen = watch(page);
  await openDefi(page);

  const table = page.locator('#defi-counterparties');
  await expect(table).toBeVisible();

  // Two counterparties over five legs. The whole point of the collapse: the
  // table below this one has five rows and says nothing about who.
  await expect(table.locator('tbody tr')).toHaveCount(2);

  const funder = table.locator('tr', { hasText: shown(TAB_FUNDER) });
  await expect(funder).toContainText(gnot(FUNDED));
  // Down: it put money in and took none out.
  await expect(funder).toContainText(`-${gnot(FUNDED)}`);

  const payee = table.locator('tr', { hasText: shown(TAB_PAYEE) });
  await expect(payee).toContainText(`+${gnot(PAID)}`);

  // Sorted by net, best first, so the row order is itself the leaderboard.
  const firstRow = table.locator('tbody tr').first();
  await expect(firstRow).toContainText(shown(TAB_PAYEE));

  expect(seen.jsErrors, 'uncaught exceptions').toEqual([]);
  expect(unexpected(seen.consoleErrors), 'console errors').toEqual([]);
});

test('the graph draws, and its mode is in the URL', async ({ page }) => {
  const seen = watch(page);
  await openDefi(page);

  const box = page.locator('#defi-flow-graph');
  await expect(box).toBeVisible();
  // SVG, like the usage treemap: a dozen nodes is not a series with thousands
  // of points, and in SVG the node labels are real text a test can read.
  await expect(box.locator('svg')).toBeVisible();

  // Combined is the default and says so in words a reader can check against
  // the table: every transfer, not just the page.
  const note = page.locator('#tab-defi .detail-chart-note', { hasText: 'counterparties' });
  await expect(note).toContainText(`over all ${TAB_TRANSFERS.length} transfers`);

  await page.locator('#tab-defi .dash-seg button', { hasText: 'detail' }).click();
  await expect(page).toHaveURL(/[?&]flow=detail/);
  await expect(page.locator('#defi-flow-graph svg')).toBeVisible();
  await expect(page.locator('#tab-defi .detail-chart-note', { hasText: 'drawn individually' }))
    .toContainText(`${TAB_TRANSFERS.length} transfer legs`);

  // A mode is a link: reloading has to land back in detail rather than
  // silently resetting, which is the convention the rest of the site follows.
  await page.reload();
  await settle(page);
  await expect(page.locator('#tab-defi .dash-seg button.on')).toHaveText('detail');

  expect(seen.jsErrors, 'uncaught exceptions').toEqual([]);
  expect(unexpected(seen.consoleErrors), 'console errors').toEqual([]);
});

// Switching modes twice used to be the way to leave an ECharts instance behind
// on a detached node: the toggle rebuilds the whole tab. The assertion is that
// there is exactly one graph afterwards, not two stacked.
test('toggling the mode does not leave a second graph behind', async ({ page }) => {
  const seen = watch(page);
  await openDefi(page);

  for (const mode of ['detail', 'combined', 'detail']) {
    await page.locator('#tab-defi .dash-seg button', { hasText: mode }).click();
    await settle(page);
  }
  await expect(page.locator('#defi-flow-graph')).toHaveCount(1);
  await expect(page.locator('#defi-flow-graph svg')).toHaveCount(1);
  await expect(page.locator('#defi-counterparties')).toHaveCount(1);

  expect(seen.jsErrors, 'uncaught exceptions').toEqual([]);
  expect(unexpected(seen.consoleErrors), 'console errors').toEqual([]);
});

test('the API collapses every leg, not the page the table shows', async ({ request }) => {
  const res = await request.get(`/api/realm/defi/${HUB_ROUTE}?network=alpha&flows_limit=1`);
  expect(res.status()).toBe(200);
  const body = await res.json();

  // One leg in the table, and still both counterparties with their full
  // totals: a per-counterparty figure summed over one page of several would be
  // wrong for exactly the heaviest counterparty, which is the one being read.
  expect(body.flows_shown).toBe(1);
  expect(body.flows_total).toBe(TAB_TRANSFERS.length);
  expect(body.counterparties_total).toBe(2);

  const byAddr = Object.fromEntries(body.counterparties.map(c => [c.address, c]));
  expect(byAddr[TAB_FUNDER].sent).toBe(FUNDED);
  expect(byAddr[TAB_FUNDER].received).toBe(0);
  expect(byAddr[TAB_FUNDER].net).toBe(-FUNDED);
  expect(byAddr[TAB_PAYEE].received).toBe(PAID);
  expect(byAddr[TAB_PAYEE].net).toBe(PAID);
});

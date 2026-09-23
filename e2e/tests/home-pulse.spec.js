import { expect, test } from '@playwright/test';

import {
  FRESH_DEV, FRESH_LIB, FRESH_REALM, FRESH_VETERAN,
} from '../harness/fixture.mjs';
import { settle, unexpected, watch } from './helpers.js';

// The home page's windowed panels, over /api/pulse.
//
// Every one of these is empty on a chain with nothing recent, and an empty
// table renders without a console error — so "the page loaded" proves nothing
// here, the same gap home.spec.js exists to close for the all-time panels.
//
// What makes a window assertion possible at all is the fixture's recent tail
// (harness/fixture.mjs): its own library, realms, deployer, callers and token,
// all stamped inside the last few hours, beside an otherwise ancient chain.
// Every identity below comes from there, so these assertions cannot be
// satisfied by some other part of the fixture drifting into the window.
async function openHome(page, win) {
  await page.goto('/?network=alpha' + (win ? '&window=' + win : ''));
  await settle(page);
}

// A populated row has one cell per column; the empty state is a single cell
// spanning the table. Telling them apart is the whole point.
async function expectPopulated(page, id, columns) {
  const rows = page.locator('#' + id + ' tr');
  await expect.poll(() => rows.count(), {
    message: id + ' should have rows, not just a heading',
  }).toBeGreaterThan(0);
  const cells = await rows.first().locator('td').count();
  expect(cells, id + ' shows the empty-state placeholder').toBe(columns);
}

test('the window selector drives the page and lives in the URL', async ({ page }) => {
  const seen = watch(page);
  await openHome(page);

  // The default is not written into the URL, so a bare / is the bare default.
  expect(new URL(page.url()).searchParams.get('window')).toBeNull();
  await expect(page.locator('#home-window-group button.active')).toHaveText('24h');

  await page.locator('#home-window-group button', { hasText: '7d' }).click();
  await settle(page);
  await expect.poll(() => new URL(page.url()).searchParams.get('window')).toBe('7d');
  await expect(page.locator('#home-window-group button.active')).toHaveText('7d');

  // The window says when it closed, because this endpoint is cached and may be
  // served stale: without it "last 7 days" cannot be checked.
  await expect(page.locator('#home-window-range')).toContainText(/ago|just now/);

  // A reload lands on the same window rather than snapping back to the default.
  await page.reload();
  await settle(page);
  await expect(page.locator('#home-window-group button.active')).toHaveText('7d');

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.consoleErrors)).toEqual([]);
});

test('a hand-edited window falls back rather than emptying the page', async ({ page }) => {
  const seen = watch(page);
  await openHome(page, '42y');

  await expect(page.locator('#home-window-group button.active')).toHaveText('24h');
  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.consoleErrors)).toEqual([]);
});

test('the tiles carry the window under the all-time figure', async ({ page }) => {
  const seen = watch(page);
  await openHome(page);

  // The total is the anchor; the line under it is what tells a reader whether
  // the anchor moved. Before this the page was ten all-time totals and a chain
  // asleep for a week looked exactly like one that was not.
  await expect(page.locator('#stat-calls')).not.toHaveText('-');
  await expect(page.locator('#delta-calls')).toContainText(/^\+?\d/);
  await expect(page.locator('#delta-deploys')).toContainText(/^\+?\d/);

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.consoleErrors)).toEqual([]);
});

test('hot realms rank what was called in the window', async ({ page }) => {
  const seen = watch(page);
  await openHome(page);

  await expectPopulated(page, 'hot-realms', 6);
  await expect(page.locator('#hot-realms')).toContainText(FRESH_REALM.replace('gno.land', ''));
  // Six calls today against three yesterday, so the comparison column is a
  // percentage rather than the "new" badge a quiet previous window produces.
  await expect(page.locator('#hot-realms tr').first()).toContainText(/%|×/);

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.consoleErrors)).toEqual([]);
});

test('hot assets count ugnot beside the GRC20 tokens, and say which ledger each came from', async ({ page }) => {
  const seen = watch(page);
  await openHome(page);

  await expectPopulated(page, 'hot-tokens', 6);
  // The chain's own coin does not emit a Transfer event. Leaving it out of a
  // list headed "hot assets" would be the most misleading thing on the page,
  // so it is folded in from bank sends and marked as such.
  await expect(page.locator('#hot-tokens')).toContainText('ugnot');
  await expect(page.locator('#hot-tokens')).toContainText('bank sends');
  await expect(page.locator('#hot-tokens')).toContainText('freshcoin');

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.consoleErrors)).toEqual([]);
});

test('a transfer into a realm names the realm, not its hashed address', async ({ page }) => {
  const seen = watch(page);
  await openHome(page);

  await expectPopulated(page, 'hot-flows', 6);
  // The tail's payout is sent *from* the realm's own account, which is a hash
  // of its path and appears in no table. Rendering it as the realm is the whole
  // reason the reverse derivation exists.
  await expect(page.locator('#hot-flows')).toContainText(FRESH_REALM.replace('gno.land', ''));
  // An end with no address at all is the chain itself, minting or burning.
  // Drawn as a blank cell it would read as missing data.
  await expect(page.locator('#hot-flows')).toContainText('mint');

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.consoleErrors)).toEqual([]);
});

test('hot developers name who shipped and what they shipped', async ({ page }) => {
  const seen = watch(page);
  await openHome(page);

  await expectPopulated(page, 'hot-devs', 5);
  await expect(page.locator('#hot-devs')).toContainText(FRESH_DEV.slice(0, 8));
  // FRESH_DEV had never deployed before; FRESH_VETERAN had. Only one of them
  // may wear the badge, and getting that backwards is the failure this catches.
  const dev = page.locator('#hot-devs tr', { hasText: FRESH_DEV.slice(0, 8) });
  await expect(dev).toContainText('first deploy');
  const veteran = page.locator('#hot-devs tr', { hasText: FRESH_VETERAN.slice(0, 8) });
  await expect(veteran).not.toContainText('first deploy');
  // A deployer row without paths is a number nobody can act on.
  await expect(page.locator('#hot-devs')).toContainText('/r/');

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.consoleErrors)).toEqual([]);
});

test('hot libraries rank on-chain packages and drop the standard library', async ({ page }) => {
  const seen = watch(page);
  await openHome(page);

  await expectPopulated(page, 'hot-libs', 4);
  await expect(page.locator('#hot-libs')).toContainText(FRESH_LIB.replace('gno.land', ''));
  // Two of the window's new packages import it, three packages do all told.
  // Printing only the first would make every library look equally adopted.
  const lib = page.locator('#hot-libs tr', { hasText: 'fresh/kit' });
  await expect(lib.locator('td').nth(1)).toHaveText('2');
  await expect(lib.locator('td').nth(2)).toHaveText('3');
  // std, strings and testing would take the top three places on every chain in
  // every window, and a list whose answer never changes says nothing.
  const first = await page.locator('#hot-libs tr').first().locator('td').first().innerText();
  expect(['std', 'strings', 'testing']).not.toContain(first.trim());

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.consoleErrors)).toEqual([]);
});

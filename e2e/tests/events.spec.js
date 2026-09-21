import { expect, test } from '@playwright/test';

import { HUB_ROUTE } from '../harness/fixture.mjs';
import { TAB_EVENTS } from '../harness/fake-indexer.mjs';
import { settle, unexpected, watch } from './helpers.js';

// StorageDepositEvent is the chain's bookkeeping, not the realm's vocabulary:
// one is posted per state-changing transaction, it carries no attributes, and
// the storage tab already reads the same numbers as before/delta/after. Listed
// beside real events it doubles the table with a row saying nothing, which on
// a busy realm is half the page.
//
// The fixture gives every event transaction one GnoEvent and one storage
// deposit, the pair mainnet actually returns, so "hidden" and "dropped by the
// backend" are distinguishable here.

const gnoRows = TAB_EVENTS.length;

test('the realm events tab hides storage events, and the toggle brings them back', async ({ page }) => {
  const seen = watch(page);
  await page.goto(`/realm/${HUB_ROUTE}?network=alpha&tab=events`);
  await settle(page);

  const rows = page.locator('#tab-events table tbody tr');
  await expect(rows).toHaveCount(gnoRows);
  await expect(page.locator('#tab-events table')).not.toContainText('StorageDepositEvent');

  // The toggle has to say how many rows it would add, or it is a checkbox with
  // no stated consequence.
  const toggle = page.locator('#tab-events label', { hasText: 'storage events' });
  await expect(toggle).toContainText(String(gnoRows));

  await toggle.locator('input[type=checkbox]').check();
  await expect(rows).toHaveCount(gnoRows * 2);
  await expect(page.locator('#tab-events table')).toContainText('StorageDepositEvent');

  // And back, rather than a one-way reveal.
  await toggle.locator('input[type=checkbox]').uncheck();
  await expect(rows).toHaveCount(gnoRows);

  expect(unexpected(seen.failedRequests)).toEqual([]);
  expect(seen.jsErrors).toEqual([]);
});

// Not asserted here: the chain-wide /events feed and the transaction page,
// which drop storage events upstream of the DOM (gnoEvents in api_chain.go and
// the GnoEvent filter in loadTxDetail). TestGnoEvents covers the first; the
// fake indexer answers the chain-wide query with nothing, so a browser
// assertion on it would pass against an empty table and prove nothing.

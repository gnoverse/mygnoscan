import { expect, test } from '@playwright/test';

import { HUB_ROUTE } from '../harness/fixture.mjs';
import { settle, unexpected, watch } from './helpers.js';

// Every package owns two accounts, and neither is stored anywhere: both are
// hashes of the path, computed server-side in pkg/gnoaddr. These are the
// expected values for the fixture's hub realm, pinned here so a derivation that
// starts producing plausible-looking nonsense fails in the browser too and not
// only in the Go test.
const HUB_ADDRESS = 'g1qql00vm7xf0mydz74md9c57tuv34znm8wm9nxu';
const HUB_STORAGE_DEPOSIT = 'g1pw5cc2863d22uu8l04xskj74ec2ldfq8fzuyv4';

// The fixture funds neither of them, which is the case worth a test: the whole
// point of deriving rather than looking up is that the rows appear on a realm
// nobody has ever sent a coin to.
test('the info tab derives both of a realm’s accounts, unfunded', async ({ page }) => {
  const seen = watch(page);
  await page.goto(`/realm/${HUB_ROUTE}?network=alpha`);
  await settle(page);

  const info = page.locator('#tab-info');
  await expect(info).toHaveClass(/active/);

  const row = label => info.locator('> table tr').filter({
    has: page.locator(`td:text-is("${label}")`),
  });
  await expect(row('address')).toContainText(HUB_ADDRESS);
  await expect(row('storage deposit')).toContainText(HUB_STORAGE_DEPOSIT);

  // Shown in full rather than truncated, with the copy button beside them: a
  // partial address is useful for neither funding nor watching, which are the
  // two reasons to want it.
  await expect(row('address').locator('.copy-btn')).toHaveCount(1);

  // And each links to its own account page, so the derivation is not a dead end.
  await row('address').locator('a').first().click();
  await settle(page);
  expect(page.url()).toContain(`/address/${HUB_ADDRESS}`);

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.consoleErrors)).toEqual([]);
});

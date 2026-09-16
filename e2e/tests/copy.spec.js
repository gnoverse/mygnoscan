import { expect, test } from '@playwright/test';

import { BUSY_CALLER } from '../harness/fixture.mjs';
import { settle, watch } from './helpers.js';

// Copy-to-clipboard, asserted by reading the clipboard rather than by watching
// the button change colour.
//
// The distinction matters: the confirmation tick is drawn by the same handler
// that does the copying, so a button that shows "copied" while writing nothing
// passes any assertion made on its appearance. A copy button that silently does
// nothing is worse than no copy button — the reader pastes stale content and
// has no reason to suspect it.
test.use({ permissions: ['clipboard-read', 'clipboard-write'] });

test('the address header copies the full address', async ({ page }) => {
  const seen = watch(page);
  await page.goto(`/address/${BUSY_CALLER}`);
  await settle(page);

  const button = page.locator('button.copy-btn').first();
  await expect(button).toBeVisible();
  await button.click();

  const clipboard = await page.evaluate(() => navigator.clipboard.readText());
  expect(clipboard, 'the clipboard should hold the complete address').toBe(BUSY_CALLER);

  expect(seen.jsErrors, 'copying should not throw').toEqual([]);
});

// The hex rendering of a tx hash (#171). Asserted against the exact pair from
// production — the hash gnoscan.io displayed for a mainnet transaction and the
// base64 form our own indexer stores — because that pairing is the whole point
// of the feature, and a self-consistent round-trip would prove nothing.
test('a base64 tx hash renders as the hex form other explorers print', async ({ page }) => {
  await page.goto('/');
  await settle(page);

  const hex = await page.evaluate(() => hexHash('e6ChL6Trihr1GABwAWTvGOAkCGtNtvfhr4ZkoixrBAg='));
  expect(hex).toBe('7BA0A12FA4EB8A1AF51800700164EF18E024086B4DB6F7E1AF8664A22C6B0408');

  // Not a hash: handed back as empty rather than as garbage, so the caller can
  // decide to omit the row entirely.
  const junk = await page.evaluate(() => hexHash('!!!not base64!!!'));
  expect(junk).toBe('');
});

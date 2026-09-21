import { expect, test } from '@playwright/test';

import { settle, unexpected, watch } from './helpers.js';

// The assets page, over the GRC20 transfer ledger.
//
// The three rows below are the three shapes mainnet actually contains, and each
// one would render a plausible lie if the page assumed the common case.

const ASSETS = {
  network: 'alpha',
  assets: [
    {
      token: 'gno.land/r/gnoswap/gns.GNS.0000000', pkg_path: 'gno.land/r/gnoswap/gns', symbol: 'GNS',
      network: 'alpha', supply: 100393107865894, holders: 119, fungible: true,
      transfers: 1818, transfers_24h: 12, first_seen_time: '2026-09-01T00:00:00Z',
      verified: true, display_symbol: 'GNS', display_name: 'GnoSwap', decimals: 6,
    },
    {
      // GRC721 goes through the same events without an amount.
      token: 'gno.land/r/gnoswap/gnft.GNFT.0000000', pkg_path: 'gno.land/r/gnoswap/gnft', symbol: 'GNFT',
      network: 'alpha', supply: 0, holders: 0, fungible: false, transfers: 201, transfers_24h: 0,
      verified: false,
    },
    {
      // A token that emits a bare symbol instead of its realm path.
      token: 'COVID', pkg_path: '', symbol: 'COVID',
      network: 'alpha', supply: 615028450, holders: 15, fungible: true, transfers: 66, transfers_24h: 0,
      verified: false,
    },
  ],
};

async function stubAssets(page) {
  await page.route('**/api/assets*', route =>
    route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(ASSETS) }));
}

test('an asset shows supply, holders and whether anyone vouched for it', async ({ page }) => {
  const seen = watch(page);
  await stubAssets(page);

  const response = await page.goto('/tokens');
  expect(response.status()).toBe(200);
  await settle(page);

  const list = page.locator('#tokens-list');
  await expect(list).toContainText('GNS');
  await expect(list).toContainText('100,393,107,865,894');
  await expect(list).toContainText('119');
  // The badge is the registry's claim, and the tooltip says so.
  await expect(list.locator('.badge-ok').first()).toContainText('verified');

  expect(seen.jsErrors, 'uncaught exceptions').toEqual([]);
  expect(unexpected(seen.consoleErrors), 'console errors').toEqual([]);
});

test('an asset whose transfers carry no amount says n/a, not zero', async ({ page }) => {
  const seen = watch(page);
  await stubAssets(page);

  await page.goto('/tokens');
  await settle(page);

  // A supply of 0 and 0 holders would read as "this token is empty". It means
  // the arithmetic does not apply, and the row has to distinguish the two.
  const nftRow = page.locator('#tokens-list tr', { hasText: 'GNFT' });
  await expect(nftRow).toContainText('n/a');
  // Its transfers are real and still counted.
  await expect(nftRow).toContainText('201');

  expect(seen.jsErrors, 'uncaught exceptions').toEqual([]);
});

test('an asset that emits a bare symbol admits its realm is unknown', async ({ page }) => {
  const seen = watch(page);
  await stubAssets(page);

  await page.goto('/tokens');
  await settle(page);

  // Printing "COVID" under a column headed "realm" would be inventing a path.
  const row = page.locator('#tokens-list tr', { hasText: 'COVID' });
  await expect(row).toContainText('unknown');
  // Its supply is still real: only the realm is missing.
  await expect(row).toContainText('615,028,450');

  expect(seen.jsErrors, 'uncaught exceptions').toEqual([]);
});

test('the page refuses to show a price', async ({ page }) => {
  const seen = watch(page);
  await stubAssets(page);

  await page.goto('/tokens');
  await settle(page);

  // Not a style preference: GNOT is unlisted and there is no oracle on chain,
  // so any money column here would be invented. The page says why.
  await expect(page.locator('.view.active')).toContainText('no price or market value');

  expect(seen.jsErrors, 'uncaught exceptions').toEqual([]);
});

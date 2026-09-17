import { expect, test } from '@playwright/test';

import { BUSY_CALLER } from '../harness/fixture.mjs';
import { settle } from './helpers.js';

// Identicons are generated in-page, never fetched.
//
// An avatar service would mean telling a third party every address anyone looks
// at, which on a block explorer is the whole browsing history — plus a CSP
// exception, a round-trip per row, and a dependency that can go down.
test('identicons are deterministic and need no network', async ({ page }) => {
  // Image and avatar traffic specifically. The page does load d3 and Chart.js
  // from CDNs; what must never happen is an address leaving the browser to
  // fetch a picture of itself.
  const avatarish = [];
  page.on('request', r => {
    const u = r.url().toLowerCase();
    const isAsset = r.resourceType() === 'image';
    if (isAsset || /gravatar|avatar|identicon|blockie|jazzicon/.test(u)) avatarish.push(r.url());
  });

  await page.goto(`/address/${BUSY_CALLER}`);
  await settle(page);

  expect(avatarish, 'an address must never leave the browser to fetch a picture of itself').toEqual([]);

  // Same address, same image — twice, and across a reload.
  const a = await page.evaluate(() => identiconSVG('g1manfred47kzduec920z88wfr64ylksmdcedlf5', 14));
  const b = await page.evaluate(() => identiconSVG('g1manfred47kzduec920z88wfr64ylksmdcedlf5', 14));
  expect(a).toBe(b);

  await page.reload();
  await settle(page);
  const c = await page.evaluate(() => identiconSVG('g1manfred47kzduec920z88wfr64ylksmdcedlf5', 14));
  expect(c, 'the same account must look the same on every visit').toBe(a);
});

// Two addresses differing only in their last character must not collide: the
// whole point is noticing that an address is not the one you expected.
test('similar addresses produce different identicons', async ({ page }) => {
  await page.goto('/');
  await settle(page);

  const [x, y] = await page.evaluate(() => [
    identiconSVG('g1manfred47kzduec920z88wfr64ylksmdcedlf5', 14),
    identiconSVG('g1manfred47kzduec920z88wfr64ylksmdcedlf6', 14),
  ]);
  expect(x).not.toBe(y);
});

// Only real addresses get one. A moniker or a realm path must not sprout a
// meaningless block of colour.
test('non-addresses get no identicon', async ({ page }) => {
  await page.goto('/');
  await settle(page);

  const results = await page.evaluate(() => [
    identicon('gno.land/r/demo/boards'),
    identicon(''),
    identicon('not-an-address'),
    identicon('g1manfred47kzduec920z88wfr64ylksmdcedlf5') ? 'yes' : null,
  ].map(v => (v === null ? null : typeof v === 'string' ? v : 'el')));
  expect(results).toEqual([null, null, null, 'yes']);
});

// Every address link in a real listing carries one.
test('address links in a listing show identicons', async ({ page }) => {
  await page.goto('/accounts');
  await settle(page);

  const count = await page.locator('#accounts-list .identicon svg').count();
  expect(count, 'accounts rows should each carry an identicon').toBeGreaterThan(0);
});

// Golden values from the reference implementation (ethereum/blockies), which is
// where Etherscan's and MetaMask's icons come from.
//
// Using that algorithm is the point: the same account looks the same here as it
// does in the tools people already have open. That only holds while the output
// is bit-identical, and it is easy to break by accident — the `>>> 0` unsigned
// casts in the PRNG and the order the three colours are drawn in both change
// every icon if touched, while still producing something deterministic and
// plausible-looking. This is the test that notices.
test('output matches the reference blockies implementation', async ({ page }) => {
  await page.goto('/');
  await settle(page);

  const got = await page.evaluate(() => {
    identiconSeedrand('0xbaca1b5685bd1bb496843d9deed623cc87f6a154');
    const c = identiconColor(), bg = identiconColor(), sp = identiconColor();
    return { grid: identiconData(8).join(''), c, bg, sp };
  });

  expect(got.grid).toBe('1001100110000001011001101110011110111101100000011121121120211202');
  // Colours are rounded for compactness; compare the hue and rounded channels.
  const norm = v => {
    const [h, s, l] = v.replace('hsl(', '').replace(')', '').split(',');
    return [h, Math.round(parseFloat(s)), Math.round(parseFloat(l))].join(',');
  };
  expect(norm(got.c)).toBe(norm('hsl(259,62.590167010203004%,45.154170016758144%)'));
  expect(norm(got.bg)).toBe(norm('hsl(48,73.65404821932316%,48.55413617333397%)'));
  expect(norm(got.sp)).toBe(norm('hsl(339,69.52172773890197%,62.8553383750841%)'));
});

// Every page that shows an address shows its identicon.
//
// The first pass only wired this into addrLink, which missed the address page's
// own header — the one page that is *about* an account had no icon for it. This
// walks the routes rather than trusting that one call site covers everything.
const ADDRESS_PAGES = [
  ['home', '/'],
  ['accounts', '/accounts'],
  ['blocks', '/blocks'],
  ['realms', '/realms'],
  ['validators', '/validators'],
];

for (const [name, path] of ADDRESS_PAGES) {
  test(`${name} shows identicons beside its addresses`, async ({ page }) => {
    await page.goto(path);
    await settle(page);
    const icons = await page.locator('.identicon svg').count();
    expect(icons, `${path} renders addresses, so it should render their icons`).toBeGreaterThan(0);
  });
}

// The address page is about one account, so its header carries a larger icon —
// the same image as the small ones, not a second scheme.
test('the address page header carries its own identicon', async ({ page }) => {
  await page.goto(`/address/${BUSY_CALLER}`);
  await settle(page);

  const header = page.locator('#address-detail-content > div').filter({ hasText: BUSY_CALLER.slice(0, 12) }).first();
  await expect(header.locator('.identicon svg')).toBeVisible();

  const size = await header.locator('.identicon svg').getAttribute('width');
  expect(Number(size), 'the page subject deserves a bigger mark than a table row').toBeGreaterThan(14);
});

// One predicate decides what an address looks like. There used to be four
// slightly different regexes, which is how the same account could get an icon
// in a table and not in a realm path.
test('the address test is shared, not re-invented per call site', async ({ page }) => {
  await page.goto('/');
  await settle(page);

  const r = await page.evaluate(() => ({
    addr: looksLikeAddress('g1manfred47kzduec920z88wfr64ylksmdcedlf5'),
    short: looksLikeAddress('g1busycaller0000000000000000000000000'),
    path: looksLikeAddress('gno.land/r/demo/boards'),
    empty: looksLikeAddress(''),
    nonString: looksLikeAddress(null),
  }));
  expect(r).toEqual({ addr: true, short: true, path: false, empty: false, nonString: false });
});

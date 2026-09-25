import { test, expect } from '@playwright/test';
import { watch, unexpected } from './helpers.js';

// The results popup used to be exactly as wide as the box it hangs from
// (280px), with each result's path in a single element. A package path is
// gno.land/r/<40-character address>/<name>, so every row was cut off at its
// right edge — the end, which is the only part that tells two of one deployer's
// realms apart. Searching "bubble" on mainnet returned ".../bubblerumble" and
// ".../bubblerumble2" as two rows that rendered identically.
//
// So the popup is now sized for a path rather than for the input, and the path
// is two elements: a namespace that gives up its room first and elides, and a
// name that does not shrink until there is nothing else left to take.

const TERM = 'util0';

async function openResults(page) {
  const input = page.locator('#search-input');
  await input.click();
  await input.fill(TERM);
  await expect(page.locator('#search-results .search-result').first()).toBeVisible({ timeout: 15_000 });
  return page.locator('#search-results .search-result');
}

test('the popup is sized for a realm path, not for the input above it', async ({ page }) => {
  const seen = watch(page);
  await page.goto('/');
  const rows = await openResults(page);
  expect(await rows.count()).toBeGreaterThan(1);

  const { popup, input } = await page.evaluate(() => ({
    popup: document.getElementById('search-results').getBoundingClientRect().width,
    input: document.getElementById('search-input').getBoundingClientRect().width,
  }));
  // Wide enough for gno.land/r/<40 characters>/<name> plus the badge beside it.
  expect(popup).toBeGreaterThan(600);
  expect(popup).toBeGreaterThan(input * 2);

  expect(unexpected(seen.failedRequests)).toEqual([]);
  expect(seen.jsErrors).toEqual([]);
});

test('every result shows its own name in full, so near-identical paths differ', async ({ page }) => {
  await page.goto('/');
  const rows = await openResults(page);

  const names = await page.locator('#search-results .search-result-name').allInnerTexts();
  expect(names.length).toBeGreaterThan(1);
  // The fixture's util00…util11 differ only in their last character, which is
  // exactly the case the old popup erased.
  expect(new Set(names).size).toBe(names.length);

  // Nothing is elided when there is room for it: no result's name is clipped.
  const clipped = await page.locator('#search-results .search-result-name').evaluateAll(
    els => els.filter(e => e.scrollWidth > e.clientWidth + 1).map(e => e.textContent),
  );
  expect(clipped).toEqual([]);

  // The full path stays reachable on hover regardless.
  await expect(page.locator('#search-results .search-result-title').first()).toHaveAttribute(
    'title', /^gno\.land\//,
  );
});

test('when it genuinely does not fit, the namespace elides and the name survives', async ({ page }) => {
  await page.goto('/');
  // Forced rather than reached by shrinking the viewport: what is being
  // asserted is the shrink *order* between the two halves, and pinning it to a
  // width makes the test depend on the font the browser happened to pick.
  await page.addStyleTag({ content: '#search-results { width: 220px !important; }' });
  await openResults(page);

  const first = page.locator('#search-results .search-result').first();
  const overflow = el => el.scrollWidth > el.clientWidth + 1;
  expect(await first.locator('.search-result-prefix').evaluate(overflow)).toBe(true);
  expect(await first.locator('.search-result-name').evaluate(overflow)).toBe(false);
});

// The group order is the answer to a real complaint: typing "moul" on mainnet
// led with the MOULTEST token, and no query on any chain could return a user at
// all. Users, then assets, then realms, then symbols, then packages.
//
// The fixture registers `hub` to HUB's own deployer, which is also the namespace
// of a realm (r/hub/core) and a package (p/hub/toolkit), so one query exercises
// four of the five groups at once and the order between them is observable.
// Symbols are the fifth and need their own query, below: no declaration in the
// fixture's library is named after a namespace.
test('a user leads the results, and realms are their own group above packages', async ({ page }) => {
  const seen = watch(page);
  await page.goto('/');
  const input = page.locator('#search-input');
  await input.click();
  await input.fill('hub');
  await expect(page.locator('#search-results .search-result').first()).toBeVisible({ timeout: 15_000 });

  // Lowercased because the label is uppercased in CSS, and allInnerTexts reads
  // what is rendered rather than what the DOM holds.
  const labels = (await page.locator('#search-results .search-section-label').allInnerTexts())
    .map(s => s.toLowerCase());
  // The fixture's one token issuer is the hub realm itself, so this query
  // reaches four of the five groups and pins the whole order in one assertion.
  expect(labels).toEqual(['users', 'grc20 assets', 'realms', 'packages']);

  // The exact registration leads its own group, ahead of the namesake that has
  // deployed nothing.
  const users = page.locator('#search-results .search-result').filter({ hasText: '@hub' });
  await expect(users.first().locator('.search-result-name')).toHaveText('@hub');

  // A tombstoned name is still listed and says so: r/sys/users never frees one,
  // and dropping the row would claim the name is available.
  await expect(page.locator('#search-results .search-result-sub')
    .filter({ hasText: 'deleted' })).toHaveCount(1);

  expect(unexpected(seen.failedRequests)).toEqual([]);
  expect(seen.jsErrors).toEqual([]);
});

// Clicking a user goes to the address page, which is where everything else
// about that account already lives. A name with no packages has no realm page to
// land on, and 67 of mainnet's 78 registrations are in exactly that state.
test('a user result routes to the address page', async ({ page }) => {
  await page.goto('/');
  const input = page.locator('#search-input');
  await input.click();
  await input.fill('hubbot');
  const row = page.locator('#search-results .search-result').filter({ hasText: '@hubbot' }).first();
  await expect(row).toBeVisible({ timeout: 15_000 });
  await row.click();
  await expect(page).toHaveURL(/\/address\/g1hubbot/);
});

// Symbols sit above packages, which is where they were before the user group
// existed. The argument is only ever about these two: somebody typing a
// CamelCase identifier wants the declaration, not the paths whose text happens
// to contain it. It is a separate test because no one fixture query reaches all
// five groups.
//
// `re` is the query that reaches four: it is inside `Registry`, `Reset` and
// `Tree` in the library's source, inside `gno.land/r/fresh/*` and
// `gno.land/p/fresh/kit`, and inside the `freshcoin` token key. It matches no
// registered name, which is why `hub` is still the test above.
test('symbols rank above packages, and below the realms and assets a name means', async ({ page }) => {
  const seen = watch(page);
  await page.goto('/');
  const input = page.locator('#search-input');
  await input.click();
  await input.fill('re');
  await expect(page.locator('#search-results .search-result').first()).toBeVisible({ timeout: 15_000 });

  const labels = (await page.locator('#search-results .search-section-label').allInnerTexts())
    .map(s => s.toLowerCase());
  expect(labels).toContain('symbols');
  expect(labels).toContain('packages');
  // The pair this test exists for, asserted as neighbours in the rendered
  // order rather than as two independent indexes.
  expect(labels.indexOf('symbols')).toBeLessThan(labels.indexOf('packages'));
  // And it does not climb past the two groups that answer "what can I open".
  for (const above of ['grc20 assets', 'realms']) {
    if (labels.includes(above)) {
      expect(labels.indexOf(above)).toBeLessThan(labels.indexOf('symbols'));
    }
  }

  expect(unexpected(seen.failedRequests)).toEqual([]);
  expect(seen.jsErrors).toEqual([]);
});

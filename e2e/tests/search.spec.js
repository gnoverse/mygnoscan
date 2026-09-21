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

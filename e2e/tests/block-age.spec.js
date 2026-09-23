import { expect, test } from '@playwright/test';

import { BUSY_CALLER, HUB_ROUTE } from '../harness/fixture.mjs';
import { settle, unexpected, watch } from './helpers.js';

// A block height printed on its own answers "which block" and leaves "when" to
// a second page load, which is the question a reader of a table actually had.
// So every table that prints a height prints its age beside it, and the shape
// is always the same: `1,234 (3d)`, drawn by blockWithAge.
//
// The columns deliberately not covered here are the ones that already carry a
// timestamp of their own: /blocks and the home transaction feed both have a
// "time"/"when" column, and an age next to the height there is a second way of
// saying the same thing.
const AGE = /^\(\d+(s|m|h|d|mo|y)\)$/;

// Reads one column of a table back out of the DOM, found by its header rather
// than by index: the column moves whenever a table gains one, and a test
// pinned to position 6 would then assert about a different column and still
// pass. The selector may match several tables (or a tbody inside one); the
// table carrying the wanted header is the one meant.
async function column(page, selector, header) {
  return page.evaluate(([sel, want]) => {
    const tables = [...new Set([...document.querySelectorAll(sel)]
      .map(n => (n.tagName === 'TABLE' ? n : n.closest('table')))
      .filter(Boolean))];
    if (!tables.length) return { error: 'no table matched ' + sel };
    const seen = [];
    for (const table of tables) {
      const heads = [...table.querySelectorAll('thead th')].map(th => th.textContent.trim());
      seen.push(heads.join('/'));
      const col = heads.indexOf(want);
      if (col < 0) continue;
      const cells = [...table.querySelectorAll('tbody tr')].map(tr => {
        const td = tr.children[col];
        if (!td) return null;
        const age = td.querySelector('[data-age]');
        return { text: td.textContent.trim(), age: age ? age.textContent.trim() : null };
      }).filter(Boolean);
      return { cells };
    }
    return { error: 'no "' + want + '" column; tables matched were: ' + seen.join(' | ') };
  }, [selector, header]);
}

// Every table in the fixture that prints a height, and which column it is in.
// The realm detail's calls tab is the one this was opened for: it listed a
// realm's whole history against bare block numbers.
const TABLES = [
  ['the realm calls tab', `/realm/${HUB_ROUTE}?network=alpha&tab=calls`, '#tab-calls table', 'block'],
  ['the packages list', '/packages?network=alpha', '#packages-list', 'block'],
  ['the realms list', '/realms?network=alpha', '#realms-list', 'added'],
  ['the storage table', '/storage?network=alpha', '#storage-content table', 'first claim'],
  // The home page's two windowed tables. Neither has a timestamp column of its
  // own, so the age beside the height is the only "when" either one carries.
  // The fixture's recent tail is what puts rows in them at the default window.
  ['the hot realms table', '/?network=alpha', '#hot-realms', 'last call'],
  ['the notable transfers table', '/?network=alpha', '#hot-flows', 'block'],
];

for (const [name, url, selector, header] of TABLES) {
  test(`${name} dates every block it prints`, async ({ page }) => {
    const seen = watch(page);
    await page.goto(url);
    await settle(page);

    const { error, cells } = await column(page, selector, header);
    expect(error, error).toBeUndefined();
    expect(cells.length, 'no rows to assert on').toBeGreaterThan(0);

    for (const cell of cells) {
      // A row with no height at all (a realm never called, a placeholder) has
      // nothing to date and is not a failure.
      if (!/\d/.test(cell.text)) continue;
      expect(cell.age, `"${cell.text}" has a height and no age`).not.toBeNull();
      expect(cell.age).toMatch(AGE);
    }

    expect(seen.jsErrors).toEqual([]);
    expect(unexpected(seen.consoleErrors)).toEqual([]);
  });
}

// The address page states its oldest block as a headline figure rather than in
// a table, and the same argument applies: a height with no age is half an
// answer.
test('the address page dates its oldest block', async ({ page }) => {
  const seen = watch(page);
  await page.goto(`/address/${BUSY_CALLER}?network=alpha`);
  await settle(page);

  const age = await page.evaluate(() => {
    const stat = [...document.querySelectorAll('#address-detail-content .stats-bar .stat')]
      .find(s => /oldest here|first seen/.test(s.textContent));
    if (!stat) return { error: 'no oldest-block stat on the page' };
    const el = stat.querySelector('[data-age]');
    return { text: el ? el.textContent.trim() : null };
  });
  expect(age.error, age.error).toBeUndefined();
  expect(age.text, 'the oldest block is printed with no age').not.toBeNull();
  expect(age.text).toMatch(AGE);

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.consoleErrors)).toEqual([]);
});

// The age is computed once at render and would otherwise still read "2m" on a
// tab left open overnight. data-age is what the ticker finds, so an age drawn
// without it is the regression this pins.
test('every rendered age is tickable', async ({ page }) => {
  const seen = watch(page);
  await page.goto(`/realm/${HUB_ROUTE}?network=alpha&tab=calls`);
  await settle(page);

  const { ages, stale } = await page.evaluate(() => {
    const spans = [...document.querySelectorAll('#tab-calls .block-ctx')]
      .filter(s => /^\(\d/.test(s.textContent.trim()));
    return { ages: spans.length, stale: spans.filter(s => !s.hasAttribute('data-age')).length };
  });
  expect(ages, 'no ages on the page at all, so nothing was checked').toBeGreaterThan(0);
  expect(stale, 'an age the 10s ticker will never refresh').toBe(0);

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.consoleErrors)).toEqual([]);
});

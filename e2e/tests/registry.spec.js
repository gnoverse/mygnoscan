import { expect, test } from '@playwright/test';

import { settle, unexpected, watch } from './helpers.js';

// The curated registry, as it reaches a reader.
//
// The data now lives in pkg/registry/data/*.json and arrives over /api/labels
// and /api/registry/apps. Before this it was a const inside index.html, so
// there was nothing to test but the const itself.

test('the apps directory renders what each realm is for', async ({ page }) => {
  const seen = watch(page);

  const response = await page.goto('/apps');
  expect(response.status()).toBe(200);
  await settle(page);

  const content = page.locator('#apps-content');

  // Categories are sections, and every row says what the thing does. A
  // directory that only lists paths is what /realms already is.
  await expect(content).toContainText('governance');
  await expect(content).toContainText('GovDAO');
  await expect(content).toContainText('proposals, votes');

  // An entry whose blurb names a fact that can change under it says when that
  // fact was last confirmed, so a reader can weigh it rather than assume it is
  // current. Entries with nothing volatile in them carry no date and show none.
  const dated = content.locator('tr', { hasText: 'Boards2' });
  await expect(dated).toContainText(/checked \d{4}-\d{2}-\d{2}/);
  await expect(content.locator('tr', { hasText: 'Valopers' })).not.toContainText('checked ');

  expect(seen.jsErrors, 'uncaught exceptions').toEqual([]);
  expect(unexpected(seen.consoleErrors), 'console errors').toEqual([]);

  // The path is the link, because the path is the part that is on chain.
  //
  // Only the URL is asserted past this point: the directory is curated for real
  // chains, so the realm it lands on is not in the harness fixture and the
  // detail page 404s here. That is the fixture's limit, not the link's.
  const realmLink = content.getByText('gno.land/r/gov/dao', { exact: true });
  await expect(realmLink).toBeVisible();
  await realmLink.click();
  await expect(page).toHaveURL(/\/realm\/r\/gov\/dao/);
});

test('curated labels reach the page from the API, not from a const', async ({ page }) => {
  const seen = watch(page);

  await page.goto('/');
  await settle(page);

  // The registry travels over the wire now. If it ever stops being served, the
  // frontend has no fallback copy to quietly fall back on, which is the point.
  const labels = await page.evaluate(() => fetch('/api/labels').then(r => r.json()));
  const oracle = labels['g1yaaa6rcp4ew5yjzdj4yms596wx2dtrj3a86704'];
  expect(oracle, 'the gpao oracle should be labelled').toBeTruthy();
  expect(oracle.label).toBe('@gpao_oracle');
  expect(oracle.kind).toBe('curated');

  expect(seen.jsErrors, 'uncaught exceptions').toEqual([]);
});

test('an inferred label is marked and carries its evidence', async ({ page }) => {
  const seen = watch(page);

  await page.goto('/');
  await settle(page);

  // Rendering is checked through the helper the whole app funnels through,
  // rather than by hunting for an address that happens to be on screen.
  const rendered = await page.evaluate(() => {
    const span = addrLinkImpl('g18qhq2fl54lszhmxeyqlvxnwjzc3xpu4nnakclp', null, 'fallback');
    const mark = span.querySelector('.addr-inferred');
    return { text: span.textContent, mark: mark ? mark.textContent : null, hint: mark ? mark.title : '' };
  });

  expect(rendered.text).toContain('@faucet');
  // Only the kinds that ask a reader to take something on trust are marked.
  expect(rendered.mark).toBe('?');
  expect(rendered.hint).toContain('inferred');
  expect(rendered.hint).toContain('sends');

  expect(seen.jsErrors, 'uncaught exceptions').toEqual([]);
});

test('a curated label is not marked', async ({ page }) => {
  const seen = watch(page);

  await page.goto('/');
  await settle(page);

  const rendered = await page.evaluate(() => {
    const span = addrLinkImpl('g1yaaa6rcp4ew5yjzdj4yms596wx2dtrj3a86704', null, 'fallback');
    return { text: span.textContent, mark: !!span.querySelector('.addr-inferred') };
  });

  expect(rendered.text).toContain('@gpao_oracle');
  // A human vouched for this one in a merged pull request, so there is nothing
  // to caveat.
  expect(rendered.mark).toBe(false);

  expect(seen.jsErrors, 'uncaught exceptions').toEqual([]);
});

test('plain mode keeps the address and moves the label into the tooltip', async ({ page }) => {
  const seen = watch(page);

  await page.goto('/');
  await settle(page);

  // What /coins' three side-by-side leaderboards use. The label and the address
  // together are wider than a third of the page, and the figure in the next
  // cell was what got clipped — so the label gives way, and has to still be
  // reachable or a reader who knows @faucet by name loses it entirely.
  const rendered = await page.evaluate(() => {
    const addr = 'g18qhq2fl54lszhmxeyqlvxnwjzc3xpu4nnakclp';
    const span = addrLinkImpl(addr, null, 'g18qhq2f…kclp', true);
    const a = span.querySelector('a');
    return {
      text: span.textContent,
      link: a.textContent,
      title: a.title,
      ctx: !!span.querySelector('.block-ctx'),
      mark: span.querySelector('.addr-inferred') ? span.querySelector('.addr-inferred').textContent : null,
    };
  });

  expect(rendered.link).toBe('g18qhq2f…kclp');
  expect(rendered.text).not.toContain('@faucet');
  // Not lost: first line of the tooltip, ahead of the full address it explains.
  expect(rendered.title.split('\n')[0]).toBe('@faucet');
  expect(rendered.title).toContain('g18qhq2fl54lszhmxeyqlvxnwjzc3xpu4nnakclp');
  // The label is what gave way, not the caveat on it.
  expect(rendered.mark).toBe('?');
  // Plain mode drops the trailing address-beside-the-label, since the label is
  // gone and the address is now the link itself.
  expect(rendered.ctx).toBe(false);

  expect(seen.jsErrors, 'uncaught exceptions').toEqual([]);
});

import { expect, test } from '@playwright/test';

import { settle } from './helpers.js';

// Reads, which the chain does not record.
//
// Every other figure this explorer shows is a write: calls, callers, gas,
// transfers. A realm people read and never write to looks dead in all of them,
// so the one honest read signal available is how many times this explorer's own
// readers opened it. These pin the two things that make that number wrong if
// they break: counting through the cache, and counting robots.

// A real browser's user agent. Playwright sends HeadlessChrome by default,
// which the counter drops on purpose, so a test that did not set this would
// prove the opposite of what it means to.
const READER = { userAgent: 'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0 Safari/537.36' };

test('opening a realm counts, and counts again through the cache', async ({ browser }) => {
  const ctx = await browser.newContext(READER);
  const page = await ctx.newPage();

  const path = 'r/gnoland/blog';
  for (let i = 0; i < 2; i++) {
    await page.goto(`/realm/${path}?network=alpha`);
    await settle(page);
  }

  // The endpoint flushes the buffer before answering, so this needs no sleep.
  const res = await ctx.request.get('/api/views?network=alpha');
  expect(res.status()).toBe(200);
  const body = await res.json();

  // Said in the payload, not only in the docs: a consumer rendering this beside
  // a call count has to be told which one the chain proves.
  expect(body.kind).toBe('inferred');
  expect(body.why).toMatch(/this explorer.s own measurement/);

  const row = (body.realms || []).find(r => r.path === `gno.land/${path}`);
  expect(row, 'the realm was opened twice and is not in the list').toBeTruthy();
  // The second open is served from the response cache and must still count:
  // otherwise the more a realm is read, the less it appears to be read.
  expect(row.views).toBeGreaterThanOrEqual(2);

  await ctx.close();
});

test('the realm page says who counted, not just how many', async ({ browser }) => {
  const ctx = await browser.newContext(READER);
  const page = await ctx.newPage();

  await page.goto('/realm/r/gnoland/blog?network=alpha');
  await settle(page);
  await page.goto('/realm/r/gnoland/blog?network=alpha&tab=calls');
  await settle(page);

  const stat = page.locator('.stat').filter({ hasText: 'opened here' });
  await expect(stat).toBeVisible();
  // The mark is the point. A count this server made up out of its own traffic
  // must never sit in a row of chain facts without saying so.
  await expect(stat.locator('.app-prov')).toHaveAttribute('title', /not a chain fact/);

  await ctx.close();
});

test('a robot does not vote', async ({ browser }) => {
  const ctx = await browser.newContext({ userAgent: 'Googlebot/2.1 (+http://www.google.com/bot.html)' });

  const before = await ctx.request.get('/api/views?network=alpha');
  const seen = (r) => ((r.realms || []).find(x => x.path === 'gno.land/r/gnoland/wugnot') || {}).views || 0;
  const was = seen(await before.json());

  const page = await ctx.newPage();
  await page.goto('/realm/r/gnoland/wugnot?network=alpha');
  await settle(page);

  const after = await ctx.request.get('/api/views?network=alpha');
  expect(seen(await after.json())).toBe(was);

  await ctx.close();
});

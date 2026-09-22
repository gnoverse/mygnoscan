// Realm screenshots: the picture on the detail page, the thumbnails in the
// listings, and what happens when there is no picture.
//
// The assertions worth having here are not "an image appeared". They are the
// three things that make fifty images on one page acceptable: every box states
// its own size, nothing is fetched before it is scrolled to, and the page is
// entirely readable with every image still in flight.
import { expect, test } from '@playwright/test';

import { HUB_ROUTE } from '../harness/fixture.mjs';
import { settle, unexpected, watch } from './helpers.js';

test.describe('realm screenshots', () => {
  test('every listing row carries a sized, lazy thumbnail', async ({ page }) => {
    const seen = watch(page);
    await page.goto('/realms');
    await settle(page);

    const rows = page.locator('#realms-list tr');
    expect(await rows.count()).toBeGreaterThan(5);

    const imgs = page.locator('#realms-list td.shot-cell img');
    const n = await imgs.count();
    expect(n).toBeGreaterThan(5);

    // Explicit width and height on every one. Without them lazy loading is
    // worse than useless: the row has no height until the image arrives, so a
    // listing jumps under the reader's cursor as fifty of them resolve.
    const attrs = await imgs.evaluateAll(els => els.map(e => ({
      loading: e.getAttribute('loading'),
      decoding: e.getAttribute('decoding'),
      width: e.getAttribute('width'),
      height: e.getAttribute('height'),
      sizes: e.getAttribute('sizes'),
      srcset: e.getAttribute('srcset'),
      src: e.getAttribute('src'),
      alt: e.getAttribute('alt'),
      // The el() trap: el() puts every unknown key through setAttribute, so
      // {onerror: fn} would write a stringified function into an inline
      // event-handler attribute. Nothing here may carry one.
      onerror: e.getAttribute('onerror'),
      onload: e.getAttribute('onload'),
    })));
    for (const a of attrs) {
      expect(a.loading).toBe('lazy');
      expect(a.decoding).toBe('async');
      expect(Number(a.width)).toBeGreaterThan(0);
      expect(Number(a.height)).toBeGreaterThan(0);
      expect(a.sizes).toBeTruthy();
      expect(a.srcset).toContain('dpr=2');
      expect(a.src).toContain('/api/shot?');
      expect(a.alt).toBeTruthy();
      expect(a.onerror).toBeNull();
      expect(a.onload).toBeNull();
    }
    expect(unexpected(seen.failedRequests)).toEqual([]);
    expect(unexpected(seen.consoleErrors)).toEqual([]);
    expect(seen.jsErrors).toEqual([]);
  });

  test('the realm page shows what the realm looks like', async ({ page }) => {
    await page.goto(`/realm/${HUB_ROUTE}`);
    await settle(page);
    const hero = page.locator('#realm-header .shot-hero');
    await expect(hero).toHaveCount(1);
    const img = hero.locator('img');
    await expect(img).toHaveCount(1);
    await expect(img).toHaveAttribute('src', /\/api\/shot\?/);
    // The hero asks for the larger rung; a listing thumbnail would be a blurry
    // 160-pixel crop stretched across 720.
    await expect(img).toHaveAttribute('src', /size=hero/);
  });

  // The hero belongs to the overview. On the source, deps or state tab the
  // reader navigated somewhere deliberately, and 405 pixels of picture above
  // what they came for is the wrong trade — it put the dependency graph below
  // the fold, where hovering a node stopped working at all.
  test('the hero is on the overview and nowhere else', async ({ page }) => {
    await page.goto(`/realm/${HUB_ROUTE}`);
    await settle(page);
    await expect(page.locator('#realm-header .shot-hero')).toBeVisible();

    await page.locator('#realm-tabs .tab[data-tab="source"]').click();
    await expect(page.locator('#realm-header .shot-hero')).toBeHidden();

    await page.locator('#realm-tabs .tab[data-tab="info"]').click();
    await expect(page.locator('#realm-header .shot-hero')).toBeVisible();
  });

  test('a deep link to another tab does not draw the hero', async ({ page }) => {
    // The starting tab is made active by a class rather than by a click, so it
    // never passes through switchTab. Landing here with the hero showing is the
    // way this breaks.
    await page.goto(`/realm/${HUB_ROUTE}?tab=deps`);
    await settle(page);
    await expect(page.locator('#realm-header .shot-hero')).toBeHidden();
  });

  // The picture is keyed on the realm's last activity, not on its deploy: a
  // render is a function of chain state and changes when nobody redeploys
  // anything, so keying on the deploy pins a realm's thumbnail to whatever it
  // looked like the day it shipped.
  test('the image URL carries a cache key that moves with the chain', async ({ page }) => {
    await page.goto('/realms');
    await settle(page);
    const srcs = await page.locator('#realms-list td.shot-cell img').evaluateAll(
      els => els.map(e => e.getAttribute('src')));
    const withV = srcs.filter(s => /[?&]v=\d+/.test(s));
    expect(withV.length).toBeGreaterThan(0);
  });

  test('a listing is fully readable with every image failing', async ({ page }) => {
    // The whole feature is a decoration. A capture service that is down must
    // cost the reader a picture and nothing else.
    await page.route('**/api/shot**', route => route.fulfill({ status: 503, body: '' }));
    const errors = [];
    page.on('pageerror', e => errors.push(String(e)));

    await page.goto('/realms');
    await settle(page);

    const rows = page.locator('#realms-list tr');
    expect(await rows.count()).toBeGreaterThan(5);
    // The path column still says what it always said.
    await expect(page.locator('#realms-list tr').first()).toContainText('/');
    expect(errors).toEqual([]);
  });

  test('a failed image is replaced by a tile that says so in words', async ({ page }) => {
    await page.route('**/api/shot**', route => route.fulfill({ status: 503, body: '' }));
    await page.goto(`/realm/${HUB_ROUTE}`);
    await settle(page);
    const tile = page.locator('#realm-header .shot-tile');
    await expect(tile).toHaveCount(1);
    // Never a blank box: a reader who does not know what a realm is reads
    // blank as "the site is broken".
    await expect(tile).toContainText(/unavailable/i);
    // And the same sentence is the accessible name, because a screen reader
    // has no size.
    const label = await tile.getAttribute('aria-label');
    expect(label).toBeTruthy();
    expect(label.length).toBeGreaterThan(20);
  });

  test('a full page of thumbnails stays inside its byte budget', async ({ page }) => {
    // Requests, deliberately not asserted. loading="lazy" is honoured, but the
    // distance-from-viewport threshold that decides *when* is Chromium's and
    // it is generous on a fast connection: measured 2026-09-22, a 50-row
    // /realms at a 1400x900 viewport asks for all 50 before any scroll. So the
    // budget has to hold by construction rather than by deferral, which is why
    // each rung is capped at generation and the listing slot is 160 CSS px.
    let bytes = 0;
    let biggest = 0;
    const seenBody = new Map();
    page.on('response', async res => {
      if (!res.url().includes('/api/shot')) return;
      let body;
      try {
        body = await res.body();
      } catch {
        return; // Served from cache, and cache is free.
      }
      // Counted once per distinct URL: the app paints from cache and then
      // repaints from fresh, and the repaint is a cache hit in production
      // because every response is immutable.
      if (seenBody.has(res.url())) return;
      seenBody.set(res.url(), body.length);
      bytes += body.length;
      biggest = Math.max(biggest, body.length);
    });

    await page.setViewportSize({ width: 1400, height: 900 });
    await page.goto('/realms');
    await settle(page);
    await page.evaluate(() => window.scrollTo(0, document.body.scrollHeight));
    await page.waitForTimeout(1500);

    expect(seenBody.size).toBeGreaterThan(5);
    // Per image: a thumbnail that needs more than this is a bug in the ladder,
    // not a budget that should move.
    expect(biggest).toBeLessThan(12 * 1024);
    // Per page, fully scrolled. For scale, the same page's JSON is about 23 KB
    // uncompressed, so this is the one surface where the pictures outweigh the
    // data, and it is only acceptable because every byte is same-origin and
    // off the critical path.
    expect(bytes).toBeLessThan(400 * 1024);
  });

});

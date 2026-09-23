import { expect, test } from '@playwright/test';

import { settle } from './helpers.js';

// The rail is the one piece of chrome on every page, and it grows every time
// somebody adds a section. What it must not do as it grows is become a scroll
// container: `overflow-y: auto` on the rail itself means that the moment its
// content passes the viewport, a wheel anywhere over the left 172px scrolls the
// nav by a few pixels instead of scrolling the page.
//
// That is not hypothetical. Measured on 2026-09-23 at a 1000px viewport, the
// rail's content was exactly 1000px: it had been sitting one entry away from
// this for a while, and adding `glossary` tipped it over. The page stopped
// scrolling and a live-feed test three files away went red, which is a terrible
// way to find out.
//
// These two tests are deliberately about the behaviour rather than the CSS, so
// they survive the next refactor of how the scroll is confined.

test('a wheel over the rail chrome still scrolls the page', async ({ page }) => {
  // Short enough that the rail certainly overflows, whatever is in it today.
  await page.setViewportSize({ width: 1400, height: 600 });
  await page.goto('/blocks');
  await settle(page);

  // (0, 0) is the brand, and it is also where an automated wheel starts from.
  await page.mouse.move(5, 5);
  await page.mouse.wheel(0, 20_000);
  await page.waitForFunction(() => window.scrollY > 200);
});

test('the nav scrolls on its own, and the chrome around it stays put', async ({ page }) => {
  await page.setViewportSize({ width: 1400, height: 600 });
  await page.goto('/blocks');
  await settle(page);

  const nav = page.locator('aside nav');
  const overflows = await nav.evaluate(el => el.scrollHeight > el.clientHeight);
  expect(overflows, 'at 600px the nav has more entries than fit; if not, shorten the viewport').toBe(true);

  // The collapse button is the bottom of the rail. If the rail were the scroll
  // container it would scroll away with the links; pinned is the whole point.
  const toggle = page.locator('#rail-toggle');
  await expect(toggle).toBeInViewport();

  await nav.evaluate(el => { el.scrollTop = el.scrollHeight; });
  expect(await nav.evaluate(el => el.scrollTop)).toBeGreaterThan(0);
  await expect(toggle).toBeInViewport();
  await expect(page.locator('.rail-top')).toBeInViewport();
});

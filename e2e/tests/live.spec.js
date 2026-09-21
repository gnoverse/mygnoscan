import { expect, test } from '@playwright/test';

import { settle, unexpected, watch } from './helpers.js';

// Live mode is on by default, and the HUD in the header is what says so.
//
// The fake indexer serves a fixed tip, so no block is ever minted during a run.
// That is not a limitation here, it is the interesting half: it exercises the
// path where the feed connects and then nothing arrives, which is exactly the
// case a green dot must not keep claiming is fine.

const TIP = 1039; // 1000 + CHAIN_LENGTH - 1, from the fake indexer.

const hud = page => page.locator('#live-toggle');

test('live is on without anyone clicking, and the header carries the tip', async ({ page }) => {
  const seen = watch(page);

  // Deliberately not the home page: the block height used to live only in the
  // home stats bar, so every other page had no tip on it at all.
  await page.goto('/realms');
  await settle(page);

  await expect(hud(page)).toHaveClass(/active/);
  await expect(page.locator('#live-height')).toHaveText(TIP.toLocaleString('en-US'));

  expect(seen.jsErrors, 'uncaught exceptions').toEqual([]);
  expect(unexpected(seen.consoleErrors), 'console errors').toEqual([]);
});

test('the tip stays on screen at the bottom of a long page', async ({ page }) => {
  await page.goto('/blocks');
  await settle(page);

  await page.mouse.wheel(0, 40_000);
  await page.waitForFunction(() => window.scrollY > 200);

  // The point of putting it in the sticky header: scrolled to the bottom of a
  // 500-row table, the reader can still see where the chain is.
  await expect(hud(page)).toBeInViewport();
  await expect(page.locator('#live-height')).toHaveText(TIP.toLocaleString('en-US'));
});

test('a block advances the height and flashes the chip', async ({ page }) => {
  await page.goto('/blocks');
  await settle(page);

  // The flash lasts ~300ms, so it is recorded as it happens rather than polled
  // for afterwards: an assertion that has to round-trip to the browser first is
  // racing the timeout that clears it, and would pass or fail on how busy the
  // machine is.
  await page.evaluate(() => {
    window.__flashed = false;
    const btn = document.getElementById('live-toggle');
    new MutationObserver(() => { if (btn.classList.contains('tick')) window.__flashed = true; })
      .observe(btn, { attributes: true, attributeFilter: ['class'] });
  });

  // Driven through the real event handler rather than a stubbed stream: it is
  // the function the SSE message ends up calling, and it is what has to repaint
  // the HUD, the home stat and the table in one go.
  await page.evaluate(tip => {
    onLiveBlock({ height: tip + 1, time: new Date().toISOString(), num_txs: 0, hash: 'abc' }, 'alpha');
  }, TIP);

  await expect(page.locator('#live-height')).toHaveText((TIP + 1).toLocaleString('en-US'));
  expect(await page.evaluate(() => window.__flashed), 'the chip flashed on arrival').toBe(true);
  // And the flash is a flash: a chip that stays lit reads as a warning.
  await expect(hud(page)).not.toHaveClass(/tick/, { timeout: 2000 });
});

test('the beat fills between blocks', async ({ page }) => {
  await page.goto('/blocks');
  await settle(page);

  await page.evaluate(tip => {
    onLiveBlock({ height: tip + 1, time: new Date().toISOString(), num_txs: 0, hash: 'abc' }, 'alpha');
  }, TIP);

  const width = () => page.evaluate(() =>
    parseFloat(document.querySelector('#live-beat i').style.width) || 0);

  // The whole reason the beat exists: between two blocks the height does not
  // change, so something else has to move or the page cannot be told from a
  // frozen one.
  await expect.poll(width, { timeout: 5000 }).toBeGreaterThan(5);
  const first = await width();
  await expect.poll(width, { timeout: 5000 }).toBeGreaterThan(first);
});

test('the block train draws one bar per block, taller where there were txs', async ({ page }) => {
  await page.goto('/blocks');
  await settle(page);

  await page.evaluate(tip => {
    [0, 4, 0, 19].forEach((n, i) => onLiveBlock(
      { height: tip + 1 + i, time: new Date().toISOString(), num_txs: n, hash: 'h' + i }, 'alpha'));
  }, TIP);

  const bars = page.locator('#live-train i');
  await expect(bars).toHaveCount(4);

  const heights = await bars.evaluateAll(els => els.map(e => parseFloat(e.style.height)));
  // An empty block is a stub, a busy one is tall, and the scale is log, so a
  // 19-tx block must not be five times the 4-tx one.
  expect(heights[0]).toBe(3);
  expect(heights[1]).toBeGreaterThan(heights[0]);
  expect(heights[3]).toBeGreaterThan(heights[1]);
  expect(heights[3]).toBeLessThan(heights[1] * 2);

  // An empty block is dim rather than absent: most gno blocks are empty, and a
  // strip that renders as nothing on a quiet chain is a strip that never works.
  await expect(bars.nth(0)).not.toHaveClass(/txs/);
  await expect(bars.nth(1)).toHaveClass(/txs/);
  expect(await bars.nth(0).evaluate(e => parseFloat(getComputedStyle(e).opacity)))
    .toBeGreaterThan(0.2);
});

test('the train is bounded, so the header cannot grow without end', async ({ page }) => {
  await page.goto('/blocks');
  await settle(page);

  await page.evaluate(tip => {
    for (let i = 0; i < 40; i++) {
      onLiveBlock({ height: tip + 1 + i, time: new Date().toISOString(), num_txs: i % 3, hash: 'h' + i }, 'alpha');
    }
  }, TIP);

  await expect(page.locator('#live-train i')).toHaveCount(12);
});

test('a replayed block is not counted, redrawn or flashed twice', async ({ page }) => {
  await page.goto('/blocks');
  await settle(page);

  // What a reconnect looks like from the client: the feed hands every new
  // subscriber its recent tail, so the blocks either side of the drop arrive
  // again. Counting events rather than blocks tripled the tooltip's figure
  // against mainnet and shoved a screenful of bars into the train at once.
  const send = (h, txs) => page.evaluate(([h, txs]) => onLiveBlock(
    { height: h, time: new Date().toISOString(), num_txs: txs, hash: 'h' + h }, 'alpha'), [h, txs]);

  await send(TIP + 1, 2);
  await send(TIP + 2, 1);
  await send(TIP + 1, 2); // the replay
  await send(TIP + 2, 1);
  await send(TIP + 3, 4); // genuinely new, past the drop

  await expect(page.locator('#live-train i')).toHaveCount(3);
  await expect(page.locator('#live-height')).toHaveText((TIP + 3).toLocaleString('en-US'));
  await expect(page.locator('#live-toggle')).toHaveAttribute('title', /\b3 blocks\b/);
});

test('silence turns the chip amber instead of leaving it green', async ({ page }) => {
  await page.goto('/blocks');
  await settle(page);

  await expect(hud(page)).toHaveClass(/active/);

  // No block is ever minted against the fixture, so this is the real thing:
  // three expected block times of nothing, and the HUD stops claiming health.
  // The default expectation is 5s until gaps have been measured.
  await expect(hud(page)).toHaveClass(/stalled/, { timeout: 25_000 });
  await expect(hud(page)).toHaveAttribute('title', /no block since connecting/);
});

test('reduced motion drops the sweep but keeps the diagnosis', async ({ page }) => {
  await page.emulateMedia({ reducedMotion: 'reduce' });
  await page.goto('/blocks');
  await settle(page);

  expect(await page.evaluate(() => prefersReducedMotion())).toBe(true);

  // The beat is a continuous sweep and nothing but motion, so it goes entirely.
  await expect(page.locator('#live-beat')).toBeHidden();
  // Everything that is information stays: the height, and the train.
  await expect(page.locator('#live-height')).toHaveText(TIP.toLocaleString('en-US'));

  await page.evaluate(tip => onLiveBlock(
    { height: tip + 1, time: new Date().toISOString(), num_txs: 3, hash: 'z' }, 'alpha'), TIP);
  await expect(page.locator('#live-train i')).toHaveCount(1);
  // The train keeps its bars and loses only the slide-in. This assertion is the
  // one that caught the override being written above the rules it overrides,
  // where equal specificity meant source order silently discarded it.
  expect(await page.locator('#live-train i').evaluate(e => getComputedStyle(e).animationName)).toBe('none');

  // And the stall check runs off a timer rather than the frame loop, so the
  // diagnosis survives the animation being switched off.
  await expect(hud(page)).toHaveClass(/stalled/, { timeout: 25_000 });
});

test('switching it off sticks across a reload', async ({ page }) => {
  await page.goto('/blocks');
  await settle(page);
  await expect(hud(page)).toHaveClass(/active/);

  await hud(page).click();
  await expect(hud(page)).not.toHaveClass(/active/);

  await page.reload();
  await settle(page);

  // Default-on must not mean overriding a reader who said no.
  await expect(hud(page)).not.toHaveClass(/active/);
  await expect(page.locator('#live-height')).toHaveText('live');

  await hud(page).click();
  await expect(hud(page)).toHaveClass(/active|connecting/);
});

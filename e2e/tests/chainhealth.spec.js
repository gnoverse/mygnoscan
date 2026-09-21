import { expect, test } from '@playwright/test';

import { settle, unexpected, watch } from './helpers.js';

// The chain diagnosis, the heartbeat strip, and the header chip.
//
// The harness configures no RPC, so the real verdict here is always one of the
// cannot-ask states. That covers the degrade path but not the finding, so the
// interesting states are stubbed: a wedged chain is the case this whole feature
// exists for and it is not reproducible against a fixture.

const WEDGED = {
  by_network: {
    alpha: { chain_height: 546040, seconds_since_block: 6171207, is_alive: false, reachable: false },
    beta: { chain_height: 100, seconds_since_block: 3, is_alive: true, reachable: true },
  },
  tx_last_1h: 0,
  tx_last_24h: 0,
  nodes: {
    alpha: {
      reachable: true, rpc: 'https://rpc.example.invalid', chain_id: 'staging', verified: false,
      height: 546040, round_height: 546041, round: 0, step: 4, step_name: 'Prevote',
      round_start: '2026-07-10T13:11:36Z', round_age_seconds: 6171207, peers: 0, mempool_txs: 3,
    },
    beta: {
      reachable: true, rpc: 'https://rpc.example.invalid', chain_id: 'beta-1', verified: true,
      height: 100, round_height: 101, round: 0, step: 1, step_name: 'NewHeight',
      round_age_seconds: 2, peers: 7, mempool_txs: 0,
    },
  },
  diagnosis: {
    alpha: {
      state: 'wedged', healthy: false,
      detail: 'consensus has not advanced for 71d: stuck at 546041/0/4 (Prevote) since 2026-07-10T13:11:36Z, '
        + 'with 3 transaction(s) queued that will never be included. The node has no peers',
    },
    beta: { state: 'alive', healthy: true, detail: 'producing blocks at height 100, 7 peers' },
  },
};

async function stubSanity(page, payload) {
  await page.route('**/api/sanity/overview*', route =>
    route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(payload) }));
}

test('a wedged chain says where it stopped, not that it is unreachable', async ({ page }) => {
  const seen = watch(page);
  await stubSanity(page, WEDGED);

  await page.goto('/sanity');
  await settle(page);

  const content = page.locator('#sanity-content');

  // The finding, in the words a reader can act on. "unreachable" is what this
  // page said before, and it pointed at the wrong problem.
  await expect(content).toContainText('wedged');
  await expect(content).toContainText('546041/0/4');
  await expect(content).toContainText('Prevote');
  await expect(content).toContainText('will never be included');

  // The node answered, so its own height is shown even though the indexer
  // reported the network as unreachable.
  await expect(content).toContainText('546,040');

  // An endpoint nothing cross-checked is marked as such, because this probe
  // deliberately talks to unverified RPCs.
  await expect(content).toContainText('unverified');

  expect(seen.jsErrors, 'uncaught exceptions').toEqual([]);
  expect(unexpected(seen.consoleErrors), 'console errors').toEqual([]);
});

test('the header chip carries the worst verdict on every page', async ({ page }) => {
  const seen = watch(page);
  await stubSanity(page, WEDGED);

  // Deliberately not /sanity: the whole point is that a reader who never opens
  // that page still finds out.
  await page.goto('/realms');
  await settle(page);

  const chip = page.locator('#health-chip');
  await expect(chip).toBeVisible();
  await expect(chip).toContainText('alpha');
  await expect(chip).toContainText('wedged');
  await expect(chip).toHaveAttribute('title', /consensus has not advanced/);

  // And it is a way in, not just a warning.
  await chip.click();
  await expect(page).toHaveURL(/\/sanity/);

  expect(seen.jsErrors, 'uncaught exceptions').toEqual([]);
});

test('the header chip stays hidden while every chain is alive', async ({ page }) => {
  const seen = watch(page);
  await stubSanity(page, {
    by_network: { beta: { chain_height: 100, seconds_since_block: 3, is_alive: true, reachable: true } },
    nodes: { beta: WEDGED.nodes.beta },
    diagnosis: { beta: WEDGED.diagnosis.beta },
  });

  await page.goto('/realms');
  await settle(page);

  // A permanent green light is something a reader stops seeing, so there isn't
  // one.
  await expect(page.locator('#health-chip')).toBeHidden();

  expect(seen.jsErrors, 'uncaught exceptions').toEqual([]);
});

test('the heartbeat strip renders a cell per interval for every network', async ({ page }) => {
  const seen = watch(page);

  await page.goto('/sanity');
  await settle(page);

  const content = page.locator('#sanity-content');
  await expect(content).toContainText('heartbeat');
  // The harness has blocks for its networks, so there is a strip; what matters
  // here is that it draws without throwing and says its own scale.
  await expect(content).toContainText('per cell');

  // Switching the window is a second fetch and a repaint, and the generation
  // guard around it is the kind of thing that only breaks in a browser.
  await content.getByRole('button', { name: '30m', exact: true }).click();
  await expect(content).toContainText('60s per cell');

  expect(seen.jsErrors, 'uncaught exceptions').toEqual([]);
  expect(unexpected(seen.consoleErrors), 'console errors').toEqual([]);
});

// The harness's real configuration: no RPC anywhere. The page has to say it
// cannot ask, rather than reporting a chain as broken on no evidence.
test('with no node configured the page says it cannot ask', async ({ page }) => {
  const seen = watch(page);

  const response = await page.goto('/sanity');
  expect(response.status()).toBe(200);
  await settle(page);

  await expect(page.locator('#sanity-content')).toContainText('chain diagnosis');

  expect(seen.jsErrors, 'uncaught exceptions').toEqual([]);
  expect(unexpected(seen.failedRequests), 'failed requests').toEqual([]);
});

// The storage panel reads two series that measure different things, and only
// one of them can see a release: bytes_added comes from package_files and
// only ever grows, while storage_events is what the chain charged and
// refunded. A "recovered" figure sourced from the wrong one would be a
// constant zero that looked like a fact.
//
// The fixture releases 2 MiB on alpha (store-hub-free), at a timestamp 50-odd
// days old, so this also pins the two halves of the empty state: the default
// 30d window has nothing to draw and has to say so, and 90d finds it.
test('the storage panel reports bytes recovered, and says when it has none', async ({ page }) => {
  await page.goto('/sanity?network=alpha');
  await settle(page);

  const note = page.locator('#sanity-storage .section-sub');
  await expect(note).toContainText('no storage events in this window');

  await page.locator('#sanity-days-group button', { hasText: '90d' }).click();
  await expect(note).toContainText('2.00 MB recovered in this window');
  await expect(note).toContainText('what the chain charged and refunded');
});

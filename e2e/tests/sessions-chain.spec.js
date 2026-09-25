import { expect, test } from '@playwright/test';

import { settle, unexpected, watch } from './helpers.js';

// The chain-wide sessions page, and the session address page that used to be
// blank.
//
// Both read the index, not the chain. The index exists because neither the
// chain nor the rest of this explorer can answer these: a session signs for its
// master and the master is the caller of every message, so a session address
// appears in no call, no send and no deploy, and auth/accounts/<session_addr>
// returns null because a session is not a plain account.

const MASTER = 'g1master00000000000000000000000000000';
const KEY_LIVE = 'g1keylive0000000000000000000000000000';
const KEY_REVOKED = 'g1keyrevoked00000000000000000000000000';
const KEY_EXPIRED = 'g1keyexpired00000000000000000000000000';

const PAST = 1600000000; // 2020
const FUTURE = 4102444800; // 2100

const CHAIN = {
  stats: { total: 3, live: 1, expired: 1, revoked: 1, masters: 2, realms: 2 },
  total: 3,
  scanned: { complete: true, at: 1000, stop: 1000 },
  realms: [
    { path: 'gno.land/r/popular', grants: 3, masters: 3 },
    { path: '*', grants: 1, masters: 1 },
  ],
  grants: [
    {
      session_addr: KEY_LIVE, master: MASTER, allow_paths: ['vm/exec:gno.land/r/moul/faucet'],
      spend_limit: '100000000ugnot', spend_period: 2592000, expires_at: FUTURE,
      granted_height: 900, granted_time: '2026-09-20T10:00:00Z', granted_tx: 'dGVzdGhhc2gx',
    },
    {
      session_addr: KEY_REVOKED, master: MASTER, allow_paths: ['*'],
      spend_limit: '5000000ugnot', spend_period: 0, expires_at: FUTURE,
      granted_height: 800, granted_time: '2026-09-19T10:00:00Z', granted_tx: 'dGVzdGhhc2gy',
      revoked_height: 850, revoked_time: '2026-09-19T12:00:00Z', revoked_tx: 'dGVzdGhhc2gz',
    },
    {
      session_addr: KEY_EXPIRED, master: 'g1other0000000000000000000000000000000',
      allow_paths: ['vm/exec:gno.land/r/gov/dao'],
      spend_limit: '5000000ugnot', spend_period: 0, expires_at: PAST,
      granted_height: 700, granted_time: '2026-09-18T10:00:00Z', granted_tx: 'dGVzdGhhc2g0',
    },
  ],
};

function stubChain(page, body) {
  return page.route('**/api/sessions*', route =>
    route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(body) }));
}

test('the sessions page counts delegation across the chain', async ({ page }) => {
  const seen = watch(page);
  await stubChain(page, CHAIN);

  const response = await page.goto('/sessions');
  expect(response.status()).toBe(200);
  await settle(page);

  const body = page.locator('#sessions-content');
  await expect(body).toContainText('Sessions');
  // The four counters have to partition, so each is worth naming.
  await expect(body).toContainText('grants');
  await expect(body).toContainText('live');
  await expect(body).toContainText('expired');
  await expect(body).toContainText('revoked');
  // Who delegates, which is the adoption figure the grant log does not show.
  await expect(body).toContainText('accounts');

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.failedRequests)).toEqual([]);
});

test('a revoked grant does not read like a live one', async ({ page }) => {
  await stubChain(page, CHAIN);
  await page.goto('/sessions');
  await settle(page);

  const revoked = page.locator('#sessions-content tr', { hasText: 'g1keyrevoked' });
  await expect(revoked).toContainText('revoked');
  // When it ended, not only that it did.
  await expect(revoked).toContainText('850');

  const expired = page.locator('#sessions-content tr', { hasText: 'g1keyexpired' });
  await expect(expired).toContainText('expired');
});

// The realm ranking is by distinct accounts, not by grant count, so one account
// delegating five times cannot look like five adopters.
test('the delegated-to table ranks by accounts', async ({ page }) => {
  await stubChain(page, CHAIN);
  await page.goto('/sessions');
  await settle(page);

  const body = page.locator('#sessions-content');
  await expect(body).toContainText('delegated to');
  await expect(body).toContainText('r/popular');
  // "*" names no realm, so it must not render as a clickable package path.
  await expect(body).toContainText('anything');
});

// A partial index must say so. "3 grants" means something different when the
// sweep has only read a tenth of the chain.
test('a still-running sweep says the counts are partial', async ({ page }) => {
  await stubChain(page, {
    ...CHAIN,
    scanned: { complete: false, at: 100, stop: 1000 },
  });
  await page.goto('/sessions');
  await settle(page);

  const body = page.locator('#sessions-content');
  await expect(body).toContainText('still reading back through the chain');
  await expect(body).toContainText('not the whole chain');
});

test('an empty chain says nobody delegates rather than nothing at all', async ({ page }) => {
  await stubChain(page, {
    stats: { total: 0, live: 0, expired: 0, revoked: 0, masters: 0 },
    grants: [], realms: [], total: 0, scanned: { complete: true, at: 10, stop: 10 },
  });
  await page.goto('/sessions');
  await settle(page);

  await expect(page.locator('#sessions-content')).toContainText('no account on this chain has delegated');
});

// The page this whole feature exists to un-blank.
test('a session address says whose key it is', async ({ page }) => {
  const seen = watch(page);
  await page.route('**/api/address/*/session?*', route =>
    route.fulfill({
      status: 200, contentType: 'application/json',
      body: JSON.stringify({
        address: KEY_LIVE, is_session: true, scan_complete: true, granted: [],
        grants: [CHAIN.grants[0]],
      }),
    }));

  await page.goto('/address/' + KEY_LIVE);
  await settle(page);

  const body = page.locator('#address-detail-content');
  // The headline: this address does not act for itself.
  await expect(body).toContainText('session key');
  await expect(body).toContainText('signs on behalf of');
  await expect(body).toContainText('g1master');
  // And the grant, so the page says what the key may do, not just who owns it.
  await expect(body).toContainText('moul/faucet');
  await expect(body).toContainText('granted');

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.failedRequests)).toEqual([]);
});

// An ordinary address must not grow a session banner.
test('a plain address gets no session banner', async ({ page }) => {
  await page.route('**/api/address/*/session?*', route =>
    route.fulfill({
      status: 200, contentType: 'application/json',
      body: JSON.stringify({ address: MASTER, is_session: false, grants: [], granted: [], scan_complete: true }),
    }));

  await page.goto('/address/' + MASTER);
  await settle(page);

  await expect(page.locator('#address-detail-content')).not.toContainText('signs on behalf of');
});

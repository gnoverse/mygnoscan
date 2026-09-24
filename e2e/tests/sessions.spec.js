import { expect, test } from '@playwright/test';

import { settle, unexpected, watch } from './helpers.js';

// The sessions section on an account page.
//
// A session key signs on a master's behalf and the master stays the caller of
// every message, so the transaction table underneath reads identically whether
// the account signed for itself or handed a key to an agent. This section is
// the only place the difference is visible, which is why an empty-looking one
// is worse than none at all.

const MASTER = 'g1busycaller0000000000000000000000000';

// One live grant, one rolling-window grant, one already expired: the three
// shapes mainnet actually returns, each of which renders a plausible lie if the
// page assumes the common case.
const SESSIONS = {
  address: MASTER,
  supported: true,
  sessions: [
    {
      address: 'g1session1lifetime000000000000000000',
      master: MASTER,
      allow_paths: ['vm/exec:gno.land/r/moul/x/reaper'],
      spend_limit: '5000000ugnot',
      spend_used: '9350ugnot',
      spend_period: 0,
      expires_at: 4102444800, // 2100, comfortably live
      sequence: 1,
    },
    {
      address: 'g1session2rolling0000000000000000000',
      master: MASTER,
      allow_paths: ['vm/exec:gno.land/r/moul/faucet', 'bank/send'],
      spend_limit: '100000000ugnot',
      spend_used: '356972ugnot',
      spend_period: 2592000,
      expires_at: 4102444800,
      sequence: 42,
    },
    {
      address: 'g1session3expired0000000000000000000',
      master: MASTER,
      allow_paths: ['*'],
      spend_limit: '1000000ugnot',
      spend_period: 0,
      expires_at: 1600000000, // 2020
      sequence: 0,
    },
  ],
};

function stubSessions(page, body) {
  return page.route('**/api/address/*/sessions*', route =>
    route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(body) }));
}

test('an account page names the keys that sign for it', async ({ page }) => {
  const seen = watch(page);
  await stubSessions(page, SESSIONS);

  await page.goto('/address/' + MASTER);
  await settle(page);

  const body = page.locator('#address-detail-content');
  await expect(body).toContainText('sessions (3)');

  // The grant, not just that a grant exists. A key scoped to one realm and a
  // key that may do anything are different facts about the same account, and
  // the realm is the part a reader checks.
  await expect(body).toContainText('moul/x/reaper');
  await expect(body).toContainText('moul/faucet');
  await expect(body, 'a wildcard grant must say so in words').toContainText('anything');

  // The window is half the meaning of a spend cap. A lifetime cap is stricter
  // than a rolling one, so rendering both as a bare "x of y" makes the tighter
  // grant look like the looser one.
  await expect(body).toContainText('lifetime');
  await expect(body).toContainText('per 30d');

  // How many transactions the key has actually signed: the one usage figure
  // that survives a call costing no gas.
  await expect(body).toContainText('42');

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.failedRequests)).toEqual([]);
});

// An empty spend_limit is the most restrictive grant there is, not the least.
// The chain rejects any non-zero amount from a session that has none
// (tm2/pkg/sdk/auth/spend.go), so such a key can only make calls that move
// nothing. This rendered as "unlimited" until 2026-09-25, which told a reader
// the opposite of the truth about the most locked-down key on the page.
test('a key with no spend limit reads as unable to spend, not unlimited', async ({ page }) => {
  await stubSessions(page, {
    address: MASTER, supported: true,
    sessions: [{
      address: 'g1nospend00000000000000000000000000000', master: MASTER,
      allow_paths: ['vm/exec:gno.land/r/moul/faucet'],
      spend_limit: '', spend_period: 0, expires_at: 4102444800, sequence: 7,
    }],
  });

  await page.goto('/address/' + MASTER);
  await settle(page);

  const row = page.locator('#address-detail-content tr', { hasText: 'g1nospend' });
  await expect(row).toContainText('cannot spend');
  await expect(row, 'an empty limit is not permission to spend freely').not.toContainText('unlimited');
});

test('an expired key does not read like a live one', async ({ page }) => {
  await stubSessions(page, SESSIONS);

  await page.goto('/address/' + MASTER);
  await settle(page);

  // The chain keeps returning an expired session, and it can no longer sign.
  // Marked beside the key itself, not only in the expiry column, so a scan down
  // the first column is honest.
  const row = page.locator('#address-detail-content tr', { hasText: 'g1session3expired' });
  await expect(row).toContainText('expired');
});

test('a chain that cannot answer says nothing rather than "no sessions"', async ({ page }) => {
  const seen = watch(page);
  // What the endpoint returns with no RPC configured, which is also the e2e
  // harness's own situation: the fake indexer speaks GraphQL only.
  await stubSessions(page, { address: MASTER, supported: false, sessions: [] });

  await page.goto('/address/' + MASTER);
  await settle(page);

  const body = page.locator('#address-detail-content');
  await expect(body, 'the page still renders').toContainText('Address');
  // "this account delegates nothing" and "this chain has never heard of
  // sessions" are different claims, and only one of them is ours to make.
  await expect(body).not.toContainText('sessions (');

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.failedRequests)).toEqual([]);
});

test('an account with no grants draws no empty table', async ({ page }) => {
  await stubSessions(page, { address: MASTER, supported: true, sessions: [] });

  await page.goto('/address/' + MASTER);
  await settle(page);

  await expect(page.locator('#address-detail-content')).not.toContainText('sessions (');
});

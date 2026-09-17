import { expect, test } from '@playwright/test';

import { settle } from './helpers.js';

// Message-type badges and their detail column.
//
// Colour used to be borrowed from status classes: BankMsgSend rendered in
// badge-fail red and MsgRun in badge-ok green, two columns from the status
// badge that uses those same classes to mean "did it succeed". A successful
// send was red beside a green "ok" on the same row.
test('each message type gets its own colour, and none reuses a status colour', async ({ page }) => {
  await page.goto('/');
  await settle(page);

  const classes = await page.evaluate(() => {
    const types = ['MsgCall', 'MsgAddPackage', 'MsgEnablePackage', 'MsgRejectPackage', 'MsgRun', 'BankMsgSend'];
    return types.map(t => typeBadge(t, { value: { __typename: t } }).className);
  });

  for (const c of classes) {
    expect(c, 'a message type must not wear a status colour').not.toMatch(/badge-(ok|fail)\b/);
  }
  expect(new Set(classes).size, 'every type should be distinguishable').toBe(classes.length);
});

// auth/create_session is 2% of mainnet traffic. It arrives as
// UnexpectedMessage, so it is keyed by route rather than typename.
test('a route-named type still gets a stable badge', async ({ page }) => {
  await page.goto('/');
  await settle(page);

  const cls = await page.evaluate(() => typeBadge('UnexpectedMessage', {
    value: { __typename: 'UnexpectedMessage' }, route: 'auth', typeUrl: 'create_session',
  }).className);
  expect(cls).toContain('badge-session');
});

// The detail column exists to say something the type badge does not.
test('enable_package shows which package was approved', async ({ page }) => {
  await page.goto('/');
  await settle(page);

  const text = await page.evaluate(() => txDetailEl({
    value: {
      __typename: 'MsgEnablePackage',
      pkg_path: 'gno.land/r/moul/config/v0',
      pkg_height: 4242,
    },
  }).textContent);

  expect(text, 'should name the package').toContain('moul/config/v0');
  // The submission height, not the approval's own block: under the inert policy
  // those differ, often by days, and which submission got blessed is the point.
  expect(text, 'should carry the submitted height').toContain('4,242');
  expect(text, 'should not just repeat the type badge').not.toContain('enable_package');
});

// A type with no fields has no detail. Repeating the type badge is not detail.
test('a fieldless message shows nothing rather than its own type', async ({ page }) => {
  await page.goto('/');
  await settle(page);

  const text = await page.evaluate(() => txDetailEl({
    value: { __typename: 'UnexpectedMessage' }, route: 'auth', typeUrl: 'create_session',
  }).textContent.trim());

  expect(text).not.toContain('create_session');
  expect(text).toBe('—');
});

// A session-signed transaction must be distinguishable from a normal one.
//
// Every message names its caller — the account being acted for — and that is
// identical whether the account signed for itself or a session key signed for
// it. The signature is the only place the difference is recorded, so without
// this the two look the same.
test('the session types render their grant, not just their name', async ({ page }) => {
  await page.goto('/');
  await settle(page);

  const create = await page.evaluate(() => txDetailEl({
    value: {
      __typename: 'MsgCreateSession',
      creator: 'g1manfred47kzduec920z88wfr64ylksmdcedlf5',
      session_key: 'g10w4vv8km5t0w382vl5qgnma5dety3fnscf7538',
      expires_at: 1792252018,
      allow_paths: ['vm/exec:gno.land/r/moul/x/daily/counter/v0'],
    },
  }).textContent);

  expect(create, 'should name the delegated key').toContain('g10w4vv8');
  // What a session may do matters more than that it exists: a key scoped to one
  // realm is a different thing from one that can spend.
  expect(create, 'should say what the key is scoped to').toContain('gno.land/r/moul/x/daily/counter/v0');
  expect(create, 'should not just repeat the type').not.toContain('create_session');

  // The signer of a session message is its creator, not a blank cell.
  const signer = await page.evaluate(() => txSigner({
    messages: [{ value: { __typename: 'MsgCreateSession', creator: 'g1manfred47kzduec920z88wfr64ylksmdcedlf5' } }],
  }));
  expect(signer).toBe('g1manfred47kzduec920z88wfr64ylksmdcedlf5');
});

// An expired session must not read like a live one.
test('an expired session says so', async ({ page }) => {
  await page.goto('/');
  await settle(page);

  const past = await page.evaluate(() => fmtExpiry(1000000000));
  const future = await page.evaluate(() => fmtExpiry(Math.floor(Date.now() / 1000) + 86400));
  expect(past).toContain('(expired)');
  expect(future).not.toContain('(expired)');
});

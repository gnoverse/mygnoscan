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

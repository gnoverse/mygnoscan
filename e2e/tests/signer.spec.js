import { expect, test } from '@playwright/test';

import { settle } from './helpers.js';

// The signer column on /txs.
//
// It read `caller || creator || from_address`, which covers four of the six
// message types gno has. MsgEnablePackage names its signer `approver` and
// MsgRejectPackage names it `sender`, so both rendered an empty cell — and on a
// chain running the inert code submission policy, MsgEnablePackage is most of
// the page. Observed on production: six of the first twelve rows blank.
const CASES = [
  ['MsgCall', { __typename: 'MsgCall', caller: 'g1caller' }, 'g1caller'],
  ['MsgRun', { __typename: 'MsgRun', caller: 'g1runner' }, 'g1runner'],
  ['MsgAddPackage', { __typename: 'MsgAddPackage', creator: 'g1creator' }, 'g1creator'],
  ['BankMsgSend', { __typename: 'BankMsgSend', from_address: 'g1sender' }, 'g1sender'],
  ['MsgEnablePackage', { __typename: 'MsgEnablePackage', approver: 'g1approver' }, 'g1approver'],
  ['MsgRejectPackage', { __typename: 'MsgRejectPackage', sender: 'g1rejecter' }, 'g1rejecter'],
];

test('every message type resolves a signer', async ({ page }) => {
  await page.goto('/');
  await settle(page);

  for (const [name, value, want] of CASES) {
    const got = await page.evaluate(v => txSigner({ messages: [{ value: v }] }), value);
    expect(got, `${name} should resolve its signer`).toBe(want);
  }
});

// An unrecognised type must not render blank. A blank cell is indistinguishable
// from "this transaction had no signer", which is never true — and that silent
// degradation is exactly how the inert types went unnoticed.
test('an unknown message type still finds a signer', async ({ page }) => {
  await page.goto('/');
  await settle(page);

  const got = await page.evaluate(() =>
    txSigner({ messages: [{ value: { __typename: 'MsgSomethingNew', approver: 'g1future' } }] }));
  expect(got).toBe('g1future');
});

// A multicall can lead with a message this build cannot read a signer from
// while a later one names the account plainly. One transaction has one signer,
// so scanning on is correct rather than a guess.
test('a later message supplies the signer when the first cannot', async ({ page }) => {
  await page.goto('/');
  await settle(page);

  const got = await page.evaluate(() => txSigner({
    messages: [
      { value: { __typename: 'MsgUnknown' } },
      { value: { __typename: 'MsgCall', caller: 'g1real' } },
    ],
  }));
  expect(got).toBe('g1real');
});

// Stored rows carry no messages; the server resolves the signer for them.
test('a stored row falls back to its resolved caller', async ({ page }) => {
  await page.goto('/');
  await settle(page);

  const got = await page.evaluate(() => txSigner({ caller: 'g1stored' }));
  expect(got).toBe('g1stored');
});

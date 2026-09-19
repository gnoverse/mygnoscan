import { expect, test } from '@playwright/test';

import { settle, unexpected, watch } from './helpers.js';

// The parameters page renders four states that the backend deliberately keeps
// apart (set, empty, unset and error), and the whole value of the page rests
// on a reader being able to tell them apart at a glance. The Go side proves the
// classification is right; this proves the page does not flatten it again.
//
// The harness has no RPC (see EXPECTED_FAILURES), so the payload is stubbed
// here rather than fetched. That is the point: this file is about rendering,
// and a fixture is the only way to get all four states on one screen.

const PAYLOAD = {
  network: 'test',
  identity: {
    network: 'test',
    chain_id: 'gnoland-1',
    node_version: 'v1.0.0-rc.0',
    tx_index: 'off',
    latest_height: 168419,
    catching_up: false,
    rpc: 'https://rpc.example.invalid',
  },
  groups: [
    {
      title: 'deploy policy',
      explain: 'whether code that lands on this chain becomes callable.',
      params: [
        {
          key: 'vm:p:code_submission_policy',
          label: 'code submission policy',
          explain: 'parks every new package until an approver enables it.',
          state: 'set',
          raw: '"inert"',
          value: 'inert',
        },
        {
          key: 'vm:p:pkg_approvers',
          label: 'package approvers',
          explain: 'the addresses allowed to send MsgEnablePackage.',
          state: 'empty',
          raw: '[]',
          note: 'no approver is configured, so every parked package is frozen',
        },
        {
          key: 'vm:p:sysusers_pkgpath',
          label: 'sys users path',
          explain: 'a key this chain has never held.',
          state: 'unset',
        },
        {
          key: 'vm:p:broken',
          label: 'broken key',
          explain: 'a key the node refused to answer.',
          state: 'error',
          error: 'abci error: unknown request',
        },
      ],
    },
    {
      title: 'transfer restrictions',
      explain: 'whether GNOT can move, and for whom.',
      params: [
        {
          key: 'auth:p:unrestricted_addrs',
          label: 'unrestricted addresses',
          explain: 'accounts exempt from the transfer lock.',
          state: 'set',
          raw: JSON.stringify(Array.from({ length: 7 }, (_, i) => 'g1aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' + i)),
          list: Array.from({ length: 7 }, (_, i) => 'g1aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' + i),
        },
      ],
    },
  ],
  consensus: { max_gas: '3000000000', time_iota_ms: '100', pubkey_types: ['/tm.PubKeyEd25519'] },
  fetched_at: new Date().toISOString(),
};

async function stubParams(page, payload = PAYLOAD) {
  await page.route('**/api/params*', route =>
    route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(payload) }));
}

test('every parameter state renders as its own thing', async ({ page }) => {
  const seen = watch(page);
  await stubParams(page);

  await page.goto('/params');
  await settle(page);

  const content = page.locator('#params-content');

  // A configured value shows decoded, not as the JSON the chain returned.
  await expect(content).toContainText('inert');
  await expect(content).not.toContainText('"inert"');

  // Empty and unset are both "there is nothing here" to a careless renderer,
  // and they mean opposite things: an empty approver list freezes the chain's
  // deploy queue, an unset one is not a configuration at all.
  await expect(content).toContainText('empty');
  await expect(content).toContainText('unset');
  await expect(content).toContainText('no approver is configured, so every parked package is frozen');

  // A key that could not be read says so instead of reading as absent.
  await expect(content).toContainText('could not be read');
  await expect(content).toContainText('abci error: unknown request');

  // The identity bar carries what chain this is, which the page has to state
  // because the endpoint never answers for more than one network.
  await expect(content).toContainText('gnoland-1');
  await expect(content).toContainText('network test');

  expect(seen.jsErrors, 'uncaught exceptions').toEqual([]);
  expect(unexpected(seen.consoleErrors), 'console errors').toEqual([]);
});

test('a long list is abbreviated and expands in place', async ({ page }) => {
  const seen = watch(page);
  await stubParams(page);

  await page.goto('/params');
  await settle(page);

  // Counting rendered links rather than matching addresses: every address on
  // this page is abbreviated for display, so a full bech32 string never appears
  // in the text and an assertion against one passes whatever the toggle does.
  const content = page.locator('#params-content');
  // The full address lives in the link's title, which is how it stays checkable
  // while the visible text is abbreviated.
  const entries = content.locator('tr', { hasText: 'unrestricted addresses' })
    .locator('a[title^="g1aaaaaa"]');

  await expect(content).toContainText('7 entries');
  await expect(entries).toHaveCount(4);

  await content.getByText('show all 7').click();
  await expect(entries).toHaveCount(7);
  await expect(content).not.toContainText('show all 7');

  expect(seen.jsErrors, 'uncaught exceptions').toEqual([]);
});

// Without an RPC the endpoint answers 200 with an explanation rather than
// failing, and the page has to show that explanation. A blank page here would
// read as "this chain has no parameters", which is the one thing it must never
// say. The harness has no RPC configured, so this is the real path, unstubbed.
test('a network with no verified RPC says why it is empty', async ({ page }) => {
  const seen = watch(page);

  const response = await page.goto('/params');
  expect(response.status()).toBe(200);
  await settle(page);

  await expect(page.locator('#params-content')).toContainText('no verified RPC endpoint');

  expect(seen.jsErrors, 'uncaught exceptions').toEqual([]);
  expect(unexpected(seen.failedRequests), 'failed requests').toEqual([]);
});

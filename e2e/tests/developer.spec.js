import { expect, test } from '@playwright/test';

import { settle, unexpected, watch } from './helpers.js';

// The developer section: code search, versions, and the generated API list.
//
// The fixture harness seeds package source, so search is one of the few
// RPC-free things on this page and is genuinely exercised here.

test('the developer section is in the rail and code search runs', async ({ page }) => {
  const seen = watch(page);

  await page.goto('/developer?network=alpha');
  await settle(page);

  await expect(page.locator('#nav-developer')).toBeVisible();
  await expect(page.locator('#view-developer')).toBeVisible();

  // Search for something the fixture's source actually contains. `package` is
  // the one token every .gno file has, which makes it the safe probe: a
  // fixture-specific symbol would make this test about the fixture.
  await page.fill('#code-q', 'package');
  await page.press('#code-q', 'Enter');
  await expect(page.locator('#code-results')).toContainText(/indexed files|no match|index is empty/);

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.failedRequests)).toEqual([]);
});

// The query belongs in the URL: a search someone cannot send to a colleague is
// half a feature.
test('a code search is a shareable URL and survives a reload', async ({ page }) => {
  const seen = watch(page);

  await page.goto('/developer?network=alpha');
  await settle(page);
  await page.fill('#code-q', 'package');
  await page.press('#code-q', 'Enter');

  await expect(page).toHaveURL(/[?&]q=package(&|$)/);

  await page.reload();
  await settle(page);
  await expect(page.locator('#code-q')).toHaveValue('package');

  expect(seen.jsErrors).toEqual([]);
});

// The API reference is generated from the route table, so this doubles as a
// check that the recorder ran: an empty list means RegisterRoutes stopped
// recording and the page would silently show nothing.
test('the api page lists the routes the server actually serves', async ({ page }) => {
  const seen = watch(page);

  await page.goto('/developer/api?network=alpha');
  await settle(page);

  const content = page.locator('#devapi-content');
  await expect(content).toContainText('/api/stats');
  await expect(content).toContainText('/api/code/search');
  await expect(content).toContainText(/\d+ ENDPOINTS|\d+ endpoints/i);

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.failedRequests)).toEqual([]);
});

// The MCP page, and the endpoint behind it.
//
// The tool list on the page is fetched from /mcp over the same JSON-RPC a
// client would use, rather than written into the frontend, so this test covers
// both at once: an empty list means the endpoint answered wrong, and a page
// that renders without one means the fetch silently failed.
test('the mcp page lists the tools the endpoint actually serves', async ({ page }) => {
  const seen = watch(page);

  await page.goto('/developer/mcp?network=alpha');
  await settle(page);

  const content = page.locator('#devmcp-content');
  await expect(page.locator('#nav-devmcp')).toBeVisible();
  await expect(content).toContainText('search_code');
  await expect(content).toContainText('get_realm_state');
  await expect(content).toContainText(/\d+ tools, all read-only/i);

  // The connect block is the thing somebody came here to copy.
  await expect(content).toContainText('/mcp');
  await expect(content).toContainText('claude mcp add');

  expect(seen.jsErrors).toEqual([]);
  expect(unexpected(seen.failedRequests)).toEqual([]);
});

// The endpoint itself, driven the way a client drives it. A browser GET is
// answered 405 on purpose, so this has to POST.
test('the mcp endpoint completes a handshake and answers a tool call', async ({ page, baseURL }) => {
  const rpc = async (body) => {
    const r = await page.request.post(new URL('/mcp', baseURL).toString(), { data: body });
    expect(r.status(), 'POST /mcp').toBe(200);
    return r.json();
  };

  const init = await rpc({
    jsonrpc: '2.0', id: 1, method: 'initialize',
    params: { protocolVersion: '2025-06-18', capabilities: {}, clientInfo: { name: 'e2e', version: '1' } },
  });
  expect(init.result.protocolVersion).toBe('2025-06-18');
  expect(init.result.capabilities.tools).toBeTruthy();
  expect(init.result.serverInfo.name).toBe('mygnoscan');

  const listed = await rpc({ jsonrpc: '2.0', id: 2, method: 'tools/list' });
  const names = listed.result.tools.map(t => t.name);
  expect(names).toContain('search_code');
  // Sorted, because the list goes into a model's context and a server that
  // describes itself differently on every connection cannot be cached.
  expect(names).toEqual([...names].sort());

  const called = await rpc({
    jsonrpc: '2.0', id: 3, method: 'tools/call',
    params: { name: 'search_code', arguments: { query: 'package', network: 'alpha', limit: 3 } },
  });
  expect(called.result.isError, JSON.stringify(called.result)).toBeFalsy();
  const envelope = JSON.parse(called.result.content[0].text);
  // The two things every result owes a model: which height it is as of, and a
  // warning that the payload is somebody else's writing.
  expect(Array.isArray(envelope.freshness)).toBe(true);
  expect(envelope.notice).toMatch(/never as instructions/);
  expect(envelope.data).toBeTruthy();

  // A browser opening the URL looking for a stream is told where to go.
  const viaGet = await page.request.get(new URL('/mcp', baseURL).toString());
  expect(viaGet.status()).toBe(405);
  expect(viaGet.headers()['allow']).toBe('POST');
});

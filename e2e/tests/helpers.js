// watch attaches the assertions that matter on every page.
//
// #16's point is that backend tests cannot catch "the JSON was fine, the JS
// threw", so what this collects is the browser's own account of the page:
// uncaught exceptions, console errors, and requests that did not succeed.
export function watch(page) {
  const jsErrors = [];
  const consoleErrors = [];
  const failedRequests = [];

  page.on('pageerror', err => jsErrors.push(String(err)));
  page.on('console', msg => {
    if (msg.type() === 'error') consoleErrors.push(msg.text());
  });
  page.on('requestfailed', req => {
    failedRequests.push(`${req.url()} — ${(req.failure() || {}).errorText}`);
  });
  page.on('response', res => {
    if (res.status() >= 400) failedRequests.push(`${res.url()} — HTTP ${res.status()}`);
  });

  return { jsErrors, consoleErrors, failedRequests };
}

// Requests the harness cannot answer, and why.
//
// Every entry here is a claim that a failure is the *harness's* fault rather
// than the page's, so each one has to say what it is waiting on. Anything not
// listed fails the suite — which is the point: a page that starts calling an
// endpoint the backend no longer serves should break this, loudly.
export const EXPECTED_FAILURES = [
  // The account balance comes from RPC, and no RPC is configured here. The
  // fake indexer speaks GraphQL only.
  { pattern: /\/api\/address\/[^/]+\?.*balance/, why: 'balance needs RPC' },
];

export function unexpected(failures) {
  return failures.filter(f => !EXPECTED_FAILURES.some(e => e.pattern.test(f)));
}

// A page is "settled" when the SPA has finished its fetches and rendered. The
// app marks nothing, so this waits for the network to go quiet and for the
// loading skeletons to be gone — the shimmer that #115 found could otherwise
// sit there forever and read as a rendered page.
export async function settle(page) {
  await page.waitForLoadState('networkidle');
  await page.waitForFunction(
    () => document.querySelectorAll('.skeleton, .shimmer').length === 0,
    null,
    { timeout: 15_000 },
  ).catch(() => {});
}

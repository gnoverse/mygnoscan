// Where the suite listens, in one place.
//
// Playwright resolves `use.baseURL` when it loads the config, which is *before*
// globalSetup runs — so the `process.env.E2E_BASE_URL = baseURL` at the end of
// globalSetup comes too late to be read, and the two halves cannot agree that
// way. `E2E_PORT_OFFSET=137 npx playwright test`, which is what the README
// documents, therefore started the server on 9036 and pointed every test at
// 8899: green if anything at all was listening there, which on a machine
// already running a mygnoscan is exactly the case the offset exists for.
export const PORT = 8899 + Number(process.env.E2E_PORT_OFFSET || 0);
export const BASE_URL = process.env.E2E_BASE_URL || `http://127.0.0.1:${PORT}`;

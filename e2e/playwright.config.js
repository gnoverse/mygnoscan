import { defineConfig, devices } from '@playwright/test';

export default defineConfig({
  testDir: './tests',
  // The frontend is one file with no build step, so there is nothing to
  // parallelise against: one worker keeps the shared server's response cache
  // and the seeded database behaving predictably.
  workers: 1,
  fullyParallel: false,
  forbidOnly: !!process.env.CI,
  // A retry would hide exactly the kind of flake this suite exists to surface,
  // except in CI where a cold browser start can genuinely time out once.
  retries: process.env.CI ? 1 : 0,
  timeout: 60_000,
  reporter: process.env.CI ? [['list'], ['html', { open: 'never' }]] : [['list']],

  globalSetup: './harness/global-setup.mjs',
  globalTeardown: './harness/global-teardown.mjs',

  use: {
    baseURL: process.env.E2E_BASE_URL || 'http://127.0.0.1:8899',
    // What #16 asked for: enough to diagnose a failure without reproducing it.
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
    video: 'retain-on-failure',
  },

  projects: [
    { name: 'chromium', use: { ...devices['Desktop Chrome'], viewport: { width: 1400, height: 1000 } } },
  ],
});

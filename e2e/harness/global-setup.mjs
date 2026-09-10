// Builds the binary, starts it against a seeded database, and hands the tests
// a base URL.
//
// Order matters and is the reason this is a global setup rather than
// Playwright's `webServer`: the binary owns the schema, so it has to run once
// before anything can be inserted, and the tests must not start until the
// inserts are done. `webServer` only knows how to wait for a URL to answer,
// which it does before the fixture exists.
import { spawn } from 'node:child_process';
import { mkdirSync, rmSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import { startFakeIndexer } from './fake-indexer.mjs';
import { NETWORKS, seed } from './fixture.mjs';

const here = dirname(fileURLToPath(import.meta.url));
const e2eDir = join(here, '..');
const repoRoot = join(e2eDir, '..');
const tmpDir = join(e2eDir, '.tmp');

const run = (cmd, args, opts = {}) => new Promise((resolve, reject) => {
  const child = spawn(cmd, args, { stdio: 'inherit', ...opts });
  child.on('error', reject);
  child.on('exit', code => code === 0 ? resolve() : reject(new Error(`${cmd} exited ${code}`)));
});

async function waitFor(url, timeoutMs = 30000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    try {
      const res = await fetch(url);
      if (res.ok) return;
    } catch {
      // Not listening yet.
    }
    await new Promise(r => setTimeout(r, 200));
  }
  throw new Error(`timed out waiting for ${url}`);
}

export default async function globalSetup() {
  mkdirSync(tmpDir, { recursive: true });

  const binary = join(tmpDir, 'mygnoscan');
  await run('go', ['build', '-o', binary, '.'], { cwd: repoRoot });

  const indexer = await startFakeIndexer();

  const configPath = join(tmpDir, 'networks.json');
  writeFileSync(configPath, JSON.stringify({
    networks: NETWORKS.map(id => ({ id, indexer: indexer.url })),
  }, null, 2));

  // A fresh database every run. The suite asserts on exact counts, and a
  // database left over from a previous run is the classic way for a suite to
  // pass on evidence it did not produce.
  const dbPath = join(tmpDir, 'e2e.db');
  for (const suffix of ['', '-wal', '-shm']) rmSync(dbPath + suffix, { force: true });

  const port = 8899 + Number(process.env.E2E_PORT_OFFSET || 0);
  const baseURL = `http://127.0.0.1:${port}`;

  const server = spawn(binary, [
    '-db', dbPath,
    '-config', configPath,
    '-sync=false',
    '-listen', `127.0.0.1:${port}`,
  ], { cwd: repoRoot, stdio: ['ignore', 'pipe', 'pipe'] });

  const log = [];
  server.stdout.on('data', d => log.push(String(d)));
  server.stderr.on('data', d => log.push(String(d)));
  server.on('exit', code => {
    if (code !== 0 && code !== null) {
      console.error(`mygnoscan exited ${code}:\n${log.join('')}`);
    }
  });

  try {
    await waitFor(`${baseURL}/api/networks`);
  } catch (err) {
    server.kill('SIGKILL');
    throw new Error(`${err.message}\nserver output:\n${log.join('')}`);
  }

  seed(dbPath);

  // The response cache keys on path plus query and lives 30 seconds, so a
  // request made between startup and the seed would pin an empty answer for
  // longer than most of the suite takes to run. Nothing has asked yet — the
  // readiness probe above is /api/networks, which reads config rather than the
  // database — but this is worth knowing before adding a probe that does.
  process.env.E2E_BASE_URL = baseURL;
  writeFileSync(join(tmpDir, 'state.json'), JSON.stringify({ pid: server.pid, baseURL }));

  server.unref();
}

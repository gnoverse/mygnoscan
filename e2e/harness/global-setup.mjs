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
import { startShotStub } from './shot-stub.mjs';
import { NETWORKS, seed } from './fixture.mjs';
import { PORT } from './port.mjs';

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
  // The screenshot service, stubbed. The frontend only draws realm pictures
  // when one is configured, so without this the suite would be reviewing a
  // build with the feature switched off.
  const shots = await startShotStub();

  const configPath = join(tmpDir, 'networks.json');
  writeFileSync(configPath, JSON.stringify({
    // gnoweb is what turns a package path into the URL a picture is taken of.
    // It points at the stub here; nothing ever fetches it, but /api/shot
    // refuses a network that has none, which is the behaviour worth keeping.
    networks: NETWORKS.map(id => ({ id, indexer: indexer.url, gnoweb: shots.url })),
  }, null, 2));

  // A fresh database every run. The suite asserts on exact counts, and a
  // database left over from a previous run is the classic way for a suite to
  // pass on evidence it did not produce.
  const dbPath = join(tmpDir, 'e2e.db');
  for (const suffix of ['', '-wal', '-shm']) rmSync(dbPath + suffix, { force: true });

  const port = PORT;
  const baseURL = `http://127.0.0.1:${port}`;

  const server = spawn(binary, [
    '-db', dbPath,
    '-config', configPath,
    '-sync=false',
    '-listen', `127.0.0.1:${port}`,
    '-gnoshot', shots.url,
    // The fixture is seeded after the binary starts, so the index pass that
    // runs at startup finds an empty corpus. Production waits ten minutes for
    // the next one; the suite cannot.
    '-symbol-index-interval', '1s',
    // Same reason: the badge table is rebuilt from indexed history, and the
    // history arrives after the binary has already run its startup pass.
    '-achievement-interval', '1s',
    // No cache warmer, for the same reason one line up, and it is the reason
    // rather than a convenience: the warmer re-requests the landing endpoints
    // and stores what it gets, and here it would run before seed() has written
    // a row. /api/accounts is first in its sorted plan, so it cached the empty
    // answer, CacheStaleGrace kept serving it for the next fifteen minutes,
    // and the activity tab had no rows to rank.
    //
    // A database written behind the binary's back is exactly the case no
    // readiness check inside the binary can see, so the harness says so out
    // loud instead. The warmer's own behaviour is covered by the Go tests in
    // pkg/httpapi/warmer_test.go.
    '-warm-interval', '0',
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
  //
  // Nothing is written back to process.env here: the tests' base URL is settled
  // in harness/port.mjs, which the config reads long before this runs.
  writeFileSync(join(tmpDir, 'state.json'), JSON.stringify({ pid: server.pid, baseURL }));

  server.unref();
}

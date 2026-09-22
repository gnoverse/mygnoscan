// A stand-in for gnoshot, so the frontend's screenshot wiring can be tested and
// reviewed without a browser farm or a live chain.
//
// The e2e fixture's paths are synthetic — gno.land/r/hub/core exists on no
// chain — so a real capture service would 404 every one of them and the review
// screenshots would be twenty pictures of a fallback tile. This serves a small
// set of real captures instead, picked by a stable hash of the requested URL,
// so a given fixture path always gets the same picture and two runs produce
// identical images.
import { createHash } from 'node:crypto';
import { createServer } from 'node:http';
import { readFileSync, readdirSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const shotsDir = join(here, 'shots');

// Real captures of live mainnet realms, taken with gnoshot. Committed because
// the point of the review set is that it is identical run to run.
const NAMES = [...new Set(readdirSync(shotsDir)
  .filter(f => f.endsWith('.webp'))
  .map(f => f.replace(/\.(thumb|hero)\.webp$/, '')))].sort();

const cache = new Map();
function body(name, size) {
  const key = name + '.' + size;
  if (!cache.has(key)) cache.set(key, readFileSync(join(shotsDir, key + '.webp')));
  return cache.get(key);
}

// A stable hash rather than a counter: the same path must get the same picture
// whichever order the page happens to request its rows in.
function pick(url) {
  const h = createHash('sha256').update(url).digest();
  return NAMES[h.readUInt32BE(0) % NAMES.length];
}

// Paths the stub deliberately refuses, so the states that are not a picture are
// reviewable too. Matched as substrings of the requested gnoweb URL.
const PARKED = '/parked';
const MISSING = '/missing';

export async function startShotStub() {
  const server = createServer((req, res) => {
    const u = new URL(req.url, 'http://127.0.0.1');
    const target = u.searchParams.get('url') || '';
    if (u.pathname === '/meta') {
      res.writeHead(200, { 'content-type': 'application/json' });
      res.end(JSON.stringify({ url: target, matched: 'realm', truncated: false }));
      return;
    }
    if (u.pathname !== '/shot') {
      res.writeHead(404).end();
      return;
    }
    if (target.includes(MISSING)) {
      res.writeHead(503).end();
      return;
    }
    const size = u.searchParams.get('size') === 'hero' ? 'hero' : 'thumb';
    const name = target.includes(PARKED) ? NAMES[0] : pick(target);
    const b = body(name, size);
    res.writeHead(200, {
      'content-type': 'image/webp',
      'cache-control': 'public, max-age=31536000, immutable',
      'x-gnoshot-matched': 'realm',
      'content-length': b.length,
    });
    res.end(b);
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  const { port } = server.address();
  return { url: `http://127.0.0.1:${port}`, close: () => server.close() };
}

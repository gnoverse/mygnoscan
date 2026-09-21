// A tx-indexer stand-in, so the suite is offline and deterministic.
//
// The real client speaks a small slice of GraphQL and dispatches on what the
// query string contains — the same three cases the Go fake in
// fakeindexer_test.go covers. Answering them with a valid, empty payload is
// what keeps the indexer-backed endpoints returning "nothing here" instead of
// "the indexer is down", which would otherwise show up as failed requests in
// every page assertion and drown out the ones that mean something.
//
// It serves no transactions: everything the suite asserts on there is stored
// data, seeded directly into SQLite.
//
// Blocks are the exception. They exist only at the indexer — nothing persists a
// proposer — so a page built on them (/validators, and the liveness sparkline
// in particular) could not be asserted on at all while this answered with an
// empty list. So it now serves a small deterministic chain with a rotating
// proposer set.
import { createServer } from 'node:http';

// A deterministic chain. The proposers rotate unevenly on purpose: an even
// round-robin makes every validator's sparkline identical, which would hide a
// bug that keyed the bars to the wrong address.
export const PROPOSERS = ['g1val0000000000000000000000000000000', 'g1val1111111111111111111111111111111', 'g1val2222222222222222222222222222222'];
const CHAIN_LENGTH = 40;
const TIP = 1000 + CHAIN_LENGTH - 1;

// A money supply, so /storage has a denominator.
//
// Picked to make the *capacity* round rather than the supply, because capacity
// is what the page displays and what the assertions read: 10,737,418.24 GNOT at
// the default 100 ugnot/byte is exactly 100 GiB. A round figure in GNOT would
// have produced 93.1 GB on screen, which is the kind of number a reader of the
// test cannot check.
export const SUPPLY_CAPACITY_BYTES = 100 * 1024 * 1024 * 1024;
export const SUPPLY_UGNOT = String(SUPPLY_CAPACITY_BYTES * 100);

function blocks() {
  // Newest first, the order the real indexer returns for this query and the
  // order the frontend's sparkline relies on when it reverses for display.
  const out = [];
  for (let i = CHAIN_LENGTH - 1; i >= 0; i--) {
    out.push({
      height: 1000 + i,
      hash: `block-${1000 + i}`,
      chain_id: 'alpha-1',
      time: new Date(Date.UTC(2026, 7, 1, 0, i)).toISOString(),
      num_txs: 1,
      total_txs: i + 1,
      // 0,0,1,2 repeating: validator 0 proposes twice as often as the others.
      proposer_address_raw: PROPOSERS[[0, 0, 1, 2][i % 4]],
    });
  }
  return out;
}

export function startFakeIndexer() {
  const server = createServer((req, res) => {
    let body = '';
    req.on('data', chunk => { body += chunk; });
    req.on('end', () => {
      let query = '';
      try {
        query = JSON.parse(body || '{}').query || '';
      } catch {
        // A malformed body is the caller's problem, and answering an empty
        // payload keeps the failure on their side rather than turning it into
        // a transport error.
      }

      let data = {};
      if (query.includes('latestBlockHeight')) {
        data = { latestBlockHeight: TIP };
      } else if (query.includes('getBlocks')) {
        data = { getBlocks: blocks() };
      } else if (query.includes('getTransactions')) {
        data = { getTransactions: [] };
      } else if (query.includes('getSupply')) {
        data = {
          getSupply: {
            denom: 'ugnot', height: TIP, total: SUPPLY_UGNOT,
            locked: '9000000000000', spendable: '1000000000000',
          },
        };
      }

      res.writeHead(200, { 'content-type': 'application/json' });
      res.end(JSON.stringify({ data }));
    });
  });

  return new Promise(resolve => {
    server.listen(0, '127.0.0.1', () => {
      const { port } = server.address();
      resolve({ server, url: `http://127.0.0.1:${port}/graphql/query` });
    });
  });
}

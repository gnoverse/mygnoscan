// A tx-indexer stand-in, so the suite is offline and deterministic.
//
// The real client speaks a small slice of GraphQL and dispatches on what the
// query string contains — the same three cases the Go fake in
// fakeindexer_test.go covers. Answering them with a valid, empty payload is
// what keeps the indexer-backed endpoints returning "nothing here" instead of
// "the indexer is down", which would otherwise show up as failed requests in
// every page assertion and drown out the ones that mean something.
//
// It deliberately serves no rows. Everything the suite asserts on is stored
// data, seeded directly into SQLite; anything that can only come from an
// indexer is out of scope here and stays that way until this grows a real
// fixture chain.
import { createServer } from 'node:http';

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
        data = { latestBlockHeight: 0 };
      } else if (query.includes('getBlocks')) {
        data = { getBlocks: [] };
      } else if (query.includes('getTransactions')) {
        data = { getTransactions: [] };
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

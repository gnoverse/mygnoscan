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

// The realm page's storage, gas and events tabs read the *indexer* live, not
// SQLite, so an empty getTransactions left all three with nothing to draw and
// no way to assert on any of them. This serves one realm's history to the
// three queries those tabs make.
//
// Heights sit inside the range blocks() covers, so the API's stampBlockTimes
// resolves a real time for every row and the charts get a real x axis. They
// are spread across it on purpose: events an hour apart must land in different
// buckets, or a chart that buckets wrongly still looks right.
export const TAB_REALM = 'gno.land/r/hub/core';
export const STORAGE_PRICE = 100;

// Signed the way the chain signs them: an unlock's bytes_delta is negative and
// its fee_refund positive. Getting that backwards is what #255 fixed one layer
// down, and what the fee total row got wrong one layer up.
export const TAB_STORAGE = [
  { height: 1002, bytes: 4096 },
  { height: 1006, bytes: 2048 },
  { height: 1012, bytes: -1024 },
  { height: 1018, bytes: 8192 },
  { height: 1030, bytes: -2048 },
];
export const TAB_NET_BYTES = TAB_STORAGE.reduce((s, e) => s + e.bytes, 0);
export const TAB_DEPOSITED_FEE = TAB_STORAGE.filter(e => e.bytes > 0)
  .reduce((s, e) => s + e.bytes * STORAGE_PRICE, 0);
export const TAB_REFUNDED_FEE = TAB_STORAGE.filter(e => e.bytes < 0)
  .reduce((s, e) => s - e.bytes * STORAGE_PRICE, 0);
export const TAB_NET_FEE = TAB_DEPOSITED_FEE - TAB_REFUNDED_FEE;

// One failed transaction, so the gas chart's failed series is not empty and a
// chart that silently dropped unsuccessful transactions would be caught.
export const TAB_GAS = [
  { height: 1002, used: 90000, wanted: 150000, fee: 800, func: 'Write', success: true },
  { height: 1006, used: 60000, wanted: 150000, fee: 700, func: 'Write', success: true },
  { height: 1012, used: 30000, wanted: 150000, fee: 600, func: 'Clear', success: true },
  { height: 1018, used: 120000, wanted: 150000, fee: 900, func: 'Write', success: true },
  { height: 1024, used: 10000, wanted: 150000, fee: 500, func: 'Boom', success: false },
  { height: 1030, used: 40000, wanted: 150000, fee: 400, func: 'Clear', success: true },
];
export const TAB_GAS_USED = TAB_GAS.reduce((s, e) => s + e.used, 0);
export const TAB_GAS_FEE = TAB_GAS.reduce((s, e) => s + e.fee, 0);

export const TAB_EVENTS = [
  { height: 1002, type: 'Deposit' },
  { height: 1006, type: 'Deposit' },
  { height: 1012, type: 'Withdraw' },
  { height: 1018, type: 'Deposit' },
  { height: 1030, type: 'Withdraw' },
];

// Every one of those transactions also changed state, so the chain posted a
// storage deposit for the realm on each: GetEventsByPkgPath filters on
// pkg_path and not on typename, so they come back in the events payload too.
// Verified against mainnet on 2026-09-22: every row of
// /api/events/r/g1leu.../bubblerumble2 carries exactly this pair. The realm
// page hides them, and it can only be shown to hide them if they are here.
export const TAB_EVENT_STORAGE_BYTES = 2048;

// The realm's own bank account, and the transfers through it.
//
// TAB_REALM_ADDRESS is not a made-up string: it is what pkg/gnoaddr derives for
// TAB_REALM, and the defi endpoint asks this fake for transfers by that derived
// address. A fixture with a decorative address would answer every query with
// nothing and the tab would test as permanently empty.
export const TAB_REALM_ADDRESS = 'g1qql00vm7xf0mydz74md9c57tuv34znm8wm9nxu';
export const TAB_FUNDER = 'g1defifunder00000000000000000000000000';
export const TAB_PAYEE = 'g1defipayee000000000000000000000000000';

// In and out, so the running balance goes up and comes back down: a chart that
// ignored the sign of a leg still ends on the right total if every leg is a
// receipt, and only a fixture that spends catches it.
export const TAB_TRANSFERS = [
  { height: 1002, from: TAB_FUNDER, to: TAB_REALM_ADDRESS, amount: 5000000 },
  { height: 1006, from: TAB_FUNDER, to: TAB_REALM_ADDRESS, amount: 3000000 },
  { height: 1012, from: TAB_REALM_ADDRESS, to: TAB_PAYEE, amount: 1000000 },
  { height: 1018, from: TAB_FUNDER, to: TAB_REALM_ADDRESS, amount: 2500000 },
  { height: 1030, from: TAB_REALM_ADDRESS, to: TAB_PAYEE, amount: 500000 },
];
export const TAB_NET_UGNOT = TAB_TRANSFERS.reduce(
  (s, t) => s + (t.to === TAB_REALM_ADDRESS ? t.amount : -t.amount), 0);

// One package lifecycle, so the realm page's info tab has a submission history
// to draw and the fold of the old inert tab into info is assertable.
//
// Only the GraphQL half of /api/inert/package reaches this. The status half is
// a live vm/qpkgmeta_json read, and no RPC is configured here, so the handler
// reports "absent" and the info table's submission row is correctly left out
// — which is the other half of what the test checks.
export const LIFECYCLE_SUBMITTED_HEIGHT = 1001;
export const LIFECYCLE_ENABLED_HEIGHT = 1004;
export const LIFECYCLE_CREATOR = 'g1lifecyclecreator000000000000000000';
export const LIFECYCLE_APPROVER = 'g1lifecycleapprover00000000000000000';

// Most of these are asked ordered heightAndIndex DESC, so the fake answers them
// that way: a consumer that forgot to sort chronologically before a running
// total draws the realm shrinking as it grew, and only a newest-first fixture
// catches it.
function desc(rows) {
  return rows.slice().reverse();
}

// The coin-flow walk is the exception: it asks ASC, because that is the only
// direction a block-height cursor can resume in. Honouring the order the query
// actually asked for is what keeps the fixture from quietly proving the
// opposite of what production does.
function ordered(rows, query) {
  return query.includes('ASC') ? rows.slice() : desc(rows);
}

function storageTxs(pkgPath) {
  return desc(TAB_STORAGE.map((e, i) => ({
    hash: `storage-tab-${i}`,
    block_height: e.height,
    gas_used: 0,
    gas_wanted: 0,
    gas_fee: { amount: 0, denom: 'ugnot' },
    success: true,
    response: {
      events: [e.bytes >= 0 ? {
        __typename: 'StorageDepositEvent',
        type: 'StorageDepositEvent',
        bytes_delta: e.bytes,
        fee_delta: { amount: e.bytes * STORAGE_PRICE, denom: 'ugnot' },
        pkg_path: pkgPath,
      } : {
        __typename: 'StorageUnlockEvent',
        type: 'StorageUnlockEvent',
        bytes_delta: e.bytes,
        fee_refund: { amount: -e.bytes * STORAGE_PRICE, denom: 'ugnot' },
        pkg_path: pkgPath,
      }],
    },
  })));
}

function transferTxs(query) {
  return ordered(TAB_TRANSFERS.map((t, i) => ({
    hash: `transfer-${i}`,
    block_height: t.height,
    success: true,
    response: {
      events: [{
        __typename: 'TransferEvent',
        from: t.from,
        to: t.to,
        coins: `${t.amount}ugnot`,
      }],
    },
  })), query);
}

function gasTxs(pkgPath) {
  return desc(TAB_GAS.map((e, i) => ({
    hash: `gas-tab-${i}`,
    block_height: e.height,
    gas_used: e.used,
    gas_wanted: e.wanted,
    gas_fee: { amount: e.fee, denom: 'ugnot' },
    success: e.success,
    messages: [{ value: { __typename: 'MsgCall', func: e.func, pkg_path: pkgPath } }],
  })));
}

function lifecycleTxs(pkgPath) {
  return desc([
    {
      hash: 'lifecycle-submitted',
      block_height: LIFECYCLE_SUBMITTED_HEIGHT,
      gas_used: 0,
      gas_wanted: 0,
      gas_fee: { amount: 0, denom: 'ugnot' },
      success: true,
      messages: [{
        value: {
          __typename: 'MsgAddPackage',
          creator: LIFECYCLE_CREATOR,
          package: { name: 'core', path: pkgPath, files: [] },
        },
      }],
    },
    {
      hash: 'lifecycle-enabled',
      block_height: LIFECYCLE_ENABLED_HEIGHT,
      gas_used: 0,
      gas_wanted: 0,
      gas_fee: { amount: 0, denom: 'ugnot' },
      success: true,
      messages: [{
        value: {
          __typename: 'MsgEnablePackage',
          pkg_path: pkgPath,
          approver: LIFECYCLE_APPROVER,
          pkg_height: LIFECYCLE_SUBMITTED_HEIGHT,
        },
      }],
    },
  ]);
}

function eventTxs(pkgPath) {
  return desc(TAB_EVENTS.map((e, i) => ({
    hash: `event-tab-${i}`,
    block_height: e.height,
    gas_used: 0,
    gas_wanted: 0,
    gas_fee: { amount: 0, denom: 'ugnot' },
    success: true,
    response: {
      events: [{
        __typename: 'GnoEvent',
        type: e.type,
        pkg_path: pkgPath,
        attrs: [{ key: 'n', value: String(i) }],
      }, {
        __typename: 'StorageDepositEvent',
        type: 'StorageDepositEvent',
        bytes_delta: TAB_EVENT_STORAGE_BYTES,
        fee_delta: { amount: TAB_EVENT_STORAGE_BYTES * STORAGE_PRICE, denom: 'ugnot' },
        pkg_path: pkgPath,
      }],
    },
  })));
}

// The three queries are told apart by their where clause rather than by the
// fields they select: GetEventsByPkgPath asks for the storage fragments too,
// so dispatching on "StorageDepositEvent" appearing anywhere would answer the
// events query with storage rows.
const PKG_PATH_RE = /pkg_path: \{ eq: "([^"]+)" \}/;

function askedPath(query) {
  const m = PKG_PATH_RE.exec(query);
  return m ? m[1] : '';
}

// The defi query is the one filtered by address rather than by path, so it has
// its own extractor. Any of the four TransferEvent clauses will do: they are
// the same two addresses repeated to/from.
const TRANSFER_ADDR_RE = /TransferEvent: \{ (?:to|from): \{ eq: "([^"]+)" \} \}/;

function askedTransferAddress(query) {
  const m = TRANSFER_ADDR_RE.exec(query);
  return m ? m[1] : '';
}

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
      if (query.includes('__type(name:')) {
        // The schema probe, answered the way a real gno.land indexer answers it.
        //
        // This branch did not exist, so every probe came back `{data:{}}` and
        // the client read every optional type as absent. That was invisible
        // while the only consequence was trimming fragments the fixture never
        // asserted on. It stopped being invisible when TransferEvent became a
        // gated type: an absent answer there means "this chain cannot be asked
        // about coins", and the whole defi tab correctly refused to render.
        //
        // Mainnet's shape, checked 2026-09-23: TransferEvent and
        // MsgEnablePackage defined, MsgCreateSession not. The NoInertTypes and
        // NoSessionTypes overrides above still take precedence, so a test can
        // still ask for an older chain.
        const known = ['TransferEvent', 'MsgEnablePackage'];
        const name = known.find(n => query.includes(`__type(name: "${n}")`));
        data = { __type: name ? { name } : null };
      } else if (query.includes('latestBlockHeight')) {
        data = { latestBlockHeight: TIP };
      } else if (query.includes('getBlocks')) {
        data = { getBlocks: blocks() };
      } else if (query.includes('TransferEvent: {')) {
        data = {
          getTransactions: askedTransferAddress(query) === TAB_REALM_ADDRESS
            ? transferTxs(query) : [],
        };
      } else if (query.includes('StorageDepositEvent: { pkg_path: { eq:')) {
        data = { getTransactions: askedPath(query) === TAB_REALM ? storageTxs(TAB_REALM) : [] };
      } else if (query.includes('MsgCall: { pkg_path: { eq:')) {
        data = { getTransactions: askedPath(query) === TAB_REALM ? gasTxs(TAB_REALM) : [] };
      } else if (query.includes('GnoEvent: { pkg_path: { eq:')) {
        data = { getTransactions: askedPath(query) === TAB_REALM ? eventTxs(TAB_REALM) : [] };
      } else if (query.includes('MsgEnablePackage: { pkg_path: { eq:')) {
        // One path's lifecycle, not the chain-wide enable/reject feed behind
        // /api/inert/history: that one asks for `MsgEnablePackage: {}` with no
        // path filter, so it still falls through to the empty answer below.
        data = { getTransactions: askedPath(query) === TAB_REALM ? lifecycleTxs(TAB_REALM) : [] };
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

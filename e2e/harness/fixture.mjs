// The seeded chain the suite runs against.
//
// Rows are written straight into the SQLite file the binary has already created,
// rather than through the syncer, so the fixture is a fact rather than the
// outcome of a sync. The schema is never restated here — the binary owns it, and
// duplicating it in JavaScript would let the two drift silently.
import { DatabaseSync } from 'node:sqlite';

export const NETWORKS = ['alpha', 'beta'];

// One heavily-depended-upon realm, which is what makes the dependency graph
// dense enough for the label collision assertions to mean anything. Sixty
// dependents is well past the point where every label fits.
export const HUB = 'gno.land/r/hub/core';
export const HUB_ROUTE = 'r/hub/core';
export const DEPENDENTS = 60;
export const SHARED_PACKAGES = 12;

export const HUB_CREATOR = 'g1hubcreator00000000000000000000000000';

// The directory entries the fixture deploys. Real paths out of
// pkg/registry/data/apps.json, because the registry ships inside the binary
// and a made-up path would simply never match an entry.
export const APP_BUSY = 'gno.land/r/gnoland/blog';
export const APP_QUIET = 'gno.land/r/gnoland/wugnot';
export const APP_ELSEWHERE = 'gno.land/r/gov/dao';

// A pure package with a symbol table worth an outline. Kept beside the source
// it is generated from so the two cannot drift: the counts below are what the
// analyzer extracts from LIBRARY_SOURCE, and a test asserts they still are.
export const LIBRARY = 'gno.land/p/hub/toolkit';
export const LIBRARY_ROUTE = 'p/hub/toolkit';
export const LIBRARY_SOURCE = `// Package toolkit is the fixture's large package.
package toolkit

// MaxDepth bounds a walk.
const MaxDepth = 32

// MinDepth is the floor.
const MinDepth = 1

// Registry is the package-level store.
var Registry = map[string]int{}

// Fallback is used when a lookup misses.
var Fallback = "none"

// Tree is a sorted map.
type Tree struct{ root *node }

// Get returns the value at key.
func (t *Tree) Get(key string) (int, bool) { return 0, false }

// Set writes a value.
func (t *Tree) Set(key string, value int) {}

// Size counts the entries.
func (t *Tree) Size() int { return 0 }

// Iterator walks a Tree.
type Iterator struct{ pos int }

// Next advances the iterator.
func (i *Iterator) Next() bool { return false }

// Reset returns the iterator to the start.
func (i *Iterator) Reset() {}

// NewTree builds an empty Tree.
func NewTree() *Tree { return nil }

// Walk visits every entry.
func Walk(t *Tree, cb func(string, int)) {}

// Merge combines two trees.
func Merge(a *Tree, b *Tree) *Tree { return nil }

// Validate reports whether a tree is well formed.
func Validate(t *Tree) error { return nil }

func unexportedHelper() {}
`;

// What LIBRARY_SOURCE declares: 2 consts, 2 vars, 2 types with 3 and 2 methods,
// 4 exported functions and 1 unexported one.
export const LIBRARY_EXPORTED_SYMBOLS = 2 + 2 + 2 + 3 + 2 + 4;
export const LIBRARY_UNEXPORTED_SYMBOLS = 1;

// The hub's own account, as pkg/gnoaddr derives it from HUB. Pinned rather than
// computed because the fixture is JavaScript and the derivation is Go: if the
// two ever disagree, the defi tab's assertions are what says so.
export const HUB_ADDRESS = 'g1qql00vm7xf0mydz74md9c57tuv34znm8wm9nxu';

// One GRC20 asset the hub holds a position in. The key is the chain's own shape,
// `<realm path>.<name>.<id>`, because TokenKeyParts splits from the right and a
// bare symbol takes a different branch.
export const GRC20_REALM = 'gno.land/r/hub/token';
export const GRC20_TOKEN = 'gno.land/r/hub/token.hubcoin.0';
export const GRC20_FUNDER = 'g1grc20funder0000000000000000000000000';
// Two ordinary receipts plus a direct mint, which is the leg with no sender at
// all: the transfers table has to name that as a mint rather than leave the
// counterparty cell blank, which would read as a value the indexer lost.
export const GRC20_IN = 750000;
export const GRC20_OUT = 150000;
export const GRC20_BALANCE = GRC20_IN - GRC20_OUT;
export const BUSY_CALLER = 'g1busycaller0000000000000000000000000';

// Two addresses that each call the same two realms, so the contracts map has a
// shared-caller edge heavy enough to survive its default minimum of two
// addresses in common. BUSY_CALLER alone cannot produce one: an address that
// touches a single realm shares it with nothing.
export const PAIR_CALLERS = [
  'g1paircaller10000000000000000000000000',
  'g1paircaller20000000000000000000000000',
];
export const PAIRED_REALMS = ['gno.land/r/consumer00/app', 'gno.land/r/consumer01/app'];

// A realm built for the calls tab's aggregates: several callers with
// different habits, a multicall, a failure, a MsgRun, and one exported
// function nobody has ever called.
//
// Its own realm rather than more traffic on HUB because the numbers here are
// asserted exactly, and HUB's counts are already pinned by the graph and
// storage suites.
export const USAGE_REALM = 'gno.land/r/rumble/game';
export const USAGE_ROUTE = 'r/rumble/game';
export const USAGE_CREATOR = 'g1rumbledev00000000000000000000000000';
// Exported in the source; Withdraw is deliberately never called, which is what
// the info tab's dimmed pill means.
export const USAGE_EXPORTED = ['Bid', 'Claim', 'Render', 'Withdraw'];
// caller -> [messages, transactions]. g1rumble1 signs one transaction carrying
// two Bids, which is why its two numbers differ.
export const USAGE_CALLERS = {
  'g1rumble100000000000000000000000000000': [4, 3],
  'g1rumble200000000000000000000000000000': [2, 2],
  'g1rumble300000000000000000000000000000': [1, 1],
  'g1rumblerun000000000000000000000000000': [1, 1],
};
export const USAGE_MESSAGES = 8;
export const USAGE_TXS = 7;
export const USAGE_FAILED = 1;
export const USAGE_BID_CALLS = 6;
export const USAGE_BID_CALLERS = 3;

// Storage. The numbers are round so the /storage assertions can name them:
// alpha holds 40 MiB across three namespaces, against the fake indexer's
// 100 GB of capacity, which is 0.04% full.
//
// Deliberately uneven across the four attribution branches, because that is the
// part of the storage map that can be silently wrong: a payer lookup that
// misses does not error, it reports a plausible address.
export const STORAGE_TOTAL_BYTES = 40 * 1024 * 1024;
export const STORAGE_HOG = 'gno.land/r/hog/vault';
export const STORAGE_HOG_BYTES = 32 * 1024 * 1024;
export const STORAGE_PAYER = 'g1storagepayer00000000000000000000000';
export const STORAGE_DEPLOYER = 'g1hogdeployer000000000000000000000000';

// Block times, one per height, rather than one constant for the whole fixture.
//
// A single timestamp is merely dull in a table and fatal in a chart: every
// time series drawn over this fixture collapsed into a single bucket, so a
// chart that bucketed correctly and one that did not drew the same picture and
// no assertion could tell them apart. One minute per block, which is roughly
// gno.land's own cadence, and the same cadence the fake indexer's blocks use.
//
// Deliberately a fixed date in the past, and the recent tail below is the
// counterweight. Several tests turn on a window being *empty* (the storage
// panel has to say it found nothing at 30d and something at 90d), which only a
// chain older than those windows can prove. A fixture that is always recent and
// one that is always ancient are equally untestable; this one is both.
const GENESIS_MS = Date.UTC(2026, 7, 1, 12, 0, 0);
export const BLOCK_MS = 60000;
export function blockTime(height) {
  return new Date(GENESIS_MS + height * BLOCK_MS).toISOString();
}

// --- the recent tail --------------------------------------------------------
//
// A slice of activity stamped relative to now rather than to GENESIS_MS, so a
// windowed page has something to show. /api/pulse asks for the last hour, day
// or week; against the ancient chain above it answered "nothing happened", five
// panels rendered their empty state, and no assertion could tell that apart
// from a broken query.
//
// Kept deliberately self-contained — its own library, its own two realms, its
// own deployer, callers and token, none of them referenced anywhere else — so
// that adding it cannot move a count some other test asserts on. Heights sit
// far above everything else for the same reason.
//
// No storage events: the storage panel's empty state is asserted at 30d, and a
// recent event would silently fill it.
export const RECENT_BASE = 9000;
export function recentTime(minutesAgo) {
  return new Date(Date.now() - minutesAgo * 60000).toISOString();
}

export const FRESH_LIB = 'gno.land/p/fresh/kit';
export const FRESH_REALM = 'gno.land/r/fresh/app';
export const FRESH_SHOP = 'gno.land/r/fresh/shop';
// An older package importing FRESH_LIB, so the hot-libraries panel can show a
// window figure that differs from the all-time one — which is the whole point
// of printing both: 2 of 3 is adoption, 2 of 300 is noise.
export const FRESH_LEGACY = 'gno.land/r/fresh/legacy';
export const FRESH_DEV = 'g1freshdev000000000000000000000000000';
export const FRESH_VETERAN = 'g1freshveteran0000000000000000000000';
export const FRESH_CALLER = 'g1freshcaller00000000000000000000000';
export const FRESH_WHALE = 'g1freshwhale000000000000000000000000';
export const FRESH_TOKEN = 'gno.land/r/fresh/app.freshcoin.0';
// What pkg/gnoaddr derives for FRESH_REALM, not a decorative string: the page
// resolves a transfer's ends by deriving every known path forward and matching,
// so a made-up address would leave the realm unnamed while the test still
// passed on "an address rendered".
export const FRESH_REALM_ADDRESS = 'g165ajlk4c06fxcp54fms89668s9h0dtjuz889zq';
// The largest ugnot move in the tail, paid out of the realm's own account.
export const FRESH_PAYOUT_UGNOT = 42000000000;

// The user registry, as r/sys/users records it. Named so the search-box test can
// assert the group without restating the strings.
//
// `hub` is deliberately the namespace of HUB and LIBRARY, so one query returns a
// user, a realm and a package at once -- which is the ordering the search box is
// asserted on. `hubbot` is the namesake that has deployed nothing, and `hubgone`
// is a tombstone: r/sys/users never frees a name.
export const USER_NAME = 'hub';
export const USER_BOT = 'hubbot';
export const USER_GONE = 'hubgone';

export function seed(dbPath) {
  const db = new DatabaseSync(dbPath);
  try {
    db.exec('BEGIN');

    const pkg = db.prepare(`INSERT OR REPLACE INTO packages
      (network, path, name, creator, block_height, block_time, tx_hash, is_realm, num_files)
      VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1)`);
    // A deploy writes both: packages for what is live at a path now, and
    // package_submissions for the message that put it there, one row per
    // message and never overwritten. Gas attribution and every other history
    // question read the second one.
    const sub = db.prepare(`INSERT OR REPLACE INTO package_submissions
      (network, tx_hash, msg_index, path, name, creator, block_height, block_time, is_realm, num_files, success)
      VALUES (?, ?, 0, ?, ?, ?, ?, ?, ?, 1, 1)`);
    const file = db.prepare(`INSERT OR REPLACE INTO package_files
      (network, package_path, file_name, body) VALUES (?, ?, ?, ?)`);
    const dep = db.prepare(`INSERT OR REPLACE INTO dependencies
      (network, package_path, import_path) VALUES (?, ?, ?)`);
    const call = db.prepare(`INSERT OR REPLACE INTO calls
      (network, tx_hash, block_height, block_time, caller, pkg_path, func_name, success)
      VALUES (?, ?, ?, ?, ?, ?, ?, 1)`);
    const run = db.prepare(`INSERT OR REPLACE INTO msg_runs
      (network, tx_hash, block_height, block_time, caller, source, success)
      VALUES (?, ?, ?, ?, ?, ?, 1)`);
    // ugnot_amount is the coin string parsed, and the fixture writes it
    // because the seed lands after the server has already opened the database:
    // the backfill migration that fills it on a real deployment has run by
    // then, so a row inserted here without it would sum as zero.
    const send = db.prepare(`INSERT OR REPLACE INTO bank_sends
      (network, tx_hash, block_height, block_time, from_address, to_address, amount, ugnot_amount, success)
      VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1)`);
    const tx = db.prepare(`INSERT OR REPLACE INTO transactions
      (network, tx_hash, block_height, block_time, gas_used, gas_wanted, gas_fee, success)
      VALUES (?, ?, ?, ?, ?, ?, ?, 1)`);
    const user = db.prepare(`INSERT OR REPLACE INTO users
      (network, name, address, tx_hash, block_height, block_time, alias, deleted)
      VALUES (?, ?, ?, ?, ?, ?, ?, ?)`);

    const addPackage = (network, path, creator, height, isRealm, txHash, body) => {
      const name = path.split('/').pop();
      const hash = txHash || `tx-${network}-${height}`;
      pkg.run(network, path, name, creator, height, blockTime(height), hash, isRealm ? 1 : 0);
      sub.run(network, hash, path, name, creator, height, blockTime(height), isRealm ? 1 : 0);
      file.run(network, path, `${name}.gno`,
        body || `package ${name}\n\nfunc Render(path string) string { return "${name}" }\n`);
      tx.run(network, txHash || `tx-${network}-${height}`, height, blockTime(height), 100000, 200000, 1000);
    };

    let height = 100;
    addPackage('alpha', HUB, HUB_CREATOR, height++, true);

    // The registry. Height 0 for the first, because that is where a third of
    // mainnet's registrations live and a genesis row is the one a
    // cursor-driven sync would miss.
    user.run('alpha', USER_NAME, HUB_CREATOR, 'tx-alpha-users', 0, blockTime(1), 0, 0);
    user.run('alpha', USER_BOT, 'g1hubbot0000000000000000000000000000', 'tx-alpha-users', 1, blockTime(1), 0, 0);
    user.run('alpha', USER_GONE, 'g1hubgone000000000000000000000000000', 'tx-alpha-users', 2, blockTime(2), 0, 1);

    // A package big enough to need navigating.
    //
    // Every other package here declares one function, which is a docs page
    // that fits on a screen and therefore exercises none of the machinery that
    // exists for the ones that do not. On mainnet r/gnoswap/staker declares 444
    // symbols across 25 types. This is the small version of that problem: each
    // kind of declaration, methods on two different types, and an unexported
    // one, so the outline, the nesting and the unexported toggle all have
    // something to act on.
    addPackage('alpha', LIBRARY, HUB_CREATOR, height++, false, null, LIBRARY_SOURCE);

    const shared = [];
    for (let i = 0; i < SHARED_PACKAGES; i++) {
      const path = `gno.land/p/common/util${String(i).padStart(2, '0')}`;
      shared.push(path);
      addPackage('alpha', path, 'g1packager0000000000000000000000000000', height++, false);
      dep.run('alpha', HUB, path);
    }

    for (let i = 0; i < DEPENDENTS; i++) {
      const path = `gno.land/r/consumer${String(i).padStart(2, '0')}/app`;
      // Two deployers only, so the dependents list has same-deployer churn to
      // group — the thing #115 asked the UI to make legible.
      addPackage('alpha', path, `g1consumer${i % 2}00000000000000000000000000`, height++, true);
      dep.run('alpha', path, HUB);
      for (let j = 0; j < 3; j++) dep.run('alpha', path, shared[(i + j) % shared.length]);
    }

    // Three of the curated directory's own paths, so /apps has something to
    // draw. The registry is embedded in the binary and is the real file, so
    // these have to be paths it actually lists; the point of seeding them is
    // that the page has one card in each of its three states.
    //
    //   APP_BUSY   deployed here and called, by more than one person
    //   APP_QUIET  deployed here and never called
    //   APP_ELSEWHERE is deliberately absent: "not on this chain" is a state
    //   the card draws differently from "nobody has used it", and getting
    //   those two confused is the way this page would start lying.
    addPackage('alpha', APP_BUSY, HUB_CREATOR, 700, true, 'tx-app-busy');
    addPackage('alpha', APP_QUIET, HUB_CREATOR, 701, true, 'tx-app-quiet');
    // Twenty days ago: inside the directory's default 30d window and outside
    // every window /api/pulse asks about (hour, day, week). The recent tail
    // below is deliberately self-contained so that adding to it cannot move a
    // count another test asserts on, and the same care applies here.
    for (let i = 0; i < 6; i++) {
      const h = 1700 + i;
      const when = recentTime(20 * 24 * 60 + i);
      call.run('alpha', `app-busy-${i}`, h, when, `g1appuser${i}0000000000000000000000000`, APP_BUSY, 'Render');
      tx.run('alpha', `app-busy-${i}`, h, when, 90000, 150000, 800);
    }

    // A second chain carrying the same package path, so anything that joins on
    // path alone rather than (path, network) shows up as wrong counts.
    addPackage('beta', HUB, 'g1betacreator000000000000000000000000', 500, true);
    addPackage('beta', 'gno.land/r/beta/only', 'g1betacreator000000000000000000000000', 501, true);

    for (let i = 0; i < 40; i++) {
      const network = NETWORKS[i % 2];
      const h = 1000 + i;
      call.run(network, `call-${network}-${i}`, h, blockTime(h), BUSY_CALLER, HUB, 'Render');
      tx.run(network, `call-${network}-${i}`, h, blockTime(h), 90000, 150000, 800);
    }
    let pairHeight = 1500;
    for (const caller of PAIR_CALLERS) {
      for (const path of PAIRED_REALMS) {
        call.run('alpha', `pair-${caller}-${pairHeight}`, pairHeight, blockTime(pairHeight), caller, path, 'Render');
        tx.run('alpha', `pair-${caller}-${pairHeight}`, pairHeight, blockTime(pairHeight), 90000, 150000, 800);
        pairHeight++;
      }
    }

    for (let i = 0; i < 5; i++) {
      run.run('alpha', `run-${i}`, 2000 + i, blockTime(2000 + i), BUSY_CALLER, 'package main\n\nfunc main() {}\n');
      tx.run('alpha', `run-${i}`, 2000 + i, blockTime(2000 + i), 50000, 60000, 500);
    }
    for (let i = 0; i < 20; i++) {
      const network = NETWORKS[i % 2];
      send.run(network, `send-${network}-${i}`, 3000 + i, blockTime(3000 + i), BUSY_CALLER,
        'g1recipient00000000000000000000000000', '1000000ugnot', 1000000);
      tx.run(network, `send-${network}-${i}`, 3000 + i, blockTime(3000 + i), 40000, 50000, 400);
    }

    // The calls-tab fixture. A dedicated prepared statement because this is
    // the only traffic in the fixture that is not uniformly successful and
    // single-message-per-transaction, which is exactly what the aggregates
    // have to get right.
    const usageCall = db.prepare(`INSERT OR REPLACE INTO calls
      (network, tx_hash, msg_index, block_height, block_time, caller, pkg_path, func_name, success)
      VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`);
    addPackage('alpha', USAGE_REALM, USAGE_CREATOR, 5000, true, 'usage-deploy');
    // Overwrites the generic body addPackage writes: the exported set is the
    // point here, and Withdraw has to be in the source and in no call.
    file.run('alpha', USAGE_REALM, 'game.gno',
      'package game\n\n' + USAGE_EXPORTED.map(f => `func ${f}() {}`).join('\n') + '\n');
    const [R1, R2, R3, RRUN] = Object.keys(USAGE_CALLERS);
    const usageRows = [
      // tx, msgIndex, height, caller, func, success
      ['usage-1', 0, 5001, R1, 'Bid', 1],
      ['usage-2', 0, 5002, R1, 'Bid', 1],
      ['usage-2', 1, 5002, R1, 'Bid', 1], // multicall: 2 messages, 1 transaction
      ['usage-3', 0, 5003, R2, 'Bid', 1],
      ['usage-4', 0, 5004, R2, 'Bid', 0], // the one failure
      ['usage-5', 0, 5005, R1, 'Claim', 1],
      ['usage-6', 0, 5006, R3, 'Bid', 1],
    ];
    for (const [hash, idx, h, caller, fn, ok] of usageRows) {
      usageCall.run('alpha', hash, idx, h, blockTime(h), caller, USAGE_REALM, fn, ok);
      tx.run('alpha', hash, h, blockTime(h), 70000, 100000, 700);
    }
    run.run('alpha', 'usage-run', 5007, blockTime(5007), RRUN, `import "${USAGE_REALM}"\n`);
    tx.run('alpha', 'usage-run', 5007, blockTime(5007), 70000, 100000, 700);

    // Storage events, the rows /storage is built on. Signed: an unlock
    // subtracts, so r/hub/core nets out below what it deposited.
    const storage = db.prepare(`INSERT OR REPLACE INTO storage_events
      (network, tx_hash, event_index, pkg_path, block_height, block_time, kind, bytes_delta, fee)
      VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`);
    const PRICE = 100;
    const store = (network, txHash, path, h, bytes, kind) => {
      storage.run(network, txHash, 0, path, h, blockTime(h), kind || (bytes > 0 ? 'deposit' : 'unlock'),
        bytes, bytes * PRICE);
      tx.run(network, txHash, h, blockTime(h), 90000, 150000, 800);
    };

    // One realm holding most of the disk, deployed by its own creator: the
    // "deployer pays" branch, and the run the block map is mostly made of.
    addPackage('alpha', STORAGE_HOG, STORAGE_DEPLOYER, 4000, true, 'store-hog-deploy');
    store('alpha', 'store-hog-deploy', STORAGE_HOG, 4000, STORAGE_HOG_BYTES);

    // The hub grows under a MsgCall and then releases part of it: the "direct
    // caller pays" branch, plus a negative delta.
    call.run('alpha', 'store-hub-grow', 4001, blockTime(4001), STORAGE_PAYER, HUB, 'Write');
    store('alpha', 'store-hub-grow', HUB, 4001, 6 * 1024 * 1024);
    call.run('alpha', 'store-hub-free', 4002, blockTime(4002), STORAGE_PAYER, HUB, 'Clear');
    store('alpha', 'store-hub-free', HUB, 4002, -2 * 1024 * 1024);

    // A cross-realm write: the call targets one realm, another one grows. Only
    // the "any caller on this transaction" fallback can attribute it.
    call.run('alpha', 'store-cross', 4003, blockTime(4003), BUSY_CALLER, HUB, 'Poke');
    store('alpha', 'store-cross', PAIRED_REALMS[0], 4003, 3 * 1024 * 1024);

    // A MsgRun script that allocates, and an event with nothing at all to
    // attribute it to, which must show as unattributed rather than as somebody.
    run.run('alpha', 'store-run', 4004, blockTime(4004), BUSY_CALLER, 'package main\n');
    store('alpha', 'store-run', PAIRED_REALMS[1], 4004, 512 * 1024);
    store('alpha', 'store-orphan', 'gno.land/r/orphan/lost', 4005, 512 * 1024);

    // The same path on the other chain, with different numbers, so anything
    // that groups by path alone reports a size belonging to neither chain.
    store('beta', 'store-beta-hog', STORAGE_HOG, 4000, 7 * 1024 * 1024);

    // GRC20 positions for the hub realm's own account, so the defi tab's token
    // half has something to reconstruct.
    //
    // The address is the one pkg/gnoaddr derives for HUB, not a decorative
    // string: the endpoint looks the ledger up by the derived address, and a
    // made-up one would leave the table permanently empty while the test still
    // passed on "no positions".
    //
    // An empty `from` is a mint, which is how the funder got its own supply,
    // and the realm both receives and spends so a balance that ignored the
    // direction of a leg would be caught.
    const token = db.prepare(`INSERT OR REPLACE INTO token_transfers
      (network, tx_hash, event_idx, token, pkg_path, from_addr, to_addr, value, block_height, block_time)
      VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`);
    const grc20 = (hash, idx, tok, from, to, value, h) =>
      token.run('alpha', hash, idx, tok, GRC20_REALM, from, to, value, h, blockTime(h));
    grc20('grc20-mint', 0, GRC20_TOKEN, '', GRC20_FUNDER, 1000000, 4100);
    grc20('grc20-in-1', 0, GRC20_TOKEN, GRC20_FUNDER, HUB_ADDRESS, 400000, 4101);
    grc20('grc20-in-2', 0, GRC20_TOKEN, GRC20_FUNDER, HUB_ADDRESS, 250000, 4102);
    grc20('grc20-out', 0, GRC20_TOKEN, HUB_ADDRESS, GRC20_FUNDER, 150000, 4103);
    grc20('grc20-mint-hub', 0, GRC20_TOKEN, '', HUB_ADDRESS, 100000, 4104);

    // --- the recent tail, stamped against the wall clock ---------------------
    //
    // See the comment on recentTime above for why this exists beside an ancient
    // chain rather than replacing it. Minutes-ago figures are chosen so the
    // default 24h window holds all of it and the 1h window holds none, which is
    // what lets the window control itself be asserted on.
    const freshDeploy = (path, creator, height, minutesAgo, isRealm) => {
      const name = path.split('/').pop();
      const hash = `fresh-deploy-${name}`;
      const when = recentTime(minutesAgo);
      pkg.run('alpha', path, name, creator, height, when, hash, isRealm ? 1 : 0);
      sub.run('alpha', hash, path, name, creator, height, when, isRealm ? 1 : 0);
      file.run('alpha', path, `${name}.gno`,
        `package ${name}\n\nfunc Render(path string) string { return "${name}" }\n`);
      tx.run('alpha', hash, height, when, 100000, 200000, 1000);
    };

    // FRESH_VETERAN deployed long ago and again just now; FRESH_DEV has never
    // deployed before, which is what the panel's "first deploy" badge claims.
    addPackage('alpha', FRESH_LEGACY, FRESH_VETERAN, 600, true);
    dep.run('alpha', FRESH_LEGACY, FRESH_LIB);

    freshDeploy(FRESH_LIB, FRESH_DEV, RECENT_BASE, 300, false);
    freshDeploy(FRESH_REALM, FRESH_DEV, RECENT_BASE + 1, 290, true);
    freshDeploy(FRESH_SHOP, FRESH_VETERAN, RECENT_BASE + 2, 280, true);
    dep.run('alpha', FRESH_REALM, FRESH_LIB);
    dep.run('alpha', FRESH_SHOP, FRESH_LIB);

    // Calls, in the window and in the one before it, so "vs before" is a
    // comparison rather than a "new" badge. One failure, because a panel that
    // cannot show a failed call hides the most interesting row on the page.
    const freshCall = (hash, height, minutesAgo, caller, path, fn, ok) => {
      usageCall.run('alpha', hash, 0, height, recentTime(minutesAgo), caller, path, fn, ok);
      tx.run('alpha', hash, height, recentTime(minutesAgo), 70000, 100000, 700);
    };
    for (let i = 0; i < 6; i++) {
      freshCall(`fresh-call-${i}`, RECENT_BASE + 10 + i, 240 - i * 30,
        i % 2 ? FRESH_CALLER : FRESH_WHALE, FRESH_REALM, 'Buy', i === 4 ? 0 : 1);
    }
    for (let i = 0; i < 2; i++) {
      freshCall(`fresh-shop-${i}`, RECENT_BASE + 20 + i, 200 - i * 20, FRESH_CALLER, FRESH_SHOP, 'List', 1);
    }
    // The previous 24h window: three calls yesterday, so today's six read as a
    // rise rather than as something that has never happened before.
    for (let i = 0; i < 3; i++) {
      freshCall(`fresh-prev-${i}`, RECENT_BASE + 30 + i, 1800 + i * 10, FRESH_CALLER, FRESH_REALM, 'Buy', 1);
    }

    // ugnot, including one payout out of the realm's own account: the largest
    // move in the tail, and the row that proves an address is resolved back to
    // the package that owns it.
    send.run('alpha', 'fresh-payout', RECENT_BASE + 40, recentTime(120),
      FRESH_REALM_ADDRESS, FRESH_WHALE, `${FRESH_PAYOUT_UGNOT}ugnot`, FRESH_PAYOUT_UGNOT);
    tx.run('alpha', 'fresh-payout', RECENT_BASE + 40, recentTime(120), 40000, 50000, 400);
    for (let i = 0; i < 5; i++) {
      const hash = `fresh-send-${i}`;
      const when = recentTime(260 - i * 40);
      send.run('alpha', hash, RECENT_BASE + 41 + i, when, FRESH_WHALE, FRESH_CALLER, '1500000ugnot', 1500000);
      tx.run('alpha', hash, RECENT_BASE + 41 + i, when, 40000, 50000, 400);
    }

    // A GRC20 token of its own, minted and then moved, including one leg into
    // the realm's account so both ends of the resolution are exercised.
    const fresh20 = (hash, from, to, value, height, minutesAgo) =>
      token.run('alpha', hash, 0, FRESH_TOKEN, FRESH_REALM, from, to, value, height, recentTime(minutesAgo));
    fresh20('fresh20-mint', '', FRESH_WHALE, 5000000, RECENT_BASE + 50, 270);
    fresh20('fresh20-1', FRESH_WHALE, FRESH_REALM_ADDRESS, 1200000, RECENT_BASE + 51, 230);
    fresh20('fresh20-2', FRESH_WHALE, FRESH_CALLER, 300000, RECENT_BASE + 52, 190);
    fresh20('fresh20-3', FRESH_REALM_ADDRESS, FRESH_CALLER, 90000, RECENT_BASE + 53, 150);

    db.exec('COMMIT');
  } finally {
    db.close();
  }
}

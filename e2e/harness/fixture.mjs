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
const GENESIS_MS = Date.UTC(2026, 7, 1, 12, 0, 0);
export const BLOCK_MS = 60000;
export function blockTime(height) {
  return new Date(GENESIS_MS + height * BLOCK_MS).toISOString();
}

export function seed(dbPath) {
  const db = new DatabaseSync(dbPath);
  try {
    db.exec('BEGIN');

    const pkg = db.prepare(`INSERT OR REPLACE INTO packages
      (network, path, name, creator, block_height, block_time, tx_hash, is_realm, num_files)
      VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1)`);
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
    const send = db.prepare(`INSERT OR REPLACE INTO bank_sends
      (network, tx_hash, block_height, block_time, from_address, to_address, amount, success)
      VALUES (?, ?, ?, ?, ?, ?, ?, 1)`);
    const tx = db.prepare(`INSERT OR REPLACE INTO transactions
      (network, tx_hash, block_height, block_time, gas_used, gas_wanted, gas_fee, success)
      VALUES (?, ?, ?, ?, ?, ?, ?, 1)`);

    const addPackage = (network, path, creator, height, isRealm, txHash) => {
      const name = path.split('/').pop();
      pkg.run(network, path, name, creator, height, blockTime(height), txHash || `tx-${network}-${height}`, isRealm ? 1 : 0);
      file.run(network, path, `${name}.gno`, `package ${name}\n\nfunc Render(path string) string { return "${name}" }\n`);
      tx.run(network, txHash || `tx-${network}-${height}`, height, blockTime(height), 100000, 200000, 1000);
    };

    let height = 100;
    addPackage('alpha', HUB, HUB_CREATOR, height++, true);

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
        'g1recipient00000000000000000000000000', '1000000ugnot');
      tx.run(network, `send-${network}-${i}`, 3000 + i, blockTime(3000 + i), 40000, 50000, 400);
    }

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

    db.exec('COMMIT');
  } finally {
    db.close();
  }
}

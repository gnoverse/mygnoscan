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

const TS = '2026-08-01T12:00:00Z';

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

    const addPackage = (network, path, creator, height, isRealm) => {
      const name = path.split('/').pop();
      pkg.run(network, path, name, creator, height, TS, `tx-${network}-${height}`, isRealm ? 1 : 0);
      file.run(network, path, `${name}.gno`, `package ${name}\n\nfunc Render(path string) string { return "${name}" }\n`);
      tx.run(network, `tx-${network}-${height}`, height, TS, 100000, 200000, 1000);
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
      call.run(network, `call-${network}-${i}`, h, TS, BUSY_CALLER, HUB, 'Render');
      tx.run(network, `call-${network}-${i}`, h, TS, 90000, 150000, 800);
    }
    for (let i = 0; i < 5; i++) {
      run.run('alpha', `run-${i}`, 2000 + i, TS, BUSY_CALLER, 'package main\n\nfunc main() {}\n');
      tx.run('alpha', `run-${i}`, 2000 + i, TS, 50000, 60000, 500);
    }
    for (let i = 0; i < 20; i++) {
      const network = NETWORKS[i % 2];
      send.run(network, `send-${network}-${i}`, 3000 + i, TS, BUSY_CALLER,
        'g1recipient00000000000000000000000000', '1000000ugnot');
      tx.run(network, `send-${network}-${i}`, 3000 + i, TS, 40000, 50000, 400);
    }

    db.exec('COMMIT');
  } finally {
    db.close();
  }
}

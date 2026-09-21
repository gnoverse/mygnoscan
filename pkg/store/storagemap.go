package store

import (
	"fmt"

	_ "modernc.org/sqlite"
)

// The storage map: who is holding the chain's bytes, and what it cost them.
//
// Every byte of realm state on gno.land is backed by a locked GNOT deposit, at
// `vm:p:storage_price` ugnot per byte. That makes the chain a disk with an
// arithmetic capacity (total supply divided by the price) rather than a
// metaphorical one, and it makes "who is using the space" a question with an
// exact answer rather than an estimate.
//
// The answer is already local. storage_events carries every StorageDepositEvent
// and StorageUnlockEvent the sync walk has seen, signed, so summing bytes_delta
// per realm reproduces what the chain itself reports for that realm: checked on
// mainnet 2026-09-21, gno.land/r/gnoland/blog sums to 1278609 here and
// `vm/qstorage` answers `storage: 1278609, deposit: 127860900`. Nothing in this
// file needs a per-realm RPC round trip.

// StorageCell is one realm's footprint.
//
// FirstHeight is what makes the map read like a defragmenter rather than a bar
// chart: laying cells out in the order their realms first took space shows the
// chain filling up over time, and shows one deployer's contiguous run for what
// it is.
type StorageCell struct {
	Network     string `json:"network"`
	PkgPath     string `json:"pkg_path"`
	Namespace   string `json:"namespace"`
	Deployer    string `json:"deployer"`
	Bytes       int    `json:"bytes"`
	Fee         int    `json:"fee"`
	Events      int    `json:"events"`
	FirstHeight int    `json:"first_height"`
	// FirstTime is the timestamp of FirstHeight, so the table can print the
	// age beside the height rather than making the reader open the block.
	// Empty for rows synced before storage_events carried block_time.
	FirstTime  string `json:"first_time,omitempty"`
	LastHeight int    `json:"last_height"`
}

// StoragePayer is one account's footprint, attributed to whoever actually paid.
//
// Not the same question as "who deployed the realm". The chain charges the
// message caller (`processStorageDeposit(ctx, caller, ...)` in
// gno.land/pkg/sdk/vm/keeper.go), so a realm anyone can write to is paid for by
// its users, and its deployer may hold almost none of its bytes.
type StoragePayer struct {
	Network string `json:"network"`
	Address string `json:"address"`
	Bytes   int    `json:"bytes"`
	Fee     int    `json:"fee"`
	Realms  int    `json:"realms"`
	Events  int    `json:"events"`
}

// StorageFootprint is the chain-wide total, so a reader is never adding up a
// truncated list to find out how full the disk is.
type StorageFootprint struct {
	Bytes  int `json:"bytes"`
	Fee    int `json:"fee"`
	Realms int `json:"realms"`
	Events int `json:"events"`
}

// StorageFootprintTotal sums every storage event on the network.
//
// Separate from StorageCells because that one is capped: a top-N list plus a
// total lets the map draw an honest "everything else" remainder instead of
// implying the chain is only as full as the rows that fit.
func (d *DB) StorageFootprintTotal(network string) (StorageFootprint, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var f StorageFootprint
	err := d.db.QueryRow(`
		SELECT COALESCE(SUM(bytes_delta), 0), COALESCE(SUM(fee), 0),
		       COUNT(DISTINCT pkg_path), COUNT(*)
		  FROM storage_events
		 WHERE `+d.networkFilter("network", network)).
		Scan(&f.Bytes, &f.Fee, &f.Realms, &f.Events)
	return f, err
}

// StorageCells returns the per-realm footprint, largest first.
//
// Grouped by (network, pkg_path) and not by path alone: 193 package paths exist
// on more than one chain, and merging them would produce a row whose size
// describes neither.
//
// The deployer comes from a LEFT JOIN rather than an inner one on purpose. A
// realm can have storage events without a packages row (genesis packages, and
// anything deployed before the sync window) and dropping those rows would
// quietly shrink the total the map is measured against.
func (d *DB) StorageCells(network string, limit int) ([]StorageCell, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if limit <= 0 {
		limit = 2000
	}

	rows, err := d.db.Query(`
		SELECT s.network, s.pkg_path,
		       COALESCE(SUM(s.bytes_delta), 0) AS bytes,
		       COALESCE(SUM(s.fee), 0),
		       COUNT(*), MIN(s.block_height), MAX(s.block_height),
		       COALESCE(MIN(NULLIF(s.block_time, '')), ''),
		       COALESCE(p.creator, '')
		  FROM storage_events s
		  LEFT JOIN packages p
		    ON p.network = s.network AND p.path = s.pkg_path
		 WHERE `+d.networkFilter("s.network", network)+`
		 GROUP BY s.network, s.pkg_path
		 ORDER BY bytes DESC, s.pkg_path ASC
		 LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []StorageCell
	for rows.Next() {
		var c StorageCell
		if err := rows.Scan(&c.Network, &c.PkgPath, &c.Bytes, &c.Fee,
			&c.Events, &c.FirstHeight, &c.LastHeight, &c.FirstTime, &c.Deployer); err != nil {
			return nil, err
		}
		c.Namespace = NamespaceOf(c.PkgPath)
		out = append(out, c)
	}
	return out, rows.Err()
}

// payerExpr resolves the account the chain charged for a storage event.
//
// Four correlated lookups in confidence order, each an index seek rather than a
// scan: idx_calls_network_hash, idx_packages_network_hash and
// idx_msg_runs_network_hash all already cover (network, tx_hash), so this needs
// no index of its own. Adding one is worse than redundant: the planner prefers
// the newer covering index and TestTopGasQueryUsesAnIndex, which pins the plan
// of an unrelated query over the same tables, starts naming it instead.
//
//  1. the MsgCall in this transaction that targeted this realm, the common case
//     and the only one that is certain;
//  2. this realm's own deploy, where the creator pays;
//  3. any MsgCall in the transaction, for a cross-realm write where the caller
//     touched realm A and realm B grew;
//  4. a MsgRun, whose script did the same.
//
// MIN() rather than a bare column: a multicall bundles several MsgCall rows
// under one tx_hash, and a join that returned all of them would multiply the
// event's bytes by the message count. A scalar subquery returns one row by
// construction, so the same bytes cannot be counted twice.
//
// Anything left unmatched groups under ” and is reported as unattributed
// rather than folded into an arbitrary account.
const payerExpr = `COALESCE(
	(SELECT MIN(c.caller) FROM calls c
	  WHERE c.network = s.network AND c.tx_hash = s.tx_hash AND c.pkg_path = s.pkg_path),
	(SELECT MIN(p.creator) FROM packages p
	  WHERE p.network = s.network AND p.tx_hash = s.tx_hash AND p.path = s.pkg_path),
	(SELECT MIN(c2.caller) FROM calls c2
	  WHERE c2.network = s.network AND c2.tx_hash = s.tx_hash),
	(SELECT MIN(m.caller) FROM msg_runs m
	  WHERE m.network = s.network AND m.tx_hash = s.tx_hash),
	'')`

// StoragePayers ranks accounts by the bytes they are paying to keep on chain.
func (d *DB) StoragePayers(network string, limit int) ([]StoragePayer, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if limit <= 0 {
		limit = 500
	}

	q := fmt.Sprintf(`
		SELECT network, addr,
		       COALESCE(SUM(bytes_delta), 0) AS bytes,
		       COALESCE(SUM(fee), 0),
		       COUNT(DISTINCT pkg_path), COUNT(*)
		  FROM (SELECT s.network, s.pkg_path, s.bytes_delta, s.fee,
		               %s AS addr
		          FROM storage_events s
		         WHERE %s)
		 GROUP BY network, addr
		 ORDER BY bytes DESC, addr ASC
		 LIMIT ?`, payerExpr, d.networkFilter("s.network", network))

	rows, err := d.db.Query(q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []StoragePayer
	for rows.Next() {
		var p StoragePayer
		if err := rows.Scan(&p.Network, &p.Address, &p.Bytes, &p.Fee,
			&p.Realms, &p.Events); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

package store

import (
	"testing"

	"github.com/moul/mygnoscan/pkg/config"
)

// seedStorageMap builds a two-chain fixture that exercises every attribution
// branch, because the payer lookup is the part that can silently get it wrong:
// a miss produces a plausible-looking address rather than an error.
//
//	alpha:
//	  r/ns1/a   deployed by deployer1, then grown by caller1 (a MsgCall)
//	            and by caller2 through a cross-realm write (a call to r/ns1/b)
//	  r/ns1/b   deployed by deployer1, then released some bytes
//	  r/ns2/c   grown by a MsgRun
//	  r/ns2/d   an orphan event: no call, no deploy, no run
//	beta:
//	  r/ns1/a   the same path on another chain, which must never merge
func seedStorageMap(t *testing.T, db *DB) {
	t.Helper()

	const when = "2026-01-01T00:00:00Z"
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	must(db.UpsertPackage("alpha", "gno.land/r/ns1/a", "a", "deployer1", "tx1", 10, when, true, 1))
	must(db.UpsertPackage("alpha", "gno.land/r/ns1/b", "b", "deployer1", "tx2", 11, when, true, 1))
	must(db.UpsertPackage("beta", "gno.land/r/ns1/a", "a", "deployer9", "tx9", 10, when, true, 1))

	// tx1 / tx2: the deploys. The creator pays.
	must(db.InsertStorageEvent("alpha", "tx1", 0, "gno.land/r/ns1/a", 10, when, "deposit", 1000, 100000))
	must(db.InsertStorageEvent("alpha", "tx2", 0, "gno.land/r/ns1/b", 11, when, "deposit", 500, 50000))

	// tx3: a MsgCall on r/ns1/a. caller1 pays for the bytes it wrote.
	must(db.InsertCall("alpha", "tx3", 12, 0, when, "caller1", "gno.land/r/ns1/a", "Grow", true))
	must(db.InsertStorageEvent("alpha", "tx3", 0, "gno.land/r/ns1/a", 12, when, "deposit", 300, 30000))

	// tx4: caller2 calls r/ns1/b, which writes into r/ns1/a. No call row names
	// r/ns1/a, so only the "any caller on this tx" branch can attribute it.
	must(db.InsertCall("alpha", "tx4", 13, 0, when, "caller2", "gno.land/r/ns1/b", "Poke", true))
	must(db.InsertStorageEvent("alpha", "tx4", 0, "gno.land/r/ns1/a", 13, when, "deposit", 200, 20000))

	// tx5: r/ns1/b frees bytes. Signed, so it subtracts.
	must(db.InsertCall("alpha", "tx5", 14, 0, when, "caller1", "gno.land/r/ns1/b", "Clear", true))
	must(db.InsertStorageEvent("alpha", "tx5", 0, "gno.land/r/ns1/b", 14, when, "unlock", -100, -10000))

	// tx6: a MsgRun grows a realm nobody called.
	must(db.InsertMsgRun("alpha", "tx6", 15, when, "runner1", "package main", true))
	must(db.InsertStorageEvent("alpha", "tx6", 0, "gno.land/r/ns2/c", 15, when, "deposit", 700, 70000))

	// tx7: nothing to attribute it to.
	must(db.InsertStorageEvent("alpha", "tx7", 0, "gno.land/r/ns2/d", 16, when, "deposit", 50, 5000))

	// beta carries the same path with different numbers.
	must(db.InsertStorageEvent("beta", "tx9", 0, "gno.land/r/ns1/a", 10, when, "deposit", 9999, 999900))
}

func newStorageMapDB(t *testing.T) *DB {
	t.Helper()
	db := NewTestDB(t)
	db.SetConfiguredNetworks([]config.NetworkConfig{{ID: "alpha"}, {ID: "beta"}})
	seedStorageMap(t, db)
	return db
}

func TestStorageCells(t *testing.T) {
	db := newStorageMapDB(t)

	cells, err := db.StorageCells("alpha", 0)
	if err != nil {
		t.Fatalf("StorageCells: %v", err)
	}

	got := map[string]StorageCell{}
	for _, c := range cells {
		if c.Network != "alpha" {
			t.Fatalf("beta row leaked into an alpha query: %+v", c)
		}
		got[c.PkgPath] = c
	}

	tests := []struct {
		path        string
		bytes       int
		fee         int
		events      int
		firstHeight int
		lastHeight  int
		namespace   string
		deployer    string
	}{
		// 1000 + 300 + 200, over three transactions.
		{"gno.land/r/ns1/a", 1500, 150000, 3, 10, 13, "ns1", "deployer1"},
		// 500 deposited, 100 released.
		{"gno.land/r/ns1/b", 400, 40000, 2, 11, 14, "ns1", "deployer1"},
		// No packages row: the deployer is empty rather than the row missing.
		{"gno.land/r/ns2/c", 700, 70000, 1, 15, 15, "ns2", ""},
		{"gno.land/r/ns2/d", 50, 5000, 1, 16, 16, "ns2", ""},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			c, ok := got[tt.path]
			if !ok {
				t.Fatalf("missing cell for %s", tt.path)
			}
			if c.Bytes != tt.bytes || c.Fee != tt.fee || c.Events != tt.events {
				t.Errorf("got bytes=%d fee=%d events=%d, want %d/%d/%d",
					c.Bytes, c.Fee, c.Events, tt.bytes, tt.fee, tt.events)
			}
			if c.FirstHeight != tt.firstHeight || c.LastHeight != tt.lastHeight {
				t.Errorf("got heights %d..%d, want %d..%d",
					c.FirstHeight, c.LastHeight, tt.firstHeight, tt.lastHeight)
			}
			if c.Namespace != tt.namespace || c.Deployer != tt.deployer {
				t.Errorf("got namespace=%q deployer=%q, want %q/%q",
					c.Namespace, c.Deployer, tt.namespace, tt.deployer)
			}
		})
	}

	if len(cells) != len(tests) {
		t.Errorf("got %d cells, want %d", len(cells), len(tests))
	}
	// Largest first, so the map can lay out the rows it is handed.
	for i := 1; i < len(cells); i++ {
		if cells[i-1].Bytes < cells[i].Bytes {
			t.Fatalf("cells are not sorted by size: %v", cells)
		}
	}
}

// A path deployed on two chains is two disks, not one. Ranking by path alone
// would report 11,499 bytes for an r/ns1/a that exists on neither chain.
func TestStorageCellsAreNetworkScoped(t *testing.T) {
	db := newStorageMapDB(t)

	for _, tt := range []struct {
		network string
		want    int
	}{{"alpha", 1500}, {"beta", 9999}} {
		cells, err := db.StorageCells(tt.network, 0)
		if err != nil {
			t.Fatalf("StorageCells(%s): %v", tt.network, err)
		}
		var got int
		for _, c := range cells {
			if c.PkgPath == "gno.land/r/ns1/a" {
				got = c.Bytes
			}
		}
		if got != tt.want {
			t.Errorf("%s: got %d bytes for r/ns1/a, want %d", tt.network, got, tt.want)
		}
	}
}

func TestStoragePayersAttributeEachBranch(t *testing.T) {
	db := newStorageMapDB(t)

	payers, err := db.StoragePayers("alpha", 0)
	if err != nil {
		t.Fatalf("StoragePayers: %v", err)
	}
	got := map[string]StoragePayer{}
	for _, p := range payers {
		got[p.Address] = p
	}

	tests := []struct {
		name    string
		address string
		bytes   int
		fee     int
		realms  int
	}{
		// Both deploys: 1000 + 500.
		{"deploy creator pays", "deployer1", 1500, 150000, 2},
		// tx3 grew r/ns1/a by 300, tx5 released 100 from r/ns1/b.
		{"direct caller pays, and a release refunds", "caller1", 200, 20000, 2},
		// tx4 targeted r/ns1/b but grew r/ns1/a: only the fallback can see it.
		{"cross-realm write falls back to the tx caller", "caller2", 200, 20000, 1},
		{"MsgRun script pays", "runner1", 700, 70000, 1},
		// Better an explicit hole than a wrong address.
		{"nothing to attribute it to", "", 50, 5000, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, ok := got[tt.address]
			if !ok {
				t.Fatalf("no row for payer %q; got %+v", tt.address, payers)
			}
			if p.Bytes != tt.bytes || p.Fee != tt.fee || p.Realms != tt.realms {
				t.Errorf("got bytes=%d fee=%d realms=%d, want %d/%d/%d",
					p.Bytes, p.Fee, p.Realms, tt.bytes, tt.fee, tt.realms)
			}
		})
	}
	if len(payers) != len(tests) {
		t.Errorf("got %d payers, want %d: %+v", len(payers), len(tests), payers)
	}
}

// A multicall bundles several MsgCall rows under one tx_hash. A join would
// return all of them and multiply the event's bytes by the message count;
// the scalar subquery must return exactly one.
func TestStoragePayersDoNotDoubleCountMulticalls(t *testing.T) {
	db := NewTestDB(t)
	db.SetConfiguredNetworks([]config.NetworkConfig{{ID: "alpha"}})

	const when = "2026-01-01T00:00:00Z"
	for i := range 4 {
		if err := db.InsertCall("alpha", "txm", 20, i, when, "caller1", "gno.land/r/ns1/a", "Grow", true); err != nil {
			t.Fatalf("InsertCall: %v", err)
		}
	}
	if err := db.InsertStorageEvent("alpha", "txm", 0, "gno.land/r/ns1/a", 20, when, "deposit", 400, 40000); err != nil {
		t.Fatalf("InsertStorageEvent: %v", err)
	}

	payers, err := db.StoragePayers("alpha", 0)
	if err != nil {
		t.Fatalf("StoragePayers: %v", err)
	}
	if len(payers) != 1 {
		t.Fatalf("got %d payers, want 1: %+v", len(payers), payers)
	}
	if payers[0].Bytes != 400 || payers[0].Realms != 1 || payers[0].Events != 1 {
		t.Errorf("got %+v, want 400 bytes over 1 realm and 1 event", payers[0])
	}
}

// The totals are what the capacity figure is measured against, so they must
// cover every realm, including the ones a truncated cell list drops.
func TestStorageFootprintTotalCoversTruncatedCells(t *testing.T) {
	db := newStorageMapDB(t)

	total, err := db.StorageFootprintTotal("alpha")
	if err != nil {
		t.Fatalf("StorageFootprintTotal: %v", err)
	}
	// 1500 + 400 + 700 + 50.
	if total.Bytes != 2650 || total.Fee != 265000 {
		t.Errorf("got bytes=%d fee=%d, want 2650/265000", total.Bytes, total.Fee)
	}
	if total.Realms != 4 || total.Events != 7 {
		t.Errorf("got realms=%d events=%d, want 4/7", total.Realms, total.Events)
	}

	cells, err := db.StorageCells("alpha", 2)
	if err != nil {
		t.Fatalf("StorageCells: %v", err)
	}
	if len(cells) != 2 {
		t.Fatalf("limit ignored: got %d cells", len(cells))
	}
	var shown int
	for _, c := range cells {
		shown += c.Bytes
	}
	if shown >= total.Bytes {
		t.Errorf("a truncated list summed to the whole chain (%d of %d)", shown, total.Bytes)
	}
}

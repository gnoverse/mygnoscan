package store

import (
	"testing"

	"github.com/moul/mygnoscan/pkg/config"
)

// TestStorageBytesAndFeeAgree pins the invariant that makes storage_events
// self-checking, and that a sign flip cannot survive.
//
// Every row carries both a byte delta and the money that moved for it, and the
// chain charges a flat price per byte. So for any grouping at all, bytes times
// price must equal fee. When the syncer negated the unlock delta, the two
// columns of the same row disagreed: on mainnet the storage map showed
// gno.land/r/gnoland/wugnot at 10,918,147 bytes while the fee on the same row
// implied 1,180,507, and the fee was the one that matched the chain.
//
// Any future query, migration or ingestion change that breaks the sign breaks
// this test, without needing a chain to compare against.
func TestStorageBytesAndFeeAgree(t *testing.T) {
	const pricePerByte = 100

	db := NewTestDB(t)
	db.SetConfiguredNetworks([]config.NetworkConfig{{ID: "alpha"}})

	const when = "2026-01-01T00:00:00Z"
	events := []struct {
		path  string
		kind  string
		bytes int
	}{
		{"gno.land/r/ns/grow", "deposit", 5016},
		{"gno.land/r/ns/grow", "deposit", 18984},
		{"gno.land/r/ns/grow", "unlock", -17835},
		{"gno.land/r/ns/grow", "unlock", -830},
		// A realm that has only ever grown, which is the shape that hid the
		// bug: it looks correct under either sign convention.
		{"gno.land/r/ns/onlygrows", "deposit", 1278609},
		// And one that ends up net negative, which no realm can do on chain
		// but which the arithmetic must still carry without special-casing.
		{"gno.land/r/ns/drained", "deposit", 100},
		{"gno.land/r/ns/drained", "unlock", -400},
	}
	for i, e := range events {
		if err := db.InsertStorageEvent("alpha", "tx", i, e.path, 10+i, when,
			e.kind, e.bytes, e.bytes*pricePerByte); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	// Per realm.
	rows, err := db.SQL().Query(
		`SELECT pkg_path, SUM(bytes_delta), SUM(fee) FROM storage_events
		  WHERE network = 'alpha' GROUP BY pkg_path ORDER BY pkg_path`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	seen := 0
	for rows.Next() {
		var path string
		var bytes, fee int
		if err := rows.Scan(&path, &bytes, &fee); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if fee != bytes*pricePerByte {
			t.Errorf("%s: %d bytes implies %d ugnot, got %d",
				path, bytes, bytes*pricePerByte, fee)
		}
		seen++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if seen != 3 {
		t.Fatalf("checked %d realms, want 3", seen)
	}

	// And chain-wide, which is the number the storage map's header reports.
	var bytes, fee int
	if err := db.SQL().QueryRow(
		`SELECT SUM(bytes_delta), SUM(fee) FROM storage_events WHERE network = 'alpha'`,
	).Scan(&bytes, &fee); err != nil {
		t.Fatalf("total: %v", err)
	}
	if want := 5016 + 18984 - 17835 - 830 + 1278609 + 100 - 400; bytes != want {
		t.Errorf("total bytes = %d, want %d", bytes, want)
	}
	if fee != bytes*pricePerByte {
		t.Errorf("total fee = %d, want %d", fee, bytes*pricePerByte)
	}
}

// TestMigrateStorageUnlockSignRepairsAndIsIdempotent covers the backfill for
// databases written before the ingestion was fixed.
func TestMigrateStorageUnlockSignRepairsAndIsIdempotent(t *testing.T) {
	db := NewTestDB(t)
	db.SetConfiguredNetworks([]config.NetworkConfig{{ID: "alpha"}})

	const when = "2026-01-01T00:00:00Z"
	// A row as the broken syncer wrote it: positive bytes, negative fee.
	if err := db.InsertStorageEvent("alpha", "txbad", 0, "gno.land/r/ns/x", 10, when,
		"unlock", 2093, -209300); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// A deposit, which must not be touched however many times this runs.
	if err := db.InsertStorageEvent("alpha", "txok", 0, "gno.land/r/ns/x", 11, when,
		"deposit", 2079, 207900); err != nil {
		t.Fatalf("seed: %v", err)
	}

	read := func() (int, int) {
		t.Helper()
		var unlock, deposit int
		if err := db.SQL().QueryRow(
			`SELECT bytes_delta FROM storage_events WHERE tx_hash = 'txbad'`).Scan(&unlock); err != nil {
			t.Fatalf("read unlock: %v", err)
		}
		if err := db.SQL().QueryRow(
			`SELECT bytes_delta FROM storage_events WHERE tx_hash = 'txok'`).Scan(&deposit); err != nil {
			t.Fatalf("read deposit: %v", err)
		}
		return unlock, deposit
	}

	for pass := 1; pass <= 3; pass++ {
		if err := migrateStorageUnlockSign(db.SQL()); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		unlock, deposit := read()
		if unlock != -2093 {
			t.Fatalf("pass %d: unlock bytes = %d, want -2093", pass, unlock)
		}
		if deposit != 2079 {
			t.Fatalf("pass %d: deposit bytes = %d, want 2079", pass, deposit)
		}
	}
}

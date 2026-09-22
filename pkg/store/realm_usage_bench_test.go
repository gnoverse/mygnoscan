package store

import (
	"fmt"
	"testing"
	"time"
)

// RealmUsage on a realm the size of the busiest ones on mainnet.
//
// This exists because the first version of the callers query answered "the
// block_time of this caller's earliest row" with a correlated subquery over
// the CTE, once per caller group. It read fine and was quadratic: 151 seconds
// for one call at the size below, against 2.1 on the packed-key version that
// replaced it (see heightTimeKey). Nothing in the unit tests or the browser
// suite could see it, because both run against a fixture of single digits.
//
// Roughly where the time goes at this size, measured 2026-09-22 on a Xeon
// D-1531: summary 580ms, gas 730ms, callers 320ms, functions 270ms, feed
// 160ms. Five scans of a materialised 50k-row CTE, no single hot spot left.
// The API's response cache (30s TTL, stale-while-revalidate) means only the
// first reader after a sync pass pays it.
//
//	go test ./pkg/store/ -run XXX -bench RealmUsage -benchtime 1x
func BenchmarkRealmUsage(b *testing.B) {
	for _, size := range []struct {
		name           string
		calls, callers int
	}{
		{"typical", 500, 50},
		{"busiest", 50000, 5000},
	} {
		b.Run(size.name, func(b *testing.B) {
			db := NewTestDB(b)
			db.configured = []string{"mainnet"}
			const path = "gno.land/r/big/realm"
			seedBigRealm(b, db, path, size.calls, size.callers)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				u, err := db.RealmUsage("mainnet", path, RealmUsageFilter{})
				if err != nil {
					b.Fatal(err)
				}
				if u.Summary.Messages != size.calls {
					b.Fatalf("messages = %d, want %d", u.Summary.Messages, size.calls)
				}
			}
		})
	}
}

// seedBigRealm writes `calls` rows spread over `callers` addresses in one
// transaction. InsertCall takes the write mutex and commits per row, which at
// 50k rows is the benchmark rather than the thing being benchmarked.
func seedBigRealm(tb TB, db *DB, path string, calls, callers int) {
	tb.Helper()
	if err := db.UpsertPackage("mainnet", path, "realm", "g1dev", "TXD", 1, "", true, 1); err != nil {
		tb.Fatalf("upsert package: %v", err)
	}
	fns := []string{"Bid", "Claim", "Withdraw", "Render", "Vote", "Poke", "Set", "Clear"}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tx, err := db.db.Begin()
	if err != nil {
		tb.Fatalf("begin: %v", err)
	}
	cs, err := tx.Prepare(`INSERT INTO calls
		(network, tx_hash, msg_index, block_height, block_time, caller, pkg_path, func_name, success)
		VALUES ('mainnet', ?, 0, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		tb.Fatalf("prepare calls: %v", err)
	}
	ts, err := tx.Prepare(`INSERT INTO transactions
		(network, tx_hash, block_height, block_time, gas_used, gas_wanted, gas_fee, success)
		VALUES ('mainnet', ?, ?, ?, 90000, 150000, 800, 1)`)
	if err != nil {
		tb.Fatalf("prepare transactions: %v", err)
	}
	for i := 0; i < calls; i++ {
		h := 1000 + i
		when := base.Add(time.Duration(i) * time.Minute).Format(time.RFC3339Nano)
		hash := fmt.Sprintf("TX%d", i)
		// One in seventeen fails, so the ok/failed split is exercised rather
		// than being a column of zeroes.
		if _, err := cs.Exec(hash, h, when, fmt.Sprintf("g1caller%05d", i%callers),
			path, fns[i%len(fns)], i%17 != 0); err != nil {
			tb.Fatalf("insert call: %v", err)
		}
		if _, err := ts.Exec(hash, h, when); err != nil {
			tb.Fatalf("insert transaction: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		tb.Fatalf("commit: %v", err)
	}
}

package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/moul/mygnoscan/pkg/config"
)

// seedActiveAddrScale fills the rollup and its source tables at roughly the
// shape production reported in #137: ~110k distinct (network, hour, address)
// tuples over ~58 days of history.
//
// Seeded through the rollup table directly rather than by inserting a million
// source rows and rebuilding: what is being measured here is the *read* path,
// and paying a multi-minute rebuild per benchmark run would make it unusable.
func seedActiveAddrScale(t testing.TB, db *DB, hours, addrsPerHour int) {
	t.Helper()

	tx, err := db.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO active_addr_rollup (network, bucket, kind, addr) VALUES (?, ?, ?, ?)`)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	start := time.Now().UTC().Add(-time.Duration(hours) * time.Hour)
	for h := 0; h < hours; h++ {
		bucket := start.Add(time.Duration(h) * time.Hour).Format(activeAddrBucketLayout)
		for i := 0; i < addrsPerHour; i++ {
			// Addresses recur across hours, which is what makes the wider
			// buckets do real deduplication work rather than trivially
			// summing.
			addr := fmt.Sprintf("g1addr%06d", (h*7+i)%(addrsPerHour*4))
			if _, err := stmt.Exec("alpha", bucket, "callers", addr); err != nil {
				t.Fatalf("insert: %v", err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Record a build time, or every read falls through to the live path.
	//
	// Without it the boundary lookup reports "no rollup", the series is
	// computed from the (empty) source tables, and the benchmark measures 13
	// buckets of zero in a tenth of a millisecond — which is exactly what the
	// first version of this did. That is why the assertion below counts
	// addresses rather than buckets: an empty series has buckets too.
	if _, err := db.db.Exec(`INSERT OR REPLACE INTO sync_state (key, value) VALUES (?, ?)`,
		rollupComputedAtKey, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("set rollup boundary: %v", err)
	}
}

// The acceptance bar in #137 is 500ms at every window. This measures the read
// path at production-like scale so the claim can be checked without production.
func BenchmarkActiveAddressSeries(b *testing.B) {
	db := NewTestDB(b)
	db.SetConfiguredNetworks([]config.NetworkConfig{{ID: "alpha"}})
	// 58 days at ~80 distinct addresses an hour ≈ 111k tuples, the figure #137
	// measured on production.
	seedActiveAddrScale(b, db, 58*24, 80)

	for _, g := range []struct {
		name        string
		granularity string
		days        int
	}{
		{"24h", "hourly", 1},
		{"7d", "daily", 7},
		{"30d", "daily", 30},
		{"all", "monthly", 365},
	} {
		b.Run(g.name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				pts, err := db.GetActiveAddressTimeSeries("", g.granularity, g.days)
				if err != nil {
					b.Fatalf("GetActiveAddressTimeSeries: %v", err)
				}
				active := 0
				for _, p := range pts {
					active += p.TotalActive
				}
				if active == 0 {
					b.Fatal("the series counted no addresses — the benchmark is measuring an empty query")
				}
			}
		})
	}
}

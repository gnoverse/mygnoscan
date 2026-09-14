package main

import (
	"fmt"
	"testing"
	"time"
)

// Production shape, as measured on the sapphire instance and recorded in #137.
//
// The point of matching it is that the two read paths are only interestingly
// different at scale: on a handful of rows the union is instant and the rollup
// buys nothing. These numbers put the same 1.27M rows, the same 58 days and the
// same ~110k distinct (network, hour, address) tuples behind both.
const (
	benchDays      = 58
	benchCalls     = 700_000
	benchSends     = 550_000
	benchDeploys   = 24_000
	benchAddresses = 95_000

	// How many distinct addresses are active in one hour on one chain.
	//
	// This is the number that decides whether the fixture measures anything.
	// The rollup's whole value is that 1.27M rows carry only ~110k distinct
	// (network, hour, address) tuples on production, a ratio of about 11.6 to 1;
	// a fixture that gives every row its own address stores 1.27M tuples, and
	// then the rollup is just the same scan under another name. Twenty per
	// (network, hour) reproduces production's ratio.
	benchPoolPerHour = 20
)

// seedProductionScale bulk-loads a chain's worth of activity.
//
// Raw SQL in one transaction rather than the Insert* methods: those take the
// write lock and commit per row, which turns a minute of setup into an hour and
// measures nothing anyone cares about.
func seedProductionScale(tb testing.TB, db *DB) {
	tb.Helper()

	networks := []string{"alpha", "beta"}
	db.SetConfiguredNetworks([]NetworkConfig{{ID: networks[0]}, {ID: networks[1]}})

	tx, err := db.db.Begin()
	if err != nil {
		tb.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()

	hours := benchDays * 24
	start := time.Now().UTC().Add(-time.Duration(hours) * time.Hour).Truncate(time.Hour)

	// Rows are dealt out hour by hour: row i lands in hour i%hours on pass
	// i/hours. The network alternates by pass so both chains get the whole
	// history, and the address comes from that hour's pool, which is what makes
	// the fixture compress the way production does.
	at := func(i int) (string, string, int) {
		hour, pass := i%hours, i/hours
		ts := start.Add(time.Duration(hour)*time.Hour + time.Duration(pass%60)*time.Minute)
		return networks[pass%len(networks)], ts.Format(time.RFC3339), hour
	}
	addr := func(i, hour int) string {
		pass := i / hours
		return fmt.Sprintf("g1addr%06d",
			(hour*benchPoolPerHour+(pass/len(networks))%benchPoolPerHour)%benchAddresses)
	}

	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO calls
		(network, tx_hash, block_height, block_time, caller, pkg_path, func_name, success)
		VALUES (?, ?, ?, ?, ?, ?, ?, 1)`)
	if err != nil {
		tb.Fatalf("prepare calls: %v", err)
	}
	for i := 0; i < benchCalls; i++ {
		net, ts, hour := at(i)
		if _, err := stmt.Exec(net, fmt.Sprintf("call%07d", i), i, ts,
			addr(i, hour), "gno.land/r/demo/boards", "Post"); err != nil {
			tb.Fatalf("insert call: %v", err)
		}
	}
	stmt.Close()

	stmt, err = tx.Prepare(`INSERT OR IGNORE INTO bank_sends
		(network, tx_hash, block_height, block_time, from_address, to_address, amount, success)
		VALUES (?, ?, ?, ?, ?, ?, '1000ugnot', 1)`)
	if err != nil {
		tb.Fatalf("prepare sends: %v", err)
	}
	for i := 0; i < benchSends; i++ {
		net, ts, hour := at(i)
		if _, err := stmt.Exec(net, fmt.Sprintf("send%07d", i), i, ts,
			addr(i, hour), "g1recipient"); err != nil {
			tb.Fatalf("insert send: %v", err)
		}
	}
	stmt.Close()

	stmt, err = tx.Prepare(`INSERT OR IGNORE INTO packages
		(network, path, name, creator, block_height, block_time, tx_hash, is_realm, num_files)
		VALUES (?, ?, 'pkg', ?, ?, ?, ?, 1, 1)`)
	if err != nil {
		tb.Fatalf("prepare packages: %v", err)
	}
	for i := 0; i < benchDeploys; i++ {
		net, ts, hour := at(i)
		if _, err := stmt.Exec(net, fmt.Sprintf("gno.land/r/bench/p%06d", i),
			addr(i, hour), i, ts, fmt.Sprintf("pkg%07d", i)); err != nil {
			tb.Fatalf("insert package: %v", err)
		}
	}
	stmt.Close()

	if err := tx.Commit(); err != nil {
		tb.Fatalf("commit: %v", err)
	}
}

// BenchmarkActiveAddressSeries measures both read paths against one database.
//
//	go test -run '^$' -bench ActiveAddressSeries -benchtime 3x
//
// The live sub-benchmarks run before the refresh, so the dispatch in
// GetActiveAddressTimeSeries picks the fallback; the rolled-up ones run after
// it. Same fixture, same public entry point, so the difference is the path.
func BenchmarkActiveAddressSeries(b *testing.B) {
	db := newTestDB(b)

	seedStart := time.Now()
	seedProductionScale(b, db)
	b.Logf("seeded %d rows in %s", benchCalls+benchSends+benchDeploys,
		time.Since(seedStart).Round(time.Millisecond))

	windows := []struct {
		name        string
		days        int
		granularity string
	}{
		{"24h", 1, "hourly"},
		{"7d", 7, "daily"},
		{"90d", 90, "daily"},
		{"1y", 365, "monthly"},
	}

	run := func(b *testing.B, days int, granularity string) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := db.GetActiveAddressTimeSeries("", granularity, days); err != nil {
				b.Fatalf("GetActiveAddressTimeSeries: %v", err)
			}
		}
	}

	for _, w := range windows {
		b.Run("live/"+w.name, func(b *testing.B) { run(b, w.days, w.granularity) })
	}

	refreshStart := time.Now()
	if err := db.RefreshRollups(); err != nil {
		b.Fatalf("RefreshRollups: %v", err)
	}
	b.Logf("rollup rebuilt in %s", time.Since(refreshStart).Round(time.Millisecond))

	var tuples int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM active_addr_rollup`).Scan(&tuples); err != nil {
		b.Fatalf("count tuples: %v", err)
	}
	b.Logf("rollup holds %d tuples", tuples)

	for _, w := range windows {
		b.Run("rolledup/"+w.name, func(b *testing.B) { run(b, w.days, w.granularity) })
	}
}

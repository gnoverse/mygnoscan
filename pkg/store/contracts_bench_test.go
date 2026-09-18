package store

import (
	"fmt"
	"math/rand"
	"testing"
	"time"
)

// Mainnet's shape on 2026-09-18, from mygnoscan.moul.p2p.team/api/stats:
// 619 realms, 322 pure packages, 192,940 calls, 2,305 unique callers.
const (
	benchPackages = 941
	benchCallers  = 2305
	benchCalls    = 192940
)

// benchFanout is how many distinct contracts one address touches.
//
// Measured, not guessed: summing unique_users across every package mainnet
// serves gives 1,019 distinct (caller, package) pairs against 2,305 callers,
// so the overwhelming majority of addresses touch exactly one contract and the
// shared-caller graph is sparse, concentrated around wugnot and the gnoswap
// realms. Seeding four contracts per caller is already several times denser
// than the chain it is modelling.
const benchFanout = 4

// seedContractScale fills packages and calls at mainnet scale.
//
// Callers are given a small home set of contracts and then call into it
// repeatedly, which is what the production numbers describe and what decides
// this query's cost: the work is proportional to distinct (caller, contract)
// pairs, not to the call count. Traffic within those sets is skewed by
// squaring a uniform draw, concentrating calls on low indices the way a real
// chain concentrates them on a handful of contracts.
//
// Written through one transaction with prepared statements rather than the
// store's own insert methods: this measures the read path, and paying per-row
// locking for 190k inserts would make the benchmark unusable.
func seedContractScale(tb testing.TB, db *DB, fanout int) {
	tb.Helper()

	tx, err := db.db.Begin()
	if err != nil {
		tb.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()

	pkgStmt, err := tx.Prepare(`INSERT OR REPLACE INTO packages
		(network, path, name, creator, block_height, block_time, tx_hash, is_realm, num_files)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		tb.Fatalf("prepare packages: %v", err)
	}
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	paths := make([]string, benchPackages)
	for i := 0; i < benchPackages; i++ {
		// 60 namespaces, the rough order of magnitude on mainnet.
		paths[i] = fmt.Sprintf("gno.land/r/ns%d/pkg%d", i%60, i)
		if _, err := pkgStmt.Exec("mainnet", paths[i], fmt.Sprintf("pkg%d", i), fmt.Sprintf("g1creator%d", i%200),
			i, rfc3339(base.Add(time.Duration(i)*time.Minute)), fmt.Sprintf("TXP%d", i), i%3 != 0, 3); err != nil {
			tb.Fatalf("insert package: %v", err)
		}
	}

	rng := rand.New(rand.NewSource(1))
	skewed := func(n int) int { return int(float64(n) * rng.Float64() * rng.Float64()) }

	// Each caller's home set, drawn skewed so popular contracts appear in many
	// of them. This is what produces the shared-caller pairs.
	home := make([][]int, benchCallers)
	for c := range home {
		k := 1 + rng.Intn(fanout)
		for i := 0; i < k; i++ {
			home[c] = append(home[c], skewed(benchPackages))
		}
	}

	callStmt, err := tx.Prepare(`INSERT OR IGNORE INTO calls
		(network, tx_hash, msg_index, block_height, block_time, caller, pkg_path, func_name, success)
		VALUES (?, ?, 0, ?, ?, ?, ?, 'Fn', 1)`)
	if err != nil {
		tb.Fatalf("prepare calls: %v", err)
	}
	for i := 0; i < benchCalls; i++ {
		caller := skewed(benchCallers)
		set := home[caller]
		pkg := set[rng.Intn(len(set))]
		if _, err := callStmt.Exec("mainnet", fmt.Sprintf("TXC%d", i), i,
			rfc3339(base.Add(time.Duration(i)*time.Second)),
			fmt.Sprintf("g1caller%d", caller), paths[pkg]); err != nil {
			tb.Fatalf("insert call: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		tb.Fatalf("commit: %v", err)
	}
}

// BenchmarkContractCallerEdges is the gate the design set before any number was
// known: over 300ms at mainnet scale and this query moves out of the request
// path into RefreshRollups.
//
// Run it with: go test ./pkg/store/ -bench ContractCallerEdges -benchtime 3x
func BenchmarkContractCallerEdges(b *testing.B) {
	db := NewTestDB(b)
	seedContractScale(b, db, benchFanout)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		edges, err := db.ContractCallerEdges("mainnet", time.Time{}, 2, 1500, 30)
		if err != nil {
			b.Fatalf("ContractCallerEdges: %v", err)
		}
		if len(edges) == 0 {
			b.Fatal("no edges: the benchmark is measuring an empty query")
		}
	}
}

// The pathological shape, kept as a ceiling rather than as a gate: every call
// from a different address to a different contract, which is roughly 50 times
// the (caller, contract) density mainnet actually has. Nothing on chain looks
// like this, and the query should still return rather than hang if something
// one day does.
func BenchmarkContractCallerEdgesWorstCase(b *testing.B) {
	db := NewTestDB(b)
	seedContractScale(b, db, benchPackages)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := db.ContractCallerEdges("mainnet", time.Time{}, 2, 1500, 30); err != nil {
			b.Fatalf("ContractCallerEdges: %v", err)
		}
	}
}

func BenchmarkContractMapNodes(b *testing.B) {
	db := NewTestDB(b)
	seedContractScale(b, db, benchFanout)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		nodes, err := db.ContractMapNodes("mainnet", time.Time{})
		if err != nil {
			b.Fatalf("ContractMapNodes: %v", err)
		}
		if len(nodes) != benchPackages {
			b.Fatalf("got %d nodes, want %d", len(nodes), benchPackages)
		}
	}
}

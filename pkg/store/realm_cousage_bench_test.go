package store

import (
	"fmt"
	"testing"
	"time"
)

// Co-usage at chain scale.
//
// This query is unusual among the realm page's for being bounded by the whole
// network rather than by the realm: it groups every call on the chain, not
// every call to this contract, because "what else did my callers call" is a
// question about everybody else's traffic. So the size that matters is the
// chain's, and a fixture of single digits says nothing about it.
//
// Two scans of `calls` per request, both linear and both index-covered: the
// partner grouping walks idx_calls_net_caller_pkg through an ephemeral index
// built from the subject's caller set, and callerTotals walks the same index
// once more. The second is deliberate. Written as a correlated subquery in the
// SELECT list it would be evaluated per group, and the groups are every
// contract the callers touched, before the LIMIT cuts it to the hundred anybody
// reads. That is the shape RealmUsage's callers query had to have removed from
// it, and it is quadratic for the same reason.
//
// Measured 2026-09-22 on a Xeon D-1531, at pearl's size today (200k calls, 350
// contracts). The numbers are in the git history of this file rather than
// restated here, except the one that matters: the cost tracks how many of the
// chain's addresses called the subject, not how big the chain is.
//
// Do not "fix" the plan by forcing idx_calls_net_pkg_caller with INDEXED BY:
// that reads like the better index, since the query groups by pkg_path, and it
// was measured at 6.8s against 570ms for the plan SQLite picks on its own.
//
//	go test ./pkg/store/ -run XXX -bench CoUsage -benchtime 1x
func BenchmarkRealmCoUsagePartners(b *testing.B) {
	for _, size := range []struct {
		name string
		// subjectCallers is how many of the chain's addresses ever called the
		// subject, and is the parameter that actually drives the cost: the
		// partner query walks every row belonging to those addresses. "busiest"
		// is the shape of r/gnoland/wugnot, 416 callers of mainnet's 2305;
		// "worst" is the case that cannot be exceeded, every address on the
		// chain having called this one realm.
		calls, callers, contracts, subjectCallers int
	}{
		{"typical", 5000, 500, 50, 100},
		{"busiest", 200000, 2300, 350, 420},
		{"worst", 200000, 5000, 350, 5000},
	} {
		b.Run(size.name, func(b *testing.B) {
			db := NewTestDB(b)
			db.configured = []string{"mainnet"}
			subject := seedBigChain(b, db, size.calls, size.callers, size.contracts, size.subjectCallers)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				got, err := db.RealmCoUsagePartners("mainnet", subject, time.Time{}, 0)
				if err != nil {
					b.Fatal(err)
				}
				// A benchmark that silently measured the empty path would report
				// a beautiful number for a query that answered nothing.
				if len(got.Partners) == 0 {
					b.Fatalf("no partners at %d calls: the query found nothing to time", size.calls)
				}
			}
		})
	}
}

// seedBigChain writes `calls` rows spread over `callers` addresses and
// `contracts` packages, and returns the path of the busiest one.
//
// One transaction, because InsertCall takes the write mutex and commits per
// row, which at 200k rows is the benchmark rather than the thing benchmarked.
//
// The spread matters as much as the count, and it is easy to get silently
// wrong. The first version of this helper picked the subject on even rows and
// every other contract on odd ones, while stepping the caller by `i*7` over an
// even caller count: that preserves the parity of i, so the subject drew only
// even-numbered addresses and everything else only odd-numbered ones, and the
// two sets never met. 200k rows of perfectly plausible traffic with zero
// co-usage in it. Selecting the subject every third row instead breaks the
// parity, and the guard in the benchmark above is what caught it.
func seedBigChain(tb TB, db *DB, calls, callers, contracts, subjectCallers int) string {
	tb.Helper()

	paths := make([]string, contracts)
	for i := range paths {
		paths[i] = fmt.Sprintf("gno.land/r/ns%02d/app%03d", i%20, i)
		if err := db.UpsertPackage("mainnet", paths[i], "app", "g1dev",
			fmt.Sprintf("TXD%d", i), 1+i, "", true, 1); err != nil {
			tb.Fatalf("upsert package: %v", err)
		}
	}

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tx, err := db.db.Begin()
	if err != nil {
		tb.Fatalf("begin: %v", err)
	}
	cs, err := tx.Prepare(`INSERT INTO calls
		(network, tx_hash, msg_index, block_height, block_time, caller, pkg_path, func_name, success)
		VALUES ('mainnet', ?, 0, ?, ?, ?, ?, 'Fn', 1)`)
	if err != nil {
		tb.Fatalf("prepare: %v", err)
	}
	for i := 0; i < calls; i++ {
		h := 1000 + i
		when := base.Add(time.Duration(i) * time.Second).Format(time.RFC3339Nano)
		// A third of the traffic lands on paths[0], the subject: a realm nobody
		// uses has nothing to compare against. Its callers come from the first
		// `subjectCallers` addresses only, so the chain can be much wider than
		// the realm, which is what a real one looks like.
		path, pool := paths[0], subjectCallers
		if i%3 != 0 {
			path, pool = paths[1+(i*3)%(contracts-1)], callers
		}
		caller := fmt.Sprintf("g1caller%06d", (i*7)%pool)
		if _, err := cs.Exec(fmt.Sprintf("TXC%d", i), h, when, caller, path); err != nil {
			tb.Fatalf("insert call %d: %v", i, err)
		}
	}
	if err := cs.Close(); err != nil {
		tb.Fatalf("close stmt: %v", err)
	}
	if err := tx.Commit(); err != nil {
		tb.Fatalf("commit: %v", err)
	}
	if _, err := db.db.Exec("ANALYZE"); err != nil {
		tb.Fatalf("analyze: %v", err)
	}
	return paths[0]
}

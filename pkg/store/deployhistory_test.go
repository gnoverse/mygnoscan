package store

import (
	"testing"

	"github.com/moul/mygnoscan/pkg/config"
)

// Gas attribution asks what a transaction deployed, which is a question about
// history, and packages cannot answer it.
//
// packages is keyed (network, path) and written INSERT OR REPLACE, so a
// redeploy overwrites the earlier row and takes that earlier transaction's
// tx_hash with it. Joining packages on tx_hash therefore drops every deploy
// that was later resubmitted at the same path, along with the gas it burned,
// and resubmission is routine: under the "inert" code submission policy a
// parked package is invisible to every liveness probe, so anything that
// verifies a deploy by querying the path concludes it failed and tries again.
//
// Measured on mainnet 2026-09-22: those joins saw 368 of 447 deploy
// transactions, losing 1,281,064,776 gas_used (9.5%) and 74,740,000 gas_fee
// (24.9%) that had genuinely been spent deploying.
func TestGasAttributionCountsEveryDeploySubmission(t *testing.T) {
	db := NewTestDB(t)
	db.SetConfiguredNetworks([]config.NetworkConfig{{ID: "live"}})

	const (
		when = "2026-08-01T00:00:00Z"
		path = "gno.land/r/demo/retried"
	)
	// One path, submitted three times: the shape a stuck approver produces.
	deploys := []struct {
		hash string
		gas  int
	}{{"try1", 100}, {"try2", 200}, {"try3", 400}}
	for i, d := range deploys {
		if err := db.UpsertTransaction("live", d.hash, 100+i, when, d.gas, d.gas*2, d.gas/10, true); err != nil {
			t.Fatalf("UpsertTransaction(%s): %v", d.hash, err)
		}
		if err := db.InsertPackageSubmission("live", d.hash, 0, path, "retried",
			"g1deployer", 100+i, when, true, 1, true); err != nil {
			t.Fatalf("InsertPackageSubmission(%s): %v", d.hash, err)
		}
		// packages keeps whichever landed last, which is the point.
		if err := db.UpsertPackage("live", path, "retried", "g1deployer", d.hash, 100+i, when, true, 1); err != nil {
			t.Fatalf("UpsertPackage(%s): %v", d.hash, err)
		}
	}

	// 100 + 200 + 400. Reading packages would find only try3 and report 400.
	const wantGas = 700

	check := func(t *testing.T, label string) {
		t.Helper()
		stats, err := db.GetGasStats("live", 10)
		if err != nil {
			t.Fatalf("%s: GetGasStats: %v", label, err)
		}

		var realm int
		for _, r := range stats.TopRealms {
			if r.Path == path {
				realm = r.Gas
			}
		}
		if realm != wantGas {
			t.Errorf("%s: gas attributed to %s = %d, want %d: the retried submissions are the difference",
				label, path, realm, wantGas)
		}

		var caller int
		for _, c := range stats.TopCallers {
			if c.Address == "g1deployer" {
				caller = c.Gas
			}
		}
		if caller != wantGas {
			t.Errorf("%s: gas attributed to the deployer = %d, want %d", label, caller, wantGas)
		}
	}

	// Both paths, because the rollup builds the same attribution with its own
	// copy of the query and one used to be fixed while the other was not.
	t.Run("computed live", func(t *testing.T) { check(t, "live") })

	if err := db.RefreshRollups(); err != nil {
		t.Fatalf("RefreshRollups: %v", err)
	}
	t.Run("from the rollup", func(t *testing.T) { check(t, "rollup") })
}

// A deploy transaction is labelled MsgAddPackage even after its path has been
// redeployed, for the same reason: the label is looked up by tx_hash.
func TestTopGasTransactionsLabelEveryDeploy(t *testing.T) {
	db := NewTestDB(t)
	db.SetConfiguredNetworks([]config.NetworkConfig{{ID: "live"}})

	const (
		when = "2026-08-01T00:00:00Z"
		path = "gno.land/r/demo/retried"
	)
	for i, hash := range []string{"first", "second"} {
		if err := db.UpsertTransaction("live", hash, 100+i, when, 5000, 10000, 500, true); err != nil {
			t.Fatalf("UpsertTransaction: %v", err)
		}
		if err := db.InsertPackageSubmission("live", hash, 0, path, "retried",
			"g1deployer", 100+i, when, true, 1, true); err != nil {
			t.Fatalf("InsertPackageSubmission: %v", err)
		}
		if err := db.UpsertPackage("live", path, "retried", "g1deployer", hash, 100+i, when, true, 1); err != nil {
			t.Fatalf("UpsertPackage: %v", err)
		}
	}

	stats, err := db.GetGasStats("live", 10)
	if err != nil {
		t.Fatalf("GetGasStats: %v", err)
	}

	seen := map[string]string{}
	for _, tx := range stats.TopTxs {
		seen[tx.Hash] = tx.Type
	}
	for _, hash := range []string{"first", "second"} {
		if seen[hash] != "MsgAddPackage" {
			t.Errorf("transaction %q typed %q, want MsgAddPackage", hash, seen[hash])
		}
	}
}

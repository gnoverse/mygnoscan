package store

import (
	"fmt"
	"testing"
	"time"
)

// seedUsage writes a realm with a known caller mix on two networks.
//
// mainnet, gno.land/r/demo/rumble:
//
//	g1alice  Bid x3 (one failed), Claim x1, two of the Bids share one tx
//	g1bob    Bid x1
//	g1carol  one MsgRun importing the realm
//
// pearl holds a same-path realm with different traffic, so a network leak
// shows up as a wrong count rather than as nothing at all.
func seedUsage(t TB, db *DB) {
	t.Helper()
	const path = "gno.land/r/demo/rumble"
	day := func(n int) string {
		return time.Date(2026, 1, n, 12, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	}
	if err := db.UpsertPackage("mainnet", path, "rumble", "g1alice", "TXDEPLOY", 100, day(1), true, 1); err != nil {
		t.Fatalf("upsert package: %v", err)
	}
	calls := []struct {
		tx     string
		idx    int
		height int
		caller string
		fn     string
		ok     bool
	}{
		{"TX1", 0, 110, "g1alice", "Bid", true},
		{"TX1", 1, 110, "g1alice", "Bid", true}, // multicall: same tx, two messages
		{"TX2", 0, 120, "g1bob", "Bid", true},
		{"TX3", 0, 130, "g1alice", "Bid", false},
		{"TX4", 0, 140, "g1alice", "Claim", true},
	}
	for i, c := range calls {
		if err := db.InsertCall("mainnet", c.tx, c.height, c.idx, day(2+i), c.caller, path, c.fn, c.ok); err != nil {
			t.Fatalf("insert call: %v", err)
		}
	}
	for _, tx := range []struct {
		hash   string
		height int
		gas    int
	}{{"TX1", 110, 1000}, {"TX2", 120, 2000}, {"TX3", 130, 3000}, {"TX4", 140, 4000}, {"TXRUN", 150, 5000}} {
		if err := db.UpsertTransaction("mainnet", tx.hash, tx.height, "", tx.gas, tx.gas*2, 1, true); err != nil {
			t.Fatalf("upsert tx: %v", err)
		}
	}
	if err := db.InsertMsgRun("mainnet", "TXRUN", 150, day(8), "g1carol", "import \""+path+"\"", true); err != nil {
		t.Fatalf("insert msgrun: %v", err)
	}
	// Same path, other network, other caller.
	if err := db.UpsertPackage("pearl", path, "rumble", "g1alice", "TXDEPLOY", 1, day(1), true, 1); err != nil {
		t.Fatalf("upsert package (pearl): %v", err)
	}
	if err := db.InsertCall("pearl", "PTX1", 10, 0, day(1), "g1mallory", path, "Bid", true); err != nil {
		t.Fatalf("insert call (pearl): %v", err)
	}
}

func TestRealmUsage(t *testing.T) {
	db := NewTestDB(t)
	db.configured = []string{"mainnet", "pearl"}
	seedUsage(t, db)
	const path = "gno.land/r/demo/rumble"

	tests := []struct {
		name          string
		filter        RealmUsageFilter
		wantMessages  int
		wantCalls     int
		wantRuns      int
		wantTxs       int
		wantUnique    int
		wantReturning int
		wantOK        int
		wantFailed    int
		wantFuncs     int
		wantRows      int
	}{
		{
			name: "everything", wantMessages: 6, wantCalls: 5, wantRuns: 1, wantTxs: 5,
			wantUnique: 3, wantReturning: 1, wantOK: 5, wantFailed: 1, wantFuncs: 2, wantRows: 6,
		},
		{
			name: "one function", filter: RealmUsageFilter{Func: "Bid"},
			wantMessages: 4, wantCalls: 4, wantTxs: 3,
			wantUnique: 2, wantReturning: 1, wantOK: 3, wantFailed: 1, wantFuncs: 1, wantRows: 4,
		},
		{
			name: "one caller", filter: RealmUsageFilter{Caller: "g1alice"},
			wantMessages: 4, wantCalls: 4, wantTxs: 3,
			wantUnique: 1, wantReturning: 1, wantOK: 3, wantFailed: 1, wantFuncs: 2, wantRows: 4,
		},
		{
			name: "failures only", filter: RealmUsageFilter{Status: "fail"},
			wantMessages: 1, wantCalls: 1, wantTxs: 1,
			wantUnique: 1, wantOK: 0, wantFailed: 1, wantFuncs: 1, wantRows: 1,
		},
		{
			name: "runs only", filter: RealmUsageFilter{Kind: "run"},
			wantMessages: 1, wantRuns: 1, wantTxs: 1,
			wantUnique: 1, wantOK: 1, wantRows: 1,
		},
		{
			name: "a function filter excludes every run", filter: RealmUsageFilter{Kind: "run", Func: "Bid"},
		},
		{
			name: "paging leaves the aggregates alone", filter: RealmUsageFilter{Limit: 2},
			wantMessages: 6, wantCalls: 5, wantRuns: 1, wantTxs: 5,
			wantUnique: 3, wantReturning: 1, wantOK: 5, wantFailed: 1, wantFuncs: 2, wantRows: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := db.RealmUsage("mainnet", path, tt.filter)
			if err != nil {
				t.Fatalf("RealmUsage: %v", err)
			}
			for _, c := range []struct {
				what      string
				got, want int
			}{
				{"messages", got.Summary.Messages, tt.wantMessages},
				{"calls", got.Summary.Calls, tt.wantCalls},
				{"runs", got.Summary.Runs, tt.wantRuns},
				{"txs", got.Summary.Txs, tt.wantTxs},
				{"unique callers", got.Summary.UniqueCallers, tt.wantUnique},
				{"returning", got.Summary.Returning, tt.wantReturning},
				{"ok", got.Summary.OK, tt.wantOK},
				{"failed", got.Summary.Failed, tt.wantFailed},
				{"functions", got.Summary.Functions, tt.wantFuncs},
				{"rows", len(got.Rows), tt.wantRows},
			} {
				if c.got != c.want {
					t.Errorf("%s = %d, want %d", c.what, c.got, c.want)
				}
			}
		})
	}
}

// The callers table is the feature: address, how many messages, how many
// transactions behind them, and gas attributed once per transaction rather
// than once per message.
func TestRealmUsageCallers(t *testing.T) {
	db := NewTestDB(t)
	db.configured = []string{"mainnet", "pearl"}
	seedUsage(t, db)

	got, err := db.RealmUsage("mainnet", "gno.land/r/demo/rumble", RealmUsageFilter{})
	if err != nil {
		t.Fatalf("RealmUsage: %v", err)
	}
	want := []RealmCaller{
		// TX1 (two messages) + TX3 + TX4: 1000 + 3000 + 4000, not 1000 twice.
		{Address: "g1alice", Messages: 4, Calls: 4, Txs: 3, OK: 3, Failed: 1, Funcs: 2,
			FirstHeight: 110, LastHeight: 140, GasUsed: 8000},
		// A tie on message count breaks on the most recent height, so the
		// MsgRun at 150 sorts ahead of g1bob's single call at 120.
		{Address: "g1carol", Messages: 1, Runs: 1, Txs: 1, OK: 1,
			FirstHeight: 150, LastHeight: 150, GasUsed: 5000},
		{Address: "g1bob", Messages: 1, Calls: 1, Txs: 1, OK: 1, Funcs: 1,
			FirstHeight: 120, LastHeight: 120, GasUsed: 2000},
	}
	if len(got.Callers) != len(want) {
		t.Fatalf("got %d callers, want %d", len(got.Callers), len(want))
	}
	for i, w := range want {
		g := got.Callers[i]
		if g.Address != w.Address || g.Messages != w.Messages || g.Calls != w.Calls ||
			g.Runs != w.Runs || g.Txs != w.Txs || g.OK != w.OK || g.Failed != w.Failed ||
			g.Funcs != w.Funcs || g.FirstHeight != w.FirstHeight || g.LastHeight != w.LastHeight ||
			g.GasUsed != w.GasUsed {
			t.Errorf("caller %d = %+v, want %+v", i, g, w)
		}
	}
	if got.Summary.GasUsed != 15000 {
		t.Errorf("summary gas = %d, want 15000", got.Summary.GasUsed)
	}

	wantFns := []RealmFunction{
		{Name: "Bid", Calls: 4, Callers: 2, OK: 3, Failed: 1, LastHeight: 130},
		{Name: "Claim", Calls: 1, Callers: 1, OK: 1, LastHeight: 140},
	}
	if len(got.Functions) != len(wantFns) {
		t.Fatalf("got %d functions, want %d", len(got.Functions), len(wantFns))
	}
	for i, w := range wantFns {
		g := got.Functions[i]
		if g.Name != w.Name || g.Calls != w.Calls || g.Callers != w.Callers ||
			g.OK != w.OK || g.Failed != w.Failed || g.LastHeight != w.LastHeight {
			t.Errorf("function %d = %+v, want %+v", i, g, w)
		}
	}
}

// The same realm path exists on two networks. Reading one must never count the
// other's callers (AGENTS.md: everything is network-scoped).
func TestRealmUsageIsNetworkScoped(t *testing.T) {
	db := NewTestDB(t)
	db.configured = []string{"mainnet", "pearl"}
	seedUsage(t, db)

	pearl, err := db.RealmUsage("pearl", "gno.land/r/demo/rumble", RealmUsageFilter{})
	if err != nil {
		t.Fatalf("RealmUsage(pearl): %v", err)
	}
	if pearl.Summary.Messages != 1 || pearl.Summary.UniqueCallers != 1 {
		t.Fatalf("pearl = %d messages / %d callers, want 1/1", pearl.Summary.Messages, pearl.Summary.UniqueCallers)
	}
	if pearl.Callers[0].Address != "g1mallory" {
		t.Errorf("pearl caller = %q, want g1mallory", pearl.Callers[0].Address)
	}
}

// A window excludes rows with no block_time at all, which is what a window
// that always ends now has to do with an undated row.
func TestRealmUsageWindow(t *testing.T) {
	db := NewTestDB(t)
	db.configured = []string{"mainnet"}
	const path = "gno.land/r/demo/windowed"
	if err := db.UpsertPackage("mainnet", path, "windowed", "g1alice", "TXD", 1, "", true, 1); err != nil {
		t.Fatalf("upsert package: %v", err)
	}
	recent := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	old := time.Now().UTC().AddDate(0, 0, -60).Format(time.RFC3339Nano)
	for i, bt := range []string{recent, old, ""} {
		if err := db.InsertCall("mainnet", fmt.Sprintf("TX%d", i), 10+i, 0, bt, "g1alice", path, "Ping", true); err != nil {
			t.Fatalf("insert call: %v", err)
		}
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -7).Format(time.RFC3339Nano)
	got, err := db.RealmUsage("mainnet", path, RealmUsageFilter{Since: cutoff})
	if err != nil {
		t.Fatalf("RealmUsage: %v", err)
	}
	if got.Summary.Messages != 1 {
		t.Errorf("7d window = %d messages, want 1", got.Summary.Messages)
	}
}

func TestRealmUsageUnknownPath(t *testing.T) {
	db := NewTestDB(t)
	db.configured = []string{"mainnet"}
	if _, err := db.RealmUsage("mainnet", "gno.land/r/demo/nope", RealmUsageFilter{}); err == nil {
		t.Fatal("RealmUsage on an unknown path returned no error")
	}
}

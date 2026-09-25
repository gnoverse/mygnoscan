package store

import "testing"

// These are the invariants the old in-Go derivation carried, ported to the SQL
// that replaced it. They are the acceptance criteria for that swap: the
// arithmetic is supposed to be identical, so a "better" answer here would look
// exactly like a regression on a live realm.

const (
	rcRealm   = "g1realm000000000000000000000000000000"
	rcDeposit = "g1deposit00000000000000000000000000000"
	rcOutside = "g1outsider0000000000000000000000000000"
	rcWinner  = "g1winner00000000000000000000000000000"
	rcFunder  = "g1funder00000000000000000000000000000"
)

func rcSeed(t *testing.T, db *DB, legs ...CoinTransfer) {
	t.Helper()
	for i, l := range legs {
		if l.BlockHeight == 0 {
			l.BlockHeight = 10 + i
		}
		if l.BlockTime == "" {
			l.BlockTime = "2026-09-0" + string(rune('1'+i%9)) + "T00:00:00Z"
		}
		if l.Ugnot == 0 {
			l.Ugnot = ParseUgnot(l.Coins)
		}
		if err := db.InsertCoinTransfer("alpha", "tx"+string(rune('a'+i)), 0, l); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
}

// The sign is what a reconstruction gets wrong silently: a reader that treats
// every leg as a receipt still lands on a plausible number, just the gross one.
func TestRealmCoinFlowsSignsAndAttributesEachLeg(t *testing.T) {
	db := NewTestDB(t)
	rcSeed(t, db,
		CoinTransfer{From: rcFunder, To: rcRealm, Coins: "5000000ugnot"},
		CoinTransfer{From: rcRealm, To: rcOutside, Coins: "1000000ugnot"},
		CoinTransfer{From: rcFunder, To: rcDeposit, Coins: "900ugnot"},
		// A leg that touches neither account: one transaction can carry many
		// transfers and only some of them are this package's.
		CoinTransfer{From: rcFunder, To: rcOutside, Coins: "77ugnot"},
	)

	flows, err := db.RealmCoinFlowsFor("alpha", rcRealm, rcDeposit, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(flows) != 3 {
		t.Fatalf("got %d legs, want the 3 touching one of the two accounts: %+v", len(flows), flows)
	}

	byHash := map[string]RealmCoinFlow{}
	for _, f := range flows {
		byHash[f.TxHash] = f
	}
	tests := []struct {
		hash, account, counterparty string
		amount                      int64
	}{
		{"txa", "banker", rcFunder, 5000000},
		{"txb", "banker", rcOutside, -1000000},
		{"txc", "storage deposit", rcFunder, 900},
	}
	for _, tt := range tests {
		t.Run(tt.hash, func(t *testing.T) {
			f := byHash[tt.hash]
			if f.Account != tt.account {
				t.Errorf("account = %q, want %q", f.Account, tt.account)
			}
			if f.Counterparty != tt.counterparty {
				t.Errorf("counterparty = %q, want %q", f.Counterparty, tt.counterparty)
			}
			if f.Amount != tt.amount {
				t.Errorf("amount = %d, want %d (positive is the package receiving)", f.Amount, tt.amount)
			}
		})
	}
}

// Summing both accounts into one figure would compare the sum of two against
// the balance of one, and report the difference as an indexer gap.
func TestRealmCoinStatsKeepTheDepositOutOfTheNet(t *testing.T) {
	db := NewTestDB(t)
	rcSeed(t, db,
		CoinTransfer{From: rcFunder, To: rcRealm, Coins: "5000000ugnot"},
		CoinTransfer{From: rcRealm, To: rcOutside, Coins: "1000000ugnot"},
		CoinTransfer{From: rcFunder, To: rcDeposit, Coins: "900ugnot"},
	)

	s, err := db.RealmCoinStatsFor("alpha", rcRealm, rcDeposit)
	if err != nil {
		t.Fatal(err)
	}
	if s.Legs != 3 {
		t.Errorf("legs = %d, want 3: the deposit leg is listed even though it is not netted", s.Legs)
	}
	if s.DerivedUgnot != 4000000 {
		t.Errorf("derived = %d, want 4000000: the deposit's 900 must not be in it", s.DerivedUgnot)
	}
}

// ⚠️ The one that would have been a live incident. from_addr and to_addr
// default to ” for the chain's own end of a leg, so a package with no deposit
// account must not have ” spliced into the predicate: it would match every
// mint and genesis allocation on the chain and attribute them to that package.
func TestRealmCoinIgnoresAnEmptyDepositAddress(t *testing.T) {
	db := NewTestDB(t)
	rcSeed(t, db,
		CoinTransfer{From: rcFunder, To: rcRealm, Coins: "100ugnot"},
		// The chain minting to a stranger: nothing to do with this package.
		CoinTransfer{From: "", To: rcOutside, Coins: "999999ugnot"},
		// And the chain collecting from one.
		CoinTransfer{From: rcOutside, To: "", Coins: "5ugnot"},
	)

	s, err := db.RealmCoinStatsFor("alpha", rcRealm, "")
	if err != nil {
		t.Fatal(err)
	}
	if s.Legs != 1 || s.DerivedUgnot != 100 {
		t.Fatalf("legs/derived = %d/%d, want 1/100: the chain's own legs are not this package's",
			s.Legs, s.DerivedUgnot)
	}
	flows, err := db.RealmCoinFlowsFor("alpha", rcRealm, "", 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(flows) != 1 || flows[0].Account != "banker" {
		t.Fatalf("got %+v, want one banker leg", flows)
	}
}

// A coin string is a list and only its ugnot entries are a ugnot total. The
// string is kept whole so a page can show what the chain actually said.
func TestRealmCoinKeepsTheCoinStringForOtherDenoms(t *testing.T) {
	db := NewTestDB(t)
	rcSeed(t, db, CoinTransfer{From: rcFunder, To: rcRealm, Coins: "5foo"})

	flows, err := db.RealmCoinFlowsFor("alpha", rcRealm, rcDeposit, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(flows) != 1 {
		t.Fatalf("got %d legs, want 1", len(flows))
	}
	if flows[0].Coins != "5foo" {
		t.Errorf("coins = %q, want the chain's own string", flows[0].Coins)
	}
	if flows[0].Amount != 0 {
		t.Errorf("amount = %d, want 0: a non-ugnot denom contributes nothing to a ugnot total", flows[0].Amount)
	}
}

func TestRealmCoinFlowsPage(t *testing.T) {
	db := NewTestDB(t)
	legs := make([]CoinTransfer, 0, 6)
	for i := 0; i < 6; i++ {
		legs = append(legs, CoinTransfer{From: rcFunder, To: rcRealm, Coins: "10ugnot", BlockHeight: 100 + i*10})
	}
	rcSeed(t, db, legs...)

	tests := []struct {
		name          string
		limit, offset int
		wantHeights   []int
	}{
		{"whole history", 50, 0, []int{150, 140, 130, 120, 110, 100}},
		{"first page", 2, 0, []int{150, 140}},
		{"second page resumes", 2, 2, []int{130, 120}},
		{"last page is short", 2, 5, []int{100}},
		{"offset past the end", 2, 99, nil},
		{"a zero limit is a legitimate totals-only ask", 0, 0, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := db.RealmCoinFlowsFor("alpha", rcRealm, rcDeposit, tt.limit, tt.offset)
			if err != nil {
				t.Fatal(err)
			}
			if got == nil {
				t.Fatal("page is nil, which marshals as null instead of []")
			}
			if len(got) != len(tt.wantHeights) {
				t.Fatalf("got %d rows, want %d", len(got), len(tt.wantHeights))
			}
			for i, h := range tt.wantHeights {
				if got[i].BlockHeight != h {
					t.Errorf("row %d is height %d, want %d", i, got[i].BlockHeight, h)
				}
			}
		})
	}
}

// The sign flip here is invisible: both conventions produce a plausible
// leaderboard, and the wrong one says the biggest loser is the biggest winner.
func TestRealmCoinPartiesNetFromTheCounterpartysSide(t *testing.T) {
	db := NewTestDB(t)
	rcSeed(t, db,
		// Staked 400, paid 100: down 300.
		CoinTransfer{From: rcOutside, To: rcRealm, Coins: "400ugnot"},
		CoinTransfer{From: rcRealm, To: rcOutside, Coins: "100ugnot"},
		// Only ever took money out: up 250.
		CoinTransfer{From: rcRealm, To: rcWinner, Coins: "250ugnot"},
		// A deposit leg, which belongs to the other account.
		CoinTransfer{From: rcFunder, To: rcDeposit, Coins: "900ugnot"},
	)

	parties, total, err := db.RealmCoinPartiesFor("alpha", rcRealm, rcDeposit, 50)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Fatalf("total = %d, want 2 (the storage-deposit funder is not one)", total)
	}

	byAddr := map[string]RealmCoinParty{}
	for _, c := range parties {
		byAddr[c.Address] = c
	}
	for _, tc := range []struct {
		addr                string
		sent, received, net int64
		legs                int
	}{
		{rcOutside, 400, 100, -300, 2},
		{rcWinner, 0, 250, 250, 1},
	} {
		t.Run(tc.addr, func(t *testing.T) {
			c, ok := byAddr[tc.addr]
			if !ok {
				t.Fatal("missing")
			}
			if c.Sent != tc.sent || c.Received != tc.received {
				t.Errorf("sent/received = %d/%d, want %d/%d", c.Sent, c.Received, tc.sent, tc.received)
			}
			if c.Net != tc.net {
				t.Errorf("net = %d, want %d (positive means this account came out ahead)", c.Net, tc.net)
			}
			if c.Legs != tc.legs {
				t.Errorf("legs = %d, want %d", c.Legs, tc.legs)
			}
		})
	}

	// Gross, not net: the 500-gross account that comes out behind still outranks
	// the 250-gross one that is ahead.
	if parties[0].Address != rcOutside {
		t.Errorf("ranked %q first, want the largest gross mover %q", parties[0].Address, rcOutside)
	}
}

// Netting at the point of aggregation makes an account that moved 400 each way
// indistinguishable from one that never moved any: a market maker and a
// bystander, reported as the same actor.
func TestRealmCoinPartiesKeepBothDirections(t *testing.T) {
	db := NewTestDB(t)
	rcSeed(t, db,
		CoinTransfer{From: rcOutside, To: rcRealm, Coins: "400ugnot"},
		CoinTransfer{From: rcRealm, To: rcOutside, Coins: "400ugnot"},
	)

	parties, _, err := db.RealmCoinPartiesFor("alpha", rcRealm, rcDeposit, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(parties) != 1 {
		t.Fatalf("got %d counterparties, want 1", len(parties))
	}
	c := parties[0]
	if c.Net != 0 {
		t.Errorf("net = %d, want 0", c.Net)
	}
	if c.Sent != 400 || c.Received != 400 {
		t.Errorf("sent/received = %d/%d, want 400/400: netting here loses the actor", c.Sent, c.Received)
	}
}

package store

import "testing"

const (
	realm    = "g1realm000000000000000000000000000000"
	funder   = "g1funder00000000000000000000000000000"
	payee    = "g1payee000000000000000000000000000000"
	otherNet = "othernet"
)

// seedLegs writes a small history: funded twice, paid out once, one leg on
// another chain, and one leg between two strangers. The last two are the
// interesting ones: neither may reach the realm's figures.
func seedLegs(t *testing.T, db *DB) {
	t.Helper()
	legs := []struct {
		network  string
		hash     string
		idx      int
		from, to string
		coins    string
		ugnot    int64
		height   int
		time     string
	}{
		{"alpha", "tx1", 0, funder, realm, "5000000ugnot", 5000000, 100, "2026-09-01T00:00:00Z"},
		{"alpha", "tx2", 0, funder, realm, "3000000ugnot", 3000000, 110, "2026-09-02T00:00:00Z"},
		{"alpha", "tx3", 1, realm, payee, "1000000ugnot", 1000000, 120, "2026-09-03T00:00:00Z"},
		{"alpha", "tx4", 0, funder, payee, "9000000ugnot", 9000000, 130, "2026-09-04T00:00:00Z"},
		{otherNet, "tx5", 0, funder, realm, "7000000ugnot", 7000000, 140, "2026-09-05T00:00:00Z"},
	}
	for _, l := range legs {
		if err := db.InsertCoinTransfer(l.network, l.hash, l.idx, CoinTransfer{
			From: l.from, To: l.to, Coins: l.coins, Ugnot: l.ugnot,
			BlockHeight: l.height, BlockTime: l.time,
		}); err != nil {
			t.Fatalf("insert %s: %v", l.hash, err)
		}
	}
}

func TestCoinLedgerStatsFor(t *testing.T) {
	db := NewTestDB(t)
	seedLegs(t, db)

	tests := []struct {
		name                    string
		network, addr           string
		legs                    int
		received, sent, net     int64
		firstHeight, lastHeight int
	}{
		{
			// The whole point: received minus sent, over every row, is the
			// figure held up against bank/balances.
			name:    "the realm nets its funding against its payout",
			network: "alpha", addr: realm,
			legs: 3, received: 8000000, sent: 1000000, net: 7000000,
			firstHeight: 100, lastHeight: 120,
		},
		{
			// A leg between two strangers must not reach the realm, and the
			// other chain's leg must not either: summing two chains' ugnot
			// invents a number.
			name:    "another chain's legs are not this chain's",
			network: otherNet, addr: realm,
			legs: 1, received: 7000000, sent: 0, net: 7000000,
			firstHeight: 140, lastHeight: 140,
		},
		{
			name:    "an address with no legs is zero, not an error",
			network: "alpha", addr: "g1nobody0000000000000000000000000000",
		},
		{
			// Guards the query rather than the caller: an empty address
			// matches the DEFAULT '' on both columns, so without the early
			// return this would report every chain-side leg as the realm's.
			name:    "the empty address matches nothing",
			network: "alpha", addr: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := db.CoinLedgerStatsFor(tt.network, tt.addr)
			if err != nil {
				t.Fatalf("CoinLedgerStatsFor: %v", err)
			}
			if got.Legs != tt.legs {
				t.Errorf("legs = %d, want %d", got.Legs, tt.legs)
			}
			if got.Received != tt.received || got.Sent != tt.sent || got.Net != tt.net {
				t.Errorf("received/sent/net = %d/%d/%d, want %d/%d/%d",
					got.Received, got.Sent, got.Net, tt.received, tt.sent, tt.net)
			}
			if got.FirstHeight != tt.firstHeight || got.LastHeight != tt.lastHeight {
				t.Errorf("height bounds = %d..%d, want %d..%d",
					got.FirstHeight, got.LastHeight, tt.firstHeight, tt.lastHeight)
			}
		})
	}
}

// A coin string is a list, and only its ugnot entries are a ugnot total. The
// string is kept whole so a page can show what the chain actually said.
func TestCoinLedgerKeepsTheCoinStringAndSumsOnlyUgnot(t *testing.T) {
	db := NewTestDB(t)
	mixed := "5foo,100ugnot"
	if err := db.InsertCoinTransfer("alpha", "txm", 0, CoinTransfer{
		From: funder, To: realm, Coins: mixed, Ugnot: ParseUgnot(mixed),
		BlockHeight: 10, BlockTime: "2026-09-01T00:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertCoinTransfer("alpha", "txf", 0, CoinTransfer{
		From: funder, To: realm, Coins: "5foo", Ugnot: ParseUgnot("5foo"),
		BlockHeight: 11, BlockTime: "2026-09-01T00:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}

	stats, err := db.CoinLedgerStatsFor("alpha", realm)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Net != 100 {
		t.Errorf("net = %d, want 100: a non-ugnot denom must contribute nothing", stats.Net)
	}
	rows, err := db.CoinTransfersFor("alpha", realm, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if rows[1].Coins != mixed {
		t.Errorf("coin string = %q, want %q kept verbatim", rows[1].Coins, mixed)
	}
}

func TestCoinTransfersForPagesNewestFirst(t *testing.T) {
	db := NewTestDB(t)
	seedLegs(t, db)

	tests := []struct {
		name          string
		limit, offset int
		wantHeights   []int
	}{
		{"whole history", 10, 0, []int{120, 110, 100}},
		{"first page", 2, 0, []int{120, 110}},
		{"second page resumes", 2, 2, []int{100}},
		{"offset past the end", 2, 99, nil},
		{"a zero limit asks for nothing", 0, 0, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows, err := db.CoinTransfersFor("alpha", realm, tt.limit, tt.offset)
			if err != nil {
				t.Fatalf("CoinTransfersFor: %v", err)
			}
			if rows == nil {
				t.Fatal("rows is nil, which marshals as null instead of []")
			}
			if len(rows) != len(tt.wantHeights) {
				t.Fatalf("got %d rows, want %d", len(rows), len(tt.wantHeights))
			}
			for i, h := range tt.wantHeights {
				if rows[i].BlockHeight != h {
					t.Errorf("row %d is height %d, want %d", i, rows[i].BlockHeight, h)
				}
			}
		})
	}
}

// The backfill overlaps the live pass by design, and re-running it must not
// double every figure it already wrote.
func TestInsertCoinTransferIsIdempotent(t *testing.T) {
	db := NewTestDB(t)
	for i := 0; i < 3; i++ {
		seedLegs(t, db)
	}
	stats, err := db.CoinLedgerStatsFor("alpha", realm)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Legs != 3 || stats.Net != 7000000 {
		t.Errorf("after 3 identical passes: legs=%d net=%d, want 3 and 7000000", stats.Legs, stats.Net)
	}
	if got := db.CoinLedgerRows("alpha"); got != 4 {
		t.Errorf("alpha holds %d rows, want 4", got)
	}
	if got := db.EarliestCoinTransfer("alpha"); got != 100 {
		t.Errorf("earliest = %d, want 100", got)
	}
}

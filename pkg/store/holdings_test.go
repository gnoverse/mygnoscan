package store

import "testing"

// The mirror of TestTokenSummariesReconstructTheLedger, read along the other
// axis: what one address holds rather than who holds one token.
func TestTokenPositionsReconstructOneAddress(t *testing.T) {
	db := NewTestDB(t)
	const (
		coin = "gno.land/r/x/coin.COIN.0000000"
		gold = "gno.land/r/x/gold.GOLD.0000000"
		nft  = "gno.land/r/x/gnft.GNFT.0000000"
		me   = "g1realm"
	)
	seedTransfers(t, db, "alpha", []TokenTransfer{
		{Token: coin, From: "", To: "g1funder", Value: 10000}, // a mint that is not ours
		{Token: coin, From: "g1funder", To: me, Value: 4000},
		{Token: coin, From: "g1funder", To: me, Value: 1000},
		{Token: coin, From: me, To: "g1payee", Value: 1500},
		// Received and spent in full: the position is zero but the address did
		// touch this asset, and the two states are not the same.
		{Token: gold, From: "g1funder", To: me, Value: 700},
		{Token: gold, From: me, To: "g1payee", Value: 700},
		// Not ours at all.
		{Token: gold, From: "g1funder", To: "g1other", Value: 900},
		// An NFT, riding the same event with no amount.
		{Token: nft, From: "g1funder", To: me, Value: 0},
	})

	got, err := db.TokenPositions("alpha", me)
	if err != nil {
		t.Fatal(err)
	}
	byToken := map[string]TokenPosition{}
	for _, p := range got {
		byToken[p.Token] = p
	}
	if len(got) != 3 {
		t.Fatalf("got %d positions, want 3 (the token this address never touched is not one): %+v", len(got), got)
	}

	for _, tc := range []struct {
		token                   string
		balance, received, sent int64
		transfers               int
		fungible                bool
		symbol                  string
	}{
		{coin, 3500, 5000, 1500, 3, true, "COIN"},
		// Kept at zero rather than filtered: a treasury that has been emptied
		// is not a treasury that was never funded.
		{gold, 0, 700, 700, 2, true, "GOLD"},
		// Zero that means "does not apply". The flag is what stops the page
		// printing it as a balance.
		{nft, 0, 0, 0, 1, false, "GNFT"},
	} {
		p, ok := byToken[tc.token]
		if !ok {
			t.Errorf("%s: no position", tc.token)
			continue
		}
		if p.Balance != tc.balance || p.Received != tc.received || p.Sent != tc.sent {
			t.Errorf("%s: balance/received/sent = %d/%d/%d, want %d/%d/%d",
				tc.token, p.Balance, p.Received, p.Sent, tc.balance, tc.received, tc.sent)
		}
		if p.Transfers != tc.transfers {
			t.Errorf("%s: transfers = %d, want %d (only the legs this address is on)", tc.token, p.Transfers, tc.transfers)
		}
		if p.Fungible != tc.fungible {
			t.Errorf("%s: fungible = %v, want %v", tc.token, p.Fungible, tc.fungible)
		}
		if p.Symbol != tc.symbol {
			t.Errorf("%s: symbol = %q, want %q", tc.token, p.Symbol, tc.symbol)
		}
	}
}

// Every query in this package is network-scoped, and a holdings query joined on
// an address alone is exactly the shape that forgets: one address holds
// different amounts on two chains and neither figure is the sum.
func TestTokenPositionsAreNetworkScoped(t *testing.T) {
	db := NewTestDB(t)
	const tok = "gno.land/r/x/coin.COIN.0000000"
	seedTransfers(t, db, "alpha", []TokenTransfer{{Token: tok, From: "g1f", To: "g1realm", Value: 100}})
	seedTransfers(t, db, "beta", []TokenTransfer{{Token: tok, From: "g1f", To: "g1realm", Value: 900}})

	for _, tc := range []struct {
		network string
		want    int64
	}{{"alpha", 100}, {"beta", 900}} {
		got, err := db.TokenPositions(tc.network, "g1realm")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Balance != tc.want {
			t.Errorf("%s: got %+v, want one position of %d", tc.network, got, tc.want)
		}
	}
}

func TestHolderTransfersSeesBothDirections(t *testing.T) {
	db := NewTestDB(t)
	const tok = "gno.land/r/x/coin.COIN.0000000"
	seedTransfers(t, db, "alpha", []TokenTransfer{
		{Token: tok, From: "g1funder", To: "g1realm", Value: 100, BlockHeight: 10},
		{Token: tok, From: "g1realm", To: "g1payee", Value: 40, BlockHeight: 20},
		{Token: tok, From: "g1funder", To: "g1other", Value: 70, BlockHeight: 30},
	})

	got, err := db.HolderTransfers("alpha", "g1realm", 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d transfers, want the 2 this address is on: %+v", len(got), got)
	}
	// Newest first, so a chart that reverses for a running total gets the order
	// it expects.
	if got[0].BlockHeight != 20 || got[1].BlockHeight != 10 {
		t.Errorf("order = %d, %d; want newest first", got[0].BlockHeight, got[1].BlockHeight)
	}
}

// An empty ledger has no oldest row, and MIN over no rows is NULL. Returning ""
// rather than erroring is what lets the API omit the caveat instead of failing
// the whole response over it.
func TestEarliestTokenTransfer(t *testing.T) {
	db := NewTestDB(t)
	if got := db.EarliestTokenTransfer("alpha"); got != "" {
		t.Errorf("empty ledger = %q, want \"\"", got)
	}
	seedTransfers(t, db, "alpha", []TokenTransfer{
		{Token: "t.T.0", From: "", To: "g1a", Value: 1, BlockTime: "2026-03-01T00:00:00Z"},
		{Token: "t.T.0", From: "g1a", To: "g1b", Value: 1, BlockTime: "2026-01-01T00:00:00Z"},
	})
	if got := db.EarliestTokenTransfer("alpha"); got != "2026-01-01T00:00:00Z" {
		t.Errorf("got %q, want the oldest row rather than the first inserted", got)
	}
}

// Paging the GRC20 table, and the count that makes a full page readable.
//
// Without the count, "500 rows" and "500 rows of 4,000" look identical, which is
// the whole reason this pair exists: r/gnoswap/pool and r/gnoswap/router both
// sat exactly on the old cap with nothing on the page saying so.
func TestHolderTransfersPages(t *testing.T) {
	db := NewTestDB(t)
	const tok = "gno.land/r/x/coin.COIN.0000000"
	xs := make([]TokenTransfer, 0, 6)
	for i := 0; i < 6; i++ {
		xs = append(xs, TokenTransfer{
			Token: tok, From: "g1funder", To: "g1realm", Value: 10, BlockHeight: 10 + i*10,
		})
	}
	// One leg the address is not on, and one on another chain: neither may
	// reach the count or the page.
	xs = append(xs, TokenTransfer{Token: tok, From: "g1funder", To: "g1other", Value: 10, BlockHeight: 99})
	seedTransfers(t, db, "alpha", xs)
	seedTransfers(t, db, "beta", []TokenTransfer{
		{Token: tok, From: "g1funder", To: "g1realm", Value: 10, BlockHeight: 5},
	})

	total, err := db.HolderTransferCount("alpha", "g1realm")
	if err != nil {
		t.Fatal(err)
	}
	if total != 6 {
		t.Fatalf("count = %d, want 6: the stranger's leg and the other chain's must not be in it", total)
	}

	tests := []struct {
		name          string
		limit, offset int
		wantHeights   []int
	}{
		{"whole history", 50, 0, []int{60, 50, 40, 30, 20, 10}},
		{"first page", 2, 0, []int{60, 50}},
		{"second page resumes where the first stopped", 2, 2, []int{40, 30}},
		{"last page is short", 2, 5, []int{10}},
		{"offset at the end", 2, 6, nil},
		{"offset past the end", 2, 99, nil},
		{"a zero limit asks for nothing", 0, 0, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := db.HolderTransfers("alpha", "g1realm", tt.limit, tt.offset)
			if err != nil {
				t.Fatalf("HolderTransfers: %v", err)
			}
			if got == nil {
				t.Fatal("page is nil, which marshals as null instead of []")
			}
			if len(got) != len(tt.wantHeights) {
				t.Fatalf("got %d rows, want %d: %+v", len(got), len(tt.wantHeights), got)
			}
			for i, h := range tt.wantHeights {
				if got[i].BlockHeight != h {
					t.Errorf("row %d is height %d, want %d", i, got[i].BlockHeight, h)
				}
			}
		})
	}
}

// Paging covers every row exactly once, including when a whole page shares one
// block height.
//
// ⚠️ This does **not** prove the ORDER BY tiebreak is load-bearing: checked
// 2026-09-25, the test still passes with `ORDER BY block_height DESC` alone,
// because SQLite happens to return these rows in a stable order anyway. That is
// an implementation detail and not a guarantee, which is why the tiebreak is
// there, but do not read a green run here as evidence it is doing work. What
// this pins is the weaker and still worth-having property in the name.
func TestHolderTransfersPagesCoverEveryRowOnce(t *testing.T) {
	db := NewTestDB(t)
	const tok = "gno.land/r/x/coin.COIN.0000000"
	legs := make([]TokenTransfer, 0, 8)
	for i := 0; i < 8; i++ {
		legs = append(legs, TokenTransfer{
			Token: tok, From: "g1funder", To: "g1realm", Value: int64(i + 1), BlockHeight: 42,
		})
	}
	seedTransfers(t, db, "alpha", legs)

	seen := map[string]bool{}
	for offset := 0; offset < 8; offset += 3 {
		page, err := db.HolderTransfers("alpha", "g1realm", 3, offset)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range page {
			key := row.TxHash
			if seen[key] {
				t.Fatalf("row %s came back on two pages, so another was skipped", key)
			}
			seen[key] = true
		}
	}
	if len(seen) != 8 {
		t.Errorf("walked %d distinct rows across the pages, want 8", len(seen))
	}
}

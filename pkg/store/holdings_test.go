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

	got, err := db.HolderTransfers("alpha", "g1realm", 50)
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

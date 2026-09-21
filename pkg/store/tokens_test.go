package store

import (
	"testing"
	"time"
)

func TestTokenKeyParts(t *testing.T) {
	tests := []struct {
		key      string
		wantPath string
		wantName string
	}{
		// The usual shape. Split from the right, because the path itself
		// contains dots.
		{"gno.land/r/gnoland/wugnot.wugnot.0000000", "gno.land/r/gnoland/wugnot", "wugnot"},
		{"gno.land/r/gnoswap/gns.GNS.0000000", "gno.land/r/gnoswap/gns", "GNS"},
		{"gno.land/r/gnoswap/gov/xgns.xGNS.0000000", "gno.land/r/gnoswap/gov/xgns", "xGNS"},
		// Two live mainnet tokens emit a bare symbol instead. Inventing a realm
		// path from one would put "COVID" in a column headed "realm".
		{"COVID", "", "COVID"},
		{"META", "", "META"},
		{"", "", ""},
	}
	for _, tt := range tests {
		gotPath, gotName := TokenKeyParts(tt.key)
		if gotPath != tt.wantPath || gotName != tt.wantName {
			t.Errorf("TokenKeyParts(%q) = (%q, %q), want (%q, %q)",
				tt.key, gotPath, gotName, tt.wantPath, tt.wantName)
		}
	}
}

func TestParseTokenValue(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want int64
	}{{"0", 0}, {"6134719058", 6134719058}, {" 42 ", 42}, {"", 0}, {"nope", 0}} {
		if got := ParseTokenValue(tt.in); got != tt.want {
			t.Errorf("ParseTokenValue(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

func seedTransfers(t *testing.T, db *DB, network string, xs []TokenTransfer) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	for i, x := range xs {
		if x.BlockTime == "" {
			x.BlockTime = now
		}
		if x.BlockHeight == 0 {
			x.BlockHeight = i + 1
		}
		if err := db.InsertTokenTransfer(network, "TX"+string(rune('a'+i)), 0, x); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
}

// Supply is mints minus burns, and holders are reconstructed balances rather
// than distinct recipients: an address that received and sent everything on is
// not a holder, and counting it would overstate every token on the chain.
func TestTokenSummariesReconstructTheLedger(t *testing.T) {
	db := NewTestDB(t)
	const tok = "gno.land/r/x/coin.COIN.0000000"
	seedTransfers(t, db, "alpha", []TokenTransfer{
		{Token: tok, From: "", To: "g1a", Value: 1000}, // mint
		{Token: tok, From: "", To: "g1b", Value: 500},  // mint
		{Token: tok, From: "g1a", To: "g1c", Value: 200},
		{Token: tok, From: "g1c", To: "g1b", Value: 200}, // g1c passes it all on
		{Token: tok, From: "g1b", To: "", Value: 100},    // burn
	})

	all, err := db.TokenSummaries("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("got %d assets, want 1: %+v", len(all), all)
	}
	got := all[0]
	if got.Supply != 1400 {
		t.Errorf("supply = %d, want 1500 minted minus 100 burned", got.Supply)
	}
	// g1a 800, g1b 600, g1c 0. Three addresses received; two hold.
	if got.Holders != 2 {
		t.Errorf("holders = %d, want 2: an address that passed it all on is not a holder", got.Holders)
	}
	if got.Transfers != 5 {
		t.Errorf("transfers = %d, want 5", got.Transfers)
	}
	if got.PkgPath != "gno.land/r/x/coin" || got.Symbol != "COIN" {
		t.Errorf("key split = (%q, %q)", got.PkgPath, got.Symbol)
	}
	if !got.Fungible {
		t.Error("fungible = false, want true for transfers carrying amounts")
	}
}

// GRC721 emits the same Transfer event without an amount. Summing those gives a
// supply of 0 and no holders, which is not "empty" but "this arithmetic does
// not apply", so the summary says so rather than printing a confident zero.
func TestTokenSummariesFlagNonFungible(t *testing.T) {
	db := NewTestDB(t)
	const nft = "gno.land/r/x/gnft.GNFT.0000000"
	seedTransfers(t, db, "alpha", []TokenTransfer{
		{Token: nft, From: "", To: "g1a", Value: 0},
		{Token: nft, From: "g1a", To: "g1b", Value: 0},
	})

	all, err := db.TokenSummaries("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if all[0].Fungible {
		t.Error("fungible = true, want false when no transfer carries an amount")
	}
	if all[0].Supply != 0 || all[0].Holders != 0 {
		t.Errorf("summary = %+v, want the counts suppressed rather than computed", all[0])
	}
	// The transfers themselves are still real and still counted.
	if all[0].Transfers != 2 {
		t.Errorf("transfers = %d, want 2", all[0].Transfers)
	}
}

// Every GRC20 event reports the library as its pkg_path, so keying on it would
// collapse every asset on the chain into one row.
func TestTokenSummariesKeepTokensApart(t *testing.T) {
	db := NewTestDB(t)
	seedTransfers(t, db, "alpha", []TokenTransfer{
		{Token: "gno.land/r/x/a.A.0000000", From: "", To: "g1a", Value: 10},
		{Token: "gno.land/r/x/b.B.0000000", From: "", To: "g1a", Value: 20},
	})

	all, err := db.TokenSummaries("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("got %d assets, want 2: %+v", len(all), all)
	}
}

func TestTokenSummariesScopeByNetwork(t *testing.T) {
	db := NewTestDB(t)
	const tok = "gno.land/r/x/coin.COIN.0000000"
	seedTransfers(t, db, "alpha", []TokenTransfer{{Token: tok, From: "", To: "g1a", Value: 10}})
	seedTransfers(t, db, "beta", []TokenTransfer{{Token: tok, From: "", To: "g1a", Value: 999}})

	alpha, err := db.TokenSummaries("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if len(alpha) != 1 || alpha[0].Supply != 10 {
		t.Errorf("alpha = %+v, want only its own 10", alpha)
	}
	// The same path on two chains is two different assets with different
	// holders, so the all-networks view keeps them as separate rows.
	both, err := db.TokenSummaries("")
	if err != nil {
		t.Fatal(err)
	}
	if len(both) != 2 {
		t.Errorf("all networks = %d rows, want one per chain", len(both))
	}
}

func TestTopHoldersRanks(t *testing.T) {
	db := NewTestDB(t)
	const tok = "gno.land/r/x/coin.COIN.0000000"
	seedTransfers(t, db, "alpha", []TokenTransfer{
		{Token: tok, From: "", To: "g1big", Value: 900},
		{Token: tok, From: "", To: "g1mid", Value: 500},
		{Token: tok, From: "", To: "g1gone", Value: 100},
		{Token: tok, From: "g1gone", To: "g1big", Value: 100},
	})

	holders, err := db.TopHolders("alpha", tok, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(holders) != 2 {
		t.Fatalf("got %d holders, want 2: %+v", len(holders), holders)
	}
	if holders[0].Address != "g1big" || holders[0].Balance != 1000 {
		t.Errorf("top = %+v, want g1big with 1000", holders[0])
	}
}

// Inserting the same event twice must not double the supply: the sync loop
// re-walks blocks and an INSERT that counted twice would inflate every figure.
func TestInsertTokenTransferIsIdempotent(t *testing.T) {
	db := NewTestDB(t)
	x := TokenTransfer{Token: "gno.land/r/x/c.C.0000000", From: "", To: "g1a", Value: 10, BlockHeight: 1, BlockTime: time.Now().UTC().Format(time.RFC3339)}
	for i := 0; i < 3; i++ {
		if err := db.InsertTokenTransfer("alpha", "TXsame", 0, x); err != nil {
			t.Fatal(err)
		}
	}
	all, _ := db.TokenSummaries("alpha")
	if all[0].Transfers != 1 || all[0].Supply != 10 {
		t.Errorf("summary = %+v, want the repeated insert ignored", all[0])
	}
}

// The supply series is cumulative from the first mint, not from the start of
// the window: a 30-day chart of an old token must start at its real supply.
func TestTokenSupplyOverTimeIsCumulative(t *testing.T) {
	db := NewTestDB(t)
	const tok = "gno.land/r/x/coin.COIN.0000000"
	old := time.Now().UTC().AddDate(0, 0, -100).Format(time.RFC3339)
	recent := time.Now().UTC().AddDate(0, 0, -1).Format(time.RFC3339)
	seedTransfers(t, db, "alpha", []TokenTransfer{
		{Token: tok, From: "", To: "g1a", Value: 1000, BlockTime: old},
		{Token: tok, From: "", To: "g1a", Value: 5, BlockTime: recent},
	})

	pts, err := db.TokenSupplyOverTime("alpha", tok, 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 1 {
		t.Fatalf("got %d points, want only the one inside the window: %+v", len(pts), pts)
	}
	if pts[0].Blocks != 1005 {
		t.Errorf("supply = %d, want the running total including the mint before the window", pts[0].Blocks)
	}
}

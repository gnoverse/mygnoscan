package store

import (
	"fmt"
	"testing"

	"github.com/moul/mygnoscan/pkg/config"
)

// A BankMsgSend carries a coin list, and only the native denom belongs in a
// ugnot total.
//
// Summing the coin string in SQL could not express that. There is no split, so
// the denom came out with REPLACE and the remainder went through
// CAST(... AS INTEGER), which takes the leading numeric prefix and discards the
// rest without erroring. So it never failed, it just answered: "5foo,100ugnot"
// summed as 5, and "5foo" invented 5 ugnot out of a transfer that moved none.
//
// Coin order is not something a sender has to get right for us, and a
// non-native transfer is not exotic, so both were reachable.
func TestBankVolumeCountsOnlyUgnot(t *testing.T) {
	db := NewTestDB(t)
	db.SetConfiguredNetworks([]config.NetworkConfig{{ID: "a"}})

	sends := []struct {
		hash, amount string
	}{
		{"plain", "1000000ugnot"},
		{"ugnot-first", "100ugnot,5foo"},
		{"ugnot-second", "5foo,100ugnot"},
		{"foreign-only", "5foo"},
	}
	for i, s := range sends {
		if err := db.InsertBankSend("a", s.hash, 100+i, "2026-09-21T10:00:00Z",
			"g1sender", "g1receiver", s.amount, true); err != nil {
			t.Fatalf("InsertBankSend(%s): %v", s.hash, err)
		}
	}

	stats, err := db.GetBankStats("a")
	if err != nil {
		t.Fatalf("GetBankStats: %v", err)
	}

	// 1000000 + 100 + 100, and nothing from the foo-only send.
	const want = 1_000_200
	if stats.TotalVolume != want {
		t.Errorf("total volume = %d, want %d", stats.TotalVolume, want)
	}
	if stats.TotalSends != len(sends) {
		t.Errorf("total sends = %d, want %d: a foreign denom is still a send", stats.TotalSends, len(sends))
	}
	for _, row := range stats.TopSenders {
		if row.Total != want {
			t.Errorf("top sender total = %d, want %d", row.Total, want)
		}
	}
}

// The column is filled for rows written before it existed, and NULL is the
// only marker: a second pass has nothing left to look at.
func TestBankSendUgnotBackfillIsIdempotent(t *testing.T) {
	db := NewTestDB(t)
	db.SetConfiguredNetworks([]config.NetworkConfig{{ID: "a"}})

	// Distinct hashes: bank_sends is UNIQUE(network, tx_hash, from, to) and the
	// insert is OR IGNORE, so reusing one hash would store a single row.
	for i, amount := range []string{"7ugnot", "5foo,100ugnot", "5foo"} {
		if err := db.InsertBankSend("a", fmt.Sprintf("tx%d", i), 100+i, "2026-09-21T10:00:00Z",
			"g1sender", "g1receiver", amount, true); err != nil {
			t.Fatalf("InsertBankSend: %v", err)
		}
	}

	// What an existing database looks like: rows whose coin string was never
	// parsed, because nothing parsed it at write time.
	if _, err := db.SQL().Exec(`UPDATE bank_sends SET ugnot_amount = NULL`); err != nil {
		t.Fatalf("clear column: %v", err)
	}

	volume := func() int64 {
		var v int64
		if err := db.SQL().QueryRow(`SELECT ` + amountExpr + ` FROM bank_sends`).Scan(&v); err != nil {
			t.Fatalf("read volume: %v", err)
		}
		return v
	}
	if got := volume(); got != 0 {
		t.Fatalf("volume before the backfill = %d, want 0: the column is the marker", got)
	}

	for pass := 1; pass <= 3; pass++ {
		if err := migrateBankSendUgnot(db.SQL()); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		if got := volume(); got != 107 {
			t.Fatalf("pass %d: volume = %d, want 107", pass, got)
		}
	}
}

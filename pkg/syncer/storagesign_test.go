package syncer

import (
	"testing"

	"github.com/moul/mygnoscan/pkg/indexer"
)

// TestRecordStorageEventsKeepsTheChainsSign is the test that was missing.
//
// The chain emits a signed byte delta: positive when a realm grows, negative
// when it frees. The syncer used to negate the unlock case, so freed bytes
// landed positive and every aggregate over storage_events added them to used
// bytes instead of cancelling them. On mainnet that put
// gno.land/r/gnoland/wugnot at 10,918,147 bytes where the chain says 1,180,507.
//
// The store tests never caught it because their fixtures seed the correct sign
// directly, which is a sign the ingestion boundary itself needs covering.
func TestRecordStorageEventsKeepsTheChainsSign(t *testing.T) {
	s, _, db := newTestSyncer(t, "alpha")

	const when = "2026-01-01T00:00:00Z"
	tx := indexer.Transaction{
		Hash:        "txsign",
		BlockHeight: 42,
		Response: &indexer.TxResponse{Events: []indexer.TxEvent{
			{
				Typename:   "StorageDepositEvent",
				PkgPath:    "gno.land/r/ns/x",
				BytesDelta: 2079,
				FeeDelta:   &indexer.Coin{Denom: "ugnot", Amount: 207900},
			},
			{
				// Exactly as the chain emits it: the delta is already negative
				// and the refund is a positive amount of money returned.
				Typename:   "StorageUnlockEvent",
				PkgPath:    "gno.land/r/ns/x",
				BytesDelta: -2093,
				FeeRefund:  &indexer.Coin{Denom: "ugnot", Amount: 209300},
			},
		}},
	}

	if n := s.recordStorageEvents(tx, when); n != 2 {
		t.Fatalf("recorded %d events, want 2", n)
	}

	type row struct {
		kind  string
		bytes int
		fee   int
	}
	rows, err := db.SQL().Query(
		`SELECT kind, bytes_delta, fee FROM storage_events
		  WHERE network = 'alpha' AND tx_hash = 'txsign' ORDER BY event_index`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.kind, &r.bytes, &r.fee); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, r)
	}

	want := []row{
		{"deposit", 2079, 207900},
		// Both columns carry the chain's sign, so summing either answers the
		// same question and the two can be checked against each other.
		{"unlock", -2093, -209300},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d rows, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	// The realm freed more than it took, so its footprint is negative here.
	// The point is that it is -14 and not +4172.
	var net, fee int
	if err := db.SQL().QueryRow(
		`SELECT COALESCE(SUM(bytes_delta), 0), COALESCE(SUM(fee), 0)
		   FROM storage_events WHERE network = 'alpha'`).Scan(&net, &fee); err != nil {
		t.Fatalf("sum: %v", err)
	}
	if net != -14 {
		t.Errorf("SUM(bytes_delta) = %d, want -14", net)
	}
	// The invariant that makes this self-checking: bytes times the storage
	// price is the fee, on every row and therefore on any sum of them.
	if fee != net*100 {
		t.Errorf("SUM(fee) = %d, want %d (SUM(bytes_delta) * 100)", fee, net*100)
	}
}

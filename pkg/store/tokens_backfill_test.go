package store

import (
	"testing"
)

// seedBackfillBlocks writes a contiguous block range so the backfill has a history to
// walk. Only height matters here; the ledger's gap is defined by block heights.
func seedBackfillBlocks(t *testing.T, d *DB, network string, from, to int) {
	t.Helper()
	for h := from; h <= to; h++ {
		if _, err := d.db.Exec(
			`INSERT OR REPLACE INTO blocks (network, height, time, num_txs) VALUES (?, ?, ?, 0)`,
			network, h, "2026-09-01T00:00:00Z"); err != nil {
			t.Fatal(err)
		}
	}
}

func seedBackfillTransfer(t *testing.T, d *DB, network string, height int) {
	t.Helper()
	if _, err := d.db.Exec(`
		INSERT OR REPLACE INTO token_transfers
			(network, tx_hash, event_idx, token, pkg_path, from_addr, to_addr, value, block_height, block_time)
		VALUES (?, ?, 0, 'tok', 'gno.land/p/nt/grc20/v0', 'g1a', 'g1b', 1, ?, ?)`,
		network, "tx-"+backfillItoa(height), height, "2026-09-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
}

func backfillItoa(n int) string { // strconv without the import churn in a test file
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// An empty ledger on a database with history means the whole stored range is
// the gap: this is the state every pre-existing deployment is in.
func TestTokenBackfillRangeCoversEverythingWhenTheLedgerIsEmpty(t *testing.T) {
	d := NewTestDB(t)
	seedBackfillBlocks(t, d, "gnoland1", 100, 400)

	from, to, more, err := d.TokenBackfillRange("gnoland1", 50)
	if err != nil {
		t.Fatal(err)
	}
	if !more || from != 100 || to != 150 {
		t.Fatalf("got from=%d to=%d more=%v, want 100..150 with work left", from, to, more)
	}
}

// With transfers recorded from 300 forward, the gap is 100..300 and the walk
// must stop where the forward fill begins rather than redo it.
func TestTokenBackfillRangeStopsAtTheForwardFill(t *testing.T) {
	d := NewTestDB(t)
	seedBackfillBlocks(t, d, "gnoland1", 100, 400)
	seedBackfillTransfer(t, d, "gnoland1", 300)

	from, to, more, err := d.TokenBackfillRange("gnoland1", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if !more || from != 100 || to != 300 {
		t.Fatalf("got from=%d to=%d more=%v, want 100..300", from, to, more)
	}
}

// The cursor is what makes the walk resumable across passes and restarts.
func TestTokenBackfillRangeResumesFromTheCursor(t *testing.T) {
	d := NewTestDB(t)
	seedBackfillBlocks(t, d, "gnoland1", 100, 400)
	seedBackfillTransfer(t, d, "gnoland1", 300)

	if err := d.SetTokenBackfillCursor("gnoland1", 250); err != nil {
		t.Fatal(err)
	}
	from, to, more, err := d.TokenBackfillRange("gnoland1", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if !more || from != 250 || to != 300 {
		t.Fatalf("got from=%d to=%d more=%v, want 250..300", from, to, more)
	}

	// Meeting the forward fill ends the work, and it must stay ended.
	if err := d.SetTokenBackfillCursor("gnoland1", 300); err != nil {
		t.Fatal(err)
	}
	if _, _, more, err = d.TokenBackfillRange("gnoland1", 1000); err != nil || more {
		t.Fatalf("the walk met the forward fill, want no work left; more=%v err=%v", more, err)
	}
}

// A database with no blocks has nothing to repair, and must not report a range
// that would send the syncer fetching height 0.
func TestTokenBackfillRangeIsEmptyWithoutHistory(t *testing.T) {
	d := NewTestDB(t)
	_, _, more, err := d.TokenBackfillRange("gnoland1", 100)
	if err != nil || more {
		t.Fatalf("no blocks stored, want no work; more=%v err=%v", more, err)
	}
}

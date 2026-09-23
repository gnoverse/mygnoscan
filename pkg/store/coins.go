package store

import (
	"database/sql"
	"errors"
)

// The native coin ledger: one row per TransferEvent leg, written by the syncer.
//
// The GRC20 half of the defi tab has read a local table since it shipped
// (token_transfers). The native half did not have one, so it re-walked the whole
// of a realm's history from the tx-indexer on every cold request. That cost
// ~2.1s for a realm holding nothing at all, because the latency is the indexer
// resolving a chain-wide event filter rather than anything about the answer, so
// no amount of response caching could reach it (ADR 0043).
//
// Everything here is the same shape as tokens.go, deliberately: two ledgers of
// the same thing in two shapes is how one of them ends up with the bug the other
// already fixed.

// CoinTransfer is one leg of one bank transfer.
type CoinTransfer struct {
	TxHash   string `json:"tx_hash"`
	EventIdx int    `json:"event_idx"`
	From     string `json:"from_addr"`
	To       string `json:"to_addr"`
	// Coins is the chain's own string and may be a list ("5foo,100ugnot").
	// Ugnot is the ugnot entries of it, summed. Both, because a total has to add
	// a number and a page has to show what the chain actually said: parsing the
	// denom out in SQL invents a figure whenever the list is not pure ugnot
	// (ADR 0041).
	Coins       string `json:"coins"`
	Ugnot       int64  `json:"ugnot"`
	BlockHeight int    `json:"block_height"`
	BlockTime   string `json:"block_time,omitempty"`
}

// CoinLedgerStats is what one address's whole history adds up to, computed in
// SQL over every row rather than over a page.
type CoinLedgerStats struct {
	// Legs counts rows. Received and Sent are ugnot, both positive; Net is
	// Received minus Sent and is the figure compared against bank/balances.
	Legs     int   `json:"legs"`
	Received int64 `json:"received"`
	Sent     int64 `json:"sent"`
	Net      int64 `json:"net"`
	// FirstHeight and LastHeight bound the history. Zero on an address with no
	// legs, which a caller must not render as block zero.
	FirstHeight int    `json:"first_height"`
	LastHeight  int    `json:"last_height"`
	FirstTime   string `json:"first_time,omitempty"`
	LastTime    string `json:"last_time,omitempty"`
}

// InsertCoinTransfer stores one leg, idempotently.
//
// event_idx is the index over *all* events in the transaction, not over transfer
// events only, for the same reason storage_events and token_transfers do it: the
// number has to stay stable across passes, and filtering first would renumber
// every row whenever the event list changed shape.
func (d *DB) InsertCoinTransfer(network, txHash string, eventIdx int, t CoinTransfer) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	_, err := d.db.Exec(`
		INSERT OR IGNORE INTO coin_transfers
			(network, tx_hash, event_idx, from_addr, to_addr, coins, ugnot, block_height, block_time)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		network, txHash, eventIdx, t.From, t.To, t.Coins, t.Ugnot, t.BlockHeight, t.BlockTime)
	return err
}

// CoinLedgerStatsFor aggregates one address's legs.
//
// Over every row, never over a page: this is the figure held up against the
// chain's own bank/balances, and the two agreeing is what says the history below
// it is complete. A paged sum would report a gap that is an artefact of the page
// size, which is exactly the mistake the old per-request derivation was careful
// to avoid and which is easier to make once paging is cheap.
func (d *DB) CoinLedgerStatsFor(network, addr string) (CoinLedgerStats, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var s CoinLedgerStats
	if addr == "" {
		return s, nil
	}
	var firstTime, lastTime sql.NullString
	err := d.db.QueryRow(`
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN to_addr   = ? THEN ugnot ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN from_addr = ? THEN ugnot ELSE 0 END), 0),
		       COALESCE(MIN(block_height), 0),
		       COALESCE(MAX(block_height), 0),
		       MIN(NULLIF(block_time, '')),
		       MAX(NULLIF(block_time, ''))
		  FROM coin_transfers
		 WHERE network = ? AND (from_addr = ? OR to_addr = ?)`,
		addr, addr, network, addr, addr,
	).Scan(&s.Legs, &s.Received, &s.Sent, &s.FirstHeight, &s.LastHeight, &firstTime, &lastTime)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return CoinLedgerStats{}, err
	}
	s.Net = s.Received - s.Sent
	s.FirstTime, s.LastTime = firstTime.String, lastTime.String
	return s, nil
}

// CoinTransfersFor returns one page of an address's legs, newest first.
//
// A leg where the address is on both ends is returned once, and a caller summing
// the page would net it to zero, which is correct: a self-transfer moves nothing.
func (d *DB) CoinTransfersFor(network, addr string, limit, offset int) ([]CoinTransfer, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	out := []CoinTransfer{}
	if addr == "" || limit <= 0 {
		return out, nil
	}
	rows, err := d.db.Query(`
		SELECT tx_hash, event_idx, from_addr, to_addr, coins, ugnot, block_height, block_time
		  FROM coin_transfers
		 WHERE network = ? AND (from_addr = ? OR to_addr = ?)
		 ORDER BY block_height DESC, tx_hash DESC, event_idx DESC
		 LIMIT ? OFFSET ?`, network, addr, addr, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var t CoinTransfer
		var blockTime sql.NullString
		if err := rows.Scan(&t.TxHash, &t.EventIdx, &t.From, &t.To,
			&t.Coins, &t.Ugnot, &t.BlockHeight, &blockTime); err != nil {
			return nil, err
		}
		t.BlockTime = blockTime.String
		out = append(out, t)
	}
	return out, rows.Err()
}

// EarliestCoinTransfer is the oldest block this ledger holds for a network, or 0
// when it holds nothing.
//
// The honesty check the GRC20 ledger needs too: a position reconstructed from a
// ledger that starts after the package's own deploy is a floor, not a figure.
// Reporting the floor lets a page say which it is showing rather than leaving a
// reader to assume (gnoverse/mygnoscan's GRC20 ledger shipped without this and
// reported a truncated supply as exact).
func (d *DB) EarliestCoinTransfer(network string) int {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var h sql.NullInt64
	if err := d.db.QueryRow(
		`SELECT MIN(block_height) FROM coin_transfers WHERE ` + d.networkFilter("network", network),
	).Scan(&h); err != nil {
		return 0
	}
	return int(h.Int64)
}

// CoinLedgerRows is how many legs a network's ledger holds, for the health page
// and for deciding whether a backfill has produced anything yet.
func (d *DB) CoinLedgerRows(network string) int {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var n int
	if err := d.db.QueryRow(
		`SELECT COUNT(*) FROM coin_transfers WHERE ` + d.networkFilter("network", network),
	).Scan(&n); err != nil {
		return 0
	}
	return n
}

// CoinBackfillCursorKey and CoinBackfillDoneKey namespace the native ledger's
// historical backfill in sync_state.
//
// Two keys rather than one, because "how far has it got" and "is it finished"
// are different questions and collapsing them loses the resume point: a run
// interrupted at height 200,000 has to start there and not at zero, and a
// finished run has to not start at all.
//
// They live here for the same reason BlocksBackfillDoneKey does: a reader of the
// table needs to know whether what it holds is the whole history yet, so the
// marker is part of the schema's vocabulary rather than the sync loop's private
// state.
func CoinBackfillCursorKey(network string) string { return "coin_backfill_cursor:" + network }
func CoinBackfillDoneKey(network string) string   { return "coin_backfill_done:" + network }

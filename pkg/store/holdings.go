package store

// What one address holds, over the GRC20 transfer ledger.
//
// The mirror of TopHolders, which asks "who holds this token": these ask "what
// does this address hold", which is the question a contract's page has. Same
// ledger, same reconstruction (an empty `from` is a mint, an empty `to` a burn),
// read along the other axis.
//
// ⚠️ That ledger is only as old as the syncer's first pass over a chain, so a
// position here is a floor rather than a figure, and the API layer says so. A
// realm that received tokens before the ledger shipped reads short.

// TokenPosition is one asset an address holds, or has held.
type TokenPosition struct {
	Token   string `json:"token"`
	PkgPath string `json:"pkg_path,omitempty"`
	Symbol  string `json:"symbol"`
	Balance int64  `json:"balance"`
	// Received and Sent are the two halves of the balance, kept apart because
	// "holds 0" and "never touched it" are different states and their sum is
	// not. A realm that took in a million and paid it all out is not a realm
	// that never saw the token.
	Received  int64  `json:"received"`
	Sent      int64  `json:"sent"`
	Transfers int    `json:"transfers"`
	FirstSeen string `json:"first_seen,omitempty"`
	LastSeen  string `json:"last_seen,omitempty"`
	// Fungible says whether Balance means anything. GRC721 rides the same
	// Transfer event and carries no amount, so its legs all parse to zero:
	// summing them yields 0, which is not "holds none", it is "this arithmetic
	// does not apply". Detected from the rows rather than from the library path,
	// the same way TokenSummaries does it.
	Fungible bool `json:"fungible"`
}

// TokenPositions returns every asset an address has touched, richest first.
//
// Zero balances are kept rather than filtered: a position that went out again is
// part of what a contract did, and dropping it would make a treasury that has
// been emptied indistinguishable from one that was never funded.
func (d *DB) TokenPositions(network, addr string) ([]TokenPosition, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	rows, err := d.db.Query(`
		SELECT token,
		       MAX(pkg_path),
		       SUM(CASE WHEN to_addr   = ?1 THEN value ELSE 0 END) AS received,
		       SUM(CASE WHEN from_addr = ?1 THEN value ELSE 0 END) AS sent,
		       COUNT(*),
		       MIN(block_time),
		       MAX(block_time),
		       MAX(value)
		  FROM token_transfers
		 WHERE network = ?2 AND (from_addr = ?1 OR to_addr = ?1)
		 GROUP BY token
		 ORDER BY (received - sent) DESC, token ASC`, addr, network)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []TokenPosition{}
	for rows.Next() {
		var p TokenPosition
		var maxValue int64
		if err := rows.Scan(&p.Token, &p.PkgPath, &p.Received, &p.Sent,
			&p.Transfers, &p.FirstSeen, &p.LastSeen, &maxValue); err != nil {
			return nil, err
		}
		p.Balance = p.Received - p.Sent
		// Judged over this address's own legs, which is all this query sees. A
		// token whose every leg here carries no amount is reported as
		// non-fungible even if it is fungible elsewhere; the alternative is a
		// second query per token to prove a negative, for a flag whose only job
		// is to stop the page printing a meaningless zero as a balance.
		p.Fungible = maxValue > 0
		_, p.Symbol = TokenKeyParts(p.Token)
		out = append(out, p)
	}
	return out, rows.Err()
}

// HolderTransfers returns one page of an address's GRC20 transfers, newest
// first, in either direction and across every token.
//
// Paged rather than merely capped. It used to take a bare limit of 500 applied
// across every token a realm holds, with no offset and no count beside it, so a
// realm sitting exactly on the cap was indistinguishable from one whose history
// happens to be 500 long: r/gnoswap/pool and r/gnoswap/router both do
// (2026-09-23). The native half of the same page already reports
// shown/total/offset, and a reader has no way to know the two halves mean
// different things by a full table.
func (d *DB) HolderTransfers(network, addr string, limit, offset int) ([]TokenTransfer, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	out := []TokenTransfer{}
	if addr == "" || limit <= 0 {
		return out, nil
	}
	rows, err := d.db.Query(`
		SELECT token, pkg_path, from_addr, to_addr, value, tx_hash, block_height, block_time
		  FROM token_transfers
		 WHERE network = ?2 AND (from_addr = ?1 OR to_addr = ?1)
		 ORDER BY block_height DESC, tx_hash DESC, event_idx DESC
		 LIMIT ?3 OFFSET ?4`, addr, network, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var t TokenTransfer
		if err := rows.Scan(&t.Token, &t.PkgPath, &t.From, &t.To, &t.Value,
			&t.TxHash, &t.BlockHeight, &t.BlockTime); err != nil {
			return nil, err
		}
		t.Network = network
		out = append(out, t)
	}
	return out, rows.Err()
}

// HolderTransferCount is how many rows HolderTransfers is paging through.
//
// Counted in SQL over the whole set, never inferred from a short page: a page
// shorter than the limit does mean the end, but a *full* one says nothing, and
// "500 of 500" is exactly the case this exists to disambiguate.
func (d *DB) HolderTransferCount(network, addr string) (int, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if addr == "" {
		return 0, nil
	}
	var n int
	err := d.db.QueryRow(`
		SELECT COUNT(*) FROM token_transfers
		 WHERE network = ?2 AND (from_addr = ?1 OR to_addr = ?1)`, addr, network).Scan(&n)
	return n, err
}

// EarliestTokenTransfer is the oldest GRC20 transfer stored for a chain, or ""
// when the ledger is empty.
//
// It is the honest bound on every position above. The ledger was added after
// these chains were already running and is filled by the sync walk, so a token a
// package received before this timestamp left no row: a position is a floor
// whenever this is later than the package's deploy, and the page needs the
// timestamp to say so rather than presenting a floor as a figure.
func (d *DB) EarliestTokenTransfer(network string) string {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var t string
	// Scanning into a string rather than a sql.NullString: MIN over no rows
	// yields NULL, and the error that produces is the same "no ledger yet"
	// answer as an empty table, so both collapse to "".
	if err := d.db.QueryRow(
		`SELECT MIN(block_time) FROM token_transfers WHERE network = ?`, network).Scan(&t); err != nil {
		return ""
	}
	return t
}

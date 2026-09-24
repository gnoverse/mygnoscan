package store

import (
	"database/sql"
	"fmt"
	"strings"
)

// A package's coin history, read from the local ledger rather than derived from
// the indexer on every request.
//
// Every figure here used to be computed in Go from a fresh walk of the whole
// chain's transfer history, per request: ~2.1 seconds even for a realm holding
// nothing, because the cost was resolving a chain-wide event filter rather than
// anything about the answer (ADR 0043). The arithmetic is unchanged and
// deliberately so, down to which end of a leg wins when both are the package's:
// the acceptance test for this swap is that the same realm reports the same
// numbers, so a "better" rule here would look exactly like a regression.
//
// A package owns two accounts and they are not interchangeable. The banker is
// the realm's own money; the storage deposit is locked against its bytes. Legs
// touching either are listed, because a deposit funding is a real movement a
// reader wants to see, but only the banker's count toward the reconstructed
// balance: summing both and comparing against one account's bank/balances would
// report a gap that is an artefact of the query.

// RealmCoinFlow is one leg, already attributed to one of the two accounts.
type RealmCoinFlow struct {
	TxHash      string `json:"tx_hash"`
	BlockHeight int    `json:"block_height"`
	BlockTime   string `json:"block_time,omitempty"`
	// Account is "banker" or "storage deposit".
	Account string `json:"account"`
	// Counterparty is the other end. Empty is possible and means the chain
	// itself (a fee collector, a genesis allocation), not a missing value.
	Counterparty string `json:"counterparty"`
	// Amount is signed ugnot from the *package's* point of view: positive is
	// received. Coins is the chain's own string, kept because a transfer can
	// carry a denom that is not ugnot and Amount cannot represent it.
	Amount int64  `json:"amount"`
	Coins  string `json:"coins"`
}

// RealmCoinParty is one counterparty's whole relationship with the package.
type RealmCoinParty struct {
	Address string `json:"address"`
	// Sent is what this account put into the realm, Received what the realm
	// paid it. Kept apart rather than netted: an account that moved 400 GNOT
	// each way is not the same actor as one that never moved any.
	Sent     int64 `json:"sent"`
	Received int64 `json:"received"`
	// Net is Received minus Sent, this account's own profit and loss. The sign
	// is the opposite of RealmCoinFlow.Amount's, which is written from the
	// realm's point of view.
	Net       int64  `json:"net"`
	Legs      int    `json:"legs"`
	FirstSeen string `json:"first_seen,omitempty"`
	LastSeen  string `json:"last_seen,omitempty"`
}

// accountPredicate builds the "touches one of this package's accounts" filter,
// plus the CASE arms that attribute a leg to one of them.
//
// ⚠️ The deposit address is spliced in only when it is non-empty, and that is
// not tidiness. `from_addr` and `to_addr` default to ” for the chain's own end
// of a leg (genesis, a fee collector), so an `IN (addr, ”)` written for a
// package with no deposit account matches **every mint and every genesis
// allocation on the chain** and attributes them to that package. A realm with
// one leg would report thousands.
func accountPredicate(addr, depositAddr string) (where string, args []any) {
	if depositAddr == "" {
		return `(from_addr = ? OR to_addr = ?)`, []any{addr, addr}
	}
	return `(from_addr IN (?, ?) OR to_addr IN (?, ?))`,
		[]any{addr, depositAddr, addr, depositAddr}
}

// flowColumns are the CASE arms, in the order the Go version's switch tried
// them: the banker wins a leg that touches both accounts.
func flowColumns(depositAddr string) string {
	if depositAddr == "" {
		return `
			'banker' AS account,
			CASE WHEN to_addr = :addr THEN from_addr ELSE to_addr END AS counterparty,
			CASE WHEN to_addr = :addr THEN ugnot ELSE -ugnot END AS amount`
	}
	return `
			CASE WHEN to_addr = :addr OR from_addr = :addr
			     THEN 'banker' ELSE 'storage deposit' END AS account,
			CASE WHEN to_addr = :addr   THEN from_addr
			     WHEN from_addr = :addr THEN to_addr
			     WHEN to_addr = :dep    THEN from_addr
			     ELSE to_addr END AS counterparty,
			CASE WHEN to_addr = :addr   THEN ugnot
			     WHEN from_addr = :addr THEN -ugnot
			     WHEN to_addr = :dep    THEN ugnot
			     ELSE -ugnot END AS amount`
}

// bindFlowColumns substitutes the two named placeholders, which exist only so
// the CASE arms above read as arithmetic rather than as a column of question
// marks. Both values are addresses this package derived itself, never input.
func bindFlowColumns(sqlText, addr, depositAddr string) string {
	sqlText = strings.ReplaceAll(sqlText, ":addr", quoteLiteral(addr))
	return strings.ReplaceAll(sqlText, ":dep", quoteLiteral(depositAddr))
}

func quoteLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// RealmCoinStats is the whole history in aggregate: what the table below is a
// page of, and the figure held up against the chain's own balance.
type RealmCoinStats struct {
	// Legs counts every leg touching either account. DerivedUgnot sums only the
	// banker's, because that is the account bank/balances is read for.
	Legs         int
	DerivedUgnot int64
}

// RealmCoinStatsFor aggregates in SQL over every row, never over a page.
//
// A paged sum would report a gap against the live balance that is an artefact
// of the page size, which is the one mistake this whole feature exists to avoid
// making visible to a reader.
func (d *DB) RealmCoinStatsFor(network, addr, depositAddr string) (RealmCoinStats, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var s RealmCoinStats
	if addr == "" {
		return s, nil
	}
	where, args := accountPredicate(addr, depositAddr)
	q := fmt.Sprintf(`
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN to_addr   = %[1]s THEN ugnot
		                         WHEN from_addr = %[1]s THEN -ugnot
		                         ELSE 0 END), 0)
		  FROM coin_transfers
		 WHERE network = ? AND %[2]s`, quoteLiteral(addr), where)
	err := d.db.QueryRow(q, append([]any{network}, args...)...).Scan(&s.Legs, &s.DerivedUgnot)
	if err != nil && err != sql.ErrNoRows {
		return RealmCoinStats{}, err
	}
	return s, nil
}

// RealmCoinFlowsFor returns one page of the package's legs, newest first.
func (d *DB) RealmCoinFlowsFor(network, addr, depositAddr string, limit, offset int) ([]RealmCoinFlow, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	out := []RealmCoinFlow{}
	if addr == "" || limit <= 0 {
		return out, nil
	}
	where, args := accountPredicate(addr, depositAddr)
	q := bindFlowColumns(fmt.Sprintf(`
		SELECT tx_hash, block_height, block_time, coins, %s
		  FROM coin_transfers
		 WHERE network = ? AND %s
		 ORDER BY block_height DESC, tx_hash DESC, event_idx DESC
		 LIMIT ? OFFSET ?`, flowColumns(depositAddr), where), addr, depositAddr)

	args = append([]any{network}, args...)
	args = append(args, limit, offset)
	rows, err := d.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var f RealmCoinFlow
		var blockTime sql.NullString
		if err := rows.Scan(&f.TxHash, &f.BlockHeight, &blockTime, &f.Coins,
			&f.Account, &f.Counterparty, &f.Amount); err != nil {
			return nil, err
		}
		f.BlockTime = blockTime.String
		out = append(out, f)
	}
	return out, rows.Err()
}

// RealmCoinPartiesFor collapses the banker's history by who was at the other
// end, heaviest first, and reports how many there were before the cut.
//
// Ranked by gross rather than by net: an account that moved a lot in both
// directions is a major counterparty even when it comes out level, and ranking
// by net buries it under someone who moved a thousandth as much one way.
//
// The storage deposit account is excluded, matching the flows table's own rule
// for what counts as the realm's money.
func (d *DB) RealmCoinPartiesFor(network, addr, depositAddr string, limit int) ([]RealmCoinParty, int, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	out := []RealmCoinParty{}
	if addr == "" {
		return out, 0, nil
	}
	// Only the banker's legs, so the predicate is the single-address one
	// whatever the deposit account is.
	inner := bindFlowColumns(`
		SELECT CASE WHEN to_addr = :addr THEN from_addr ELSE to_addr END AS cp,
		       CASE WHEN to_addr = :addr THEN ugnot ELSE -ugnot END AS amt,
		       NULLIF(block_time, '') AS bt
		  FROM coin_transfers
		 WHERE network = ? AND (from_addr = ? OR to_addr = ?)`, addr, depositAddr)

	var total int
	if err := d.db.QueryRow(
		`SELECT COUNT(*) FROM (SELECT DISTINCT cp FROM (`+inner+`))`,
		network, addr, addr,
	).Scan(&total); err != nil && err != sql.ErrNoRows {
		return nil, 0, err
	}

	rows, err := d.db.Query(`
		SELECT cp,
		       COALESCE(SUM(CASE WHEN amt >= 0 THEN amt ELSE 0 END), 0) AS sent,
		       COALESCE(SUM(CASE WHEN amt <  0 THEN -amt ELSE 0 END), 0) AS received,
		       COUNT(*), MIN(bt), MAX(bt)
		  FROM (`+inner+`)
		 GROUP BY cp
		 ORDER BY (sent + received) DESC, cp ASC
		 LIMIT ?`, network, addr, addr, limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var p RealmCoinParty
		var first, last sql.NullString
		if err := rows.Scan(&p.Address, &p.Sent, &p.Received, &p.Legs, &first, &last); err != nil {
			return nil, 0, err
		}
		p.Net = p.Received - p.Sent
		p.FirstSeen, p.LastSeen = first.String, last.String
		out = append(out, p)
	}
	return out, total, rows.Err()
}

// CoinBackfillDone reports whether this network's historical walk has finished.
//
// The honest successor to the old `truncated` flag. That one meant "the indexer
// capped the query"; this one means "the ledger does not hold the whole history
// yet", which is the same thing to a reader: it is exactly when the
// reconstructed total may be short of what the chain reports, and the page has
// to be able to say so rather than presenting a partial sum as a reconciliation.
func (d *DB) CoinBackfillDone(network string) bool {
	v, err := d.GetSyncState(CoinBackfillDoneKey(network))
	return err == nil && v == "1"
}

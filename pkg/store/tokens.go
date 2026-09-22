package store

import (
	"strconv"
	"strings"
	"time"
)

// GRC20 assets, reconstructed from the Transfer events the chain emits.
//
// /api/tokens used to be a package list with a `grc20` filter: path, name,
// creator, call count, and nothing about the asset itself. It also over-matched,
// selecting anything that *imports* grc20 rather than anything that *is* a
// token, so a DEX router sat in the list beside the tokens it calls.
//
// The events are a complete ledger and were already flowing through the sync
// walk unstored. Replaying them gives exact supply and every holder's balance
// with no extra RPC call: an empty `from` is a mint, an empty `to` is a burn.
//
// Two traps live in the event shape and both are handled at insert:
//
//   - `pkg_path` on a GRC20 event is `gno.land/p/nt/grc20/v0`, the library, for
//     every token on the chain. Grouping by it produces one giant "token"
//     holding every asset. The token is in the `token` attribute.
//   - That attribute is usually `<realm path>.<name>.<id>`, not a bare path,
//     because one realm can expose several tokens. Usually, not always: two
//     live mainnet tokens put a bare symbol there instead (measured
//     2026-09-20), so the key is stored verbatim and only split when it has
//     the shape.

// TokenTransfer is one Transfer event.
type TokenTransfer struct {
	Token       string `json:"token"`
	PkgPath     string `json:"pkg_path"`
	From        string `json:"from"`
	To          string `json:"to"`
	Value       int64  `json:"value"`
	TxHash      string `json:"tx_hash"`
	BlockHeight int    `json:"block_height"`
	BlockTime   string `json:"block_time"`
	Network     string `json:"network,omitempty"`
}

// TokenSummary is one row of the assets list.
type TokenSummary struct {
	Token   string `json:"token"`
	PkgPath string `json:"pkg_path"`
	// Symbol is the token's own name segment from the event key. The registry
	// may override it with a display symbol, and says so when it does.
	Symbol  string `json:"symbol"`
	Network string `json:"network"`
	// Supply is mints minus burns, exact rather than sampled.
	Supply  int64 `json:"supply"`
	Holders int   `json:"holders"`
	// Fungible says whether supply and holders mean anything for this asset.
	//
	// GRC721 emits Transfer events through the same shape, and its transfers
	// carry no amount: gnoswap's GNFT has 201 transfers and every one of them
	// parses to a value of 0. Summing those produces a supply of 0 and a holder
	// count of 0, which is not "this token is empty", it is "this arithmetic
	// does not apply". Detected from the data rather than from the library
	// path, so a token that starts carrying amounts starts counting.
	Fungible      bool   `json:"fungible"`
	Transfers     int    `json:"transfers"`
	Transfers24h  int    `json:"transfers_24h"`
	FirstSeenTime string `json:"first_seen_time,omitempty"`
	LastSeenTime  string `json:"last_seen_time,omitempty"`
	// Minted and Burned are the two halves Supply is the difference of, kept
	// apart because the difference alone cannot tell "never minted much" from
	// "minted a lot and burned nearly all of it", and the per-token page is
	// where that distinction is the answer rather than a detail.
	Minted     int64 `json:"minted"`
	Burned     int64 `json:"burned"`
	MintCount  int   `json:"mint_count"`
	BurnCount  int   `json:"burn_count"`
	FirstBlock int   `json:"first_block"`
	LastBlock  int   `json:"last_block"`
}

// TokenHolder is one balance, reconstructed.
type TokenHolder struct {
	Address string `json:"address"`
	Balance int64  `json:"balance"`
}

// TokenKeyParts splits the event key into its realm path and token name.
//
// The key is usually `<path>.<name>.<id>`, and the path itself contains dots
// ("gno.land/..."), so this splits from the right rather than the left.
func TokenKeyParts(key string) (pkgPath, name string) {
	// Not every token emits the triple. Measured on mainnet 2026-09-20, two
	// live tokens (COVID, META) put a bare symbol in the attribute instead. For
	// those the symbol is all that is known, and inventing a realm path from it
	// would put "COVID" in a column headed "realm".
	parts := strings.Split(key, ".")
	if len(parts) < 3 || !strings.Contains(key, "/") {
		return "", key
	}
	return strings.Join(parts[:len(parts)-2], "."), parts[len(parts)-2]
}

// ParseTokenValue reads a Transfer event's value attribute.
func ParseTokenValue(s string) int64 {
	v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// InsertTokenTransfer stores one Transfer event, idempotently.
//
// Keyed on (network, tx_hash, event_idx) and the index is over *all* events in
// the transaction, not over Transfer events only, for the same reason
// storage_events does it: the number has to stay stable across passes, and
// filtering first would renumber rows whenever the event list changed shape.
func (d *DB) InsertTokenTransfer(network, txHash string, eventIdx int, t TokenTransfer) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	pkgPath, _ := TokenKeyParts(t.Token)
	_, err := d.db.Exec(`
		INSERT OR IGNORE INTO token_transfers
			(network, tx_hash, event_idx, token, pkg_path, from_addr, to_addr, value, block_height, block_time)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		network, txHash, eventIdx, t.Token, pkgPath, t.From, t.To, t.Value, t.BlockHeight, t.BlockTime)
	return err
}

// TokenSummaries lists every asset seen on a network.
//
// Supply is mints minus burns. Holders is counted from the reconstructed
// balances rather than from distinct recipients: an address that received and
// then sent everything on is not a holder, and counting it would overstate
// every token on the chain.
func (d *DB) TokenSummaries(network string) ([]TokenSummary, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	dayAgo := time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339)
	f := d.networkFilter("network", network)

	rows, err := d.db.Query(`
		SELECT token, pkg_path, network,
		       COALESCE(SUM(CASE WHEN from_addr = '' THEN value ELSE 0 END), 0) AS minted,
		       COALESCE(SUM(CASE WHEN to_addr = '' THEN value ELSE 0 END), 0) AS burned,
		       COUNT(*) AS transfers,
		       COALESCE(SUM(CASE WHEN block_time >= ? THEN 1 ELSE 0 END), 0) AS transfers_24h,
		       COALESCE(MAX(CASE WHEN value > 0 THEN 1 ELSE 0 END), 0) AS fungible,
		       COALESCE(SUM(CASE WHEN from_addr = '' THEN 1 ELSE 0 END), 0) AS mint_count,
		       COALESCE(SUM(CASE WHEN to_addr = '' THEN 1 ELSE 0 END), 0) AS burn_count,
		       MIN(block_height), MAX(block_height),
		       MIN(block_time), MAX(block_time)
		  FROM token_transfers
		 WHERE `+f+`
		 GROUP BY network, token
		 ORDER BY transfers DESC`, dayAgo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []TokenSummary{}
	for rows.Next() {
		var t TokenSummary
		var fungible int
		if err := rows.Scan(&t.Token, &t.PkgPath, &t.Network, &t.Minted, &t.Burned,
			&t.Transfers, &t.Transfers24h, &fungible, &t.MintCount, &t.BurnCount,
			&t.FirstBlock, &t.LastBlock, &t.FirstSeenTime, &t.LastSeenTime); err != nil {
			return nil, err
		}
		t.Supply = t.Minted - t.Burned
		t.Fungible = fungible == 1
		_, t.Symbol = TokenKeyParts(t.Token)
		if !t.Fungible {
			// Zero would read as a balance. It is the absence of one.
			t.Supply = 0
			t.Minted, t.Burned = 0, 0
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Holder counts in one pass rather than a correlated subquery per token:
	// the balance reconstruction is the expensive half and doing it once for
	// every token is cheaper than once per row of the list.
	holders, err := d.holderCounts(network, dayAgo)
	if err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Fungible {
			out[i].Holders = holders[out[i].Network+"\x00"+out[i].Token]
		}
	}
	return out, nil
}

// holderCounts counts addresses with a positive reconstructed balance, per
// token.
func (d *DB) holderCounts(network, _ string) (map[string]int, error) {
	rows, err := d.db.Query(`
		SELECT network, token, COUNT(*) FROM (
			SELECT network, token, addr, SUM(delta) AS bal FROM (
				SELECT network, token, to_addr AS addr, value AS delta FROM token_transfers
				 WHERE ` + d.networkFilter("network", network) + ` AND to_addr <> ''
				UNION ALL
				SELECT network, token, from_addr AS addr, -value AS delta FROM token_transfers
				 WHERE ` + d.networkFilter("network", network) + ` AND from_addr <> ''
			)
			GROUP BY network, token, addr
			HAVING bal > 0
		)
		GROUP BY network, token`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var net, token string
		var n int
		if err := rows.Scan(&net, &token, &n); err != nil {
			return nil, err
		}
		out[net+"\x00"+token] = n
	}
	return out, rows.Err()
}

// TopHolders reconstructs the largest balances for one token.
func (d *DB) TopHolders(network, token string, limit int) ([]TokenHolder, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	rows, err := d.db.Query(`
		SELECT addr, SUM(delta) AS bal FROM (
			SELECT to_addr AS addr, value AS delta FROM token_transfers
			 WHERE network = ? AND token = ? AND to_addr <> ''
			UNION ALL
			SELECT from_addr AS addr, -value AS delta FROM token_transfers
			 WHERE network = ? AND token = ? AND from_addr <> ''
		)
		GROUP BY addr HAVING bal > 0
		ORDER BY bal DESC LIMIT ?`, network, token, network, token, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []TokenHolder{}
	for rows.Next() {
		var h TokenHolder
		if err := rows.Scan(&h.Address, &h.Balance); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// TokenTransfers returns one token's most recent transfers.
func (d *DB) TokenTransfers(network, token string, limit int) ([]TokenTransfer, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	rows, err := d.db.Query(`
		SELECT token, pkg_path, from_addr, to_addr, value, tx_hash, block_height, block_time
		  FROM token_transfers
		 WHERE network = ? AND token = ?
		 ORDER BY block_height DESC LIMIT ?`, network, token, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []TokenTransfer{}
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

// TokenSupplyOverTime returns the running supply, one point per day.
//
// Cumulative from the first mint rather than from the start of the window, so a
// chart of the last 30 days still starts at the real supply instead of at zero.
func (d *DB) TokenSupplyOverTime(network, token string, days int) ([]BlockTimePoint, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	rows, err := d.db.Query(`
		SELECT substr(block_time, 1, 10) AS day,
		       SUM(CASE WHEN from_addr = '' THEN value ELSE 0 END)
		         - SUM(CASE WHEN to_addr = '' THEN value ELSE 0 END) AS minted
		  FROM token_transfers
		 WHERE network = ? AND token = ?
		 GROUP BY day ORDER BY day ASC`, network, token)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type dayPoint struct {
		day    string
		minted int64
	}
	var all []dayPoint
	for rows.Next() {
		var p dayPoint
		if err := rows.Scan(&p.day, &p.minted); err != nil {
			return nil, err
		}
		all = append(all, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	cutoff := ""
	if days > 0 {
		cutoff = time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")
	}
	out := []BlockTimePoint{}
	var running int64
	for _, p := range all {
		running += p.minted
		if p.day >= cutoff {
			out = append(out, BlockTimePoint{Time: p.day, Blocks: int(running)})
		}
	}
	return out, nil
}

// TokenLedgerWindow reports the span of history the transfer ledger actually
// covers on a network.
//
// The ledger is filled by the sync walk and sync resumes from the highest
// stored height, so a database that existed before the feature shipped starts
// its transfer history mid-chain rather than at genesis. Every figure
// reconstructed from it is then a figure over a window, and the difference
// shows plainly: a token whose burns predate the window reports a *negative*
// supply, which is not a rendering fault but the window arriving as arithmetic.
//
// Returning the window lets a page say so instead of printing the number as a
// total. A token whose own first transfer sits at the ledger floor is the case
// to warn about: its history most likely starts earlier than anything stored.
type TokenLedgerWindow struct {
	FirstBlock int    `json:"first_block"`
	LastBlock  int    `json:"last_block"`
	FirstTime  string `json:"first_time,omitempty"`
	LastTime   string `json:"last_time,omitempty"`
	Transfers  int    `json:"transfers"`
}

func (d *DB) TokenLedgerWindow(network string) (TokenLedgerWindow, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var w TokenLedgerWindow
	var firstBlock, lastBlock *int
	var firstTime, lastTime *string
	err := d.db.QueryRow(`
		SELECT MIN(block_height), MAX(block_height), MIN(block_time), MAX(block_time), COUNT(*)
		  FROM token_transfers WHERE `+d.networkFilter("network", network)).
		Scan(&firstBlock, &lastBlock, &firstTime, &lastTime, &w.Transfers)
	if err != nil {
		return w, err
	}
	// Every column but the count is NULL on an empty ledger, which is the
	// state every pre-existing deployment is in until its backfill runs.
	if firstBlock != nil {
		w.FirstBlock, w.LastBlock = *firstBlock, *lastBlock
	}
	if firstTime != nil {
		w.FirstTime, w.LastTime = *firstTime, *lastTime
	}
	return w, nil
}

// SearchTokens matches assets by their event key, for the search box.
//
// Deliberately not TokenSummaries with a filter: that reconstructs every
// balance on the chain, which is the right cost for the assets page and the
// wrong one for something that fires on a debounced keystroke. Search needs
// enough to render a row and route to the page; the page itself does the
// arithmetic.
func (d *DB) SearchTokens(network, q string, limit int) ([]TokenSummary, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if limit <= 0 {
		limit = 10
	}
	rows, err := d.db.Query(`
		SELECT token, pkg_path, network, COUNT(*) AS transfers
		  FROM token_transfers
		 WHERE `+d.networkFilter("network", network)+` AND token LIKE ?
		 GROUP BY network, token
		 ORDER BY transfers DESC LIMIT ?`, "%"+q+"%", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []TokenSummary{}
	for rows.Next() {
		var t TokenSummary
		if err := rows.Scan(&t.Token, &t.PkgPath, &t.Network, &t.Transfers); err != nil {
			return nil, err
		}
		_, t.Symbol = TokenKeyParts(t.Token)
		out = append(out, t)
	}
	return out, rows.Err()
}

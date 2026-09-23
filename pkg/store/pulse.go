package store

import (
	"sort"
	"strings"
)

// The chain's pulse: what moved inside a window, rather than what exists.
//
// Every figure the home page showed was an all-time total, which is the one
// question a returning reader never has. A chain that carried 13,568
// transactions yesterday and 3,277 today renders identically to a chain that
// has been asleep for a week, because 32,567 is 32,567 either way. Mass is not
// movement, and the page only ever showed mass.
//
// So each list here is scoped to a window and paired with the window before it,
// which is what makes a number readable without a chart: 3,804 calls is
// meaningless, 3,804 calls against 12 yesterday is a story. The comparison is
// always against an *equal-length* preceding window for that reason — comparing
// six hours to a week would invent a collapse that never happened.
//
// All of it reads local SQLite over the (network, block_time) indexes that
// already exist, so the cost is a range scan per list rather than a new
// pipeline. The one index this needed is on token_transfers, whose only
// time-ordered index led with token.

// PulseParams bounds one pulse read.
//
// Since and PrevSince are RFC3339 strings rather than time.Time because that is
// what block_time holds and what every other windowed query here binds. The
// caller owns the clock, which is what makes this testable without one.
type PulseParams struct {
	Network string
	// Since is the start of the window, inclusive. Required.
	Since string
	// PrevSince is the start of the equal-length window before it. The
	// preceding window is [PrevSince, Since). Empty skips the comparison.
	PrevSince string
	// Limit is the length of each ranked list.
	Limit int
}

// PulseCounts is one window's worth of activity. The same shape is returned for
// the current window and the one before it, so the frontend can subtract
// without knowing which fields are comparable.
type PulseCounts struct {
	Txs            int   `json:"txs"`
	Calls          int   `json:"calls"`
	FailedCalls    int   `json:"failed_calls"`
	Deploys        int   `json:"deploys"`
	MsgRuns        int   `json:"msg_runs"`
	Sends          int   `json:"sends"`
	NewRealms      int   `json:"new_realms"`
	NewPackages    int   `json:"new_packages"`
	ActiveAddrs    int   `json:"active_addresses"`
	TokenTransfers int   `json:"token_transfers"`
	UgnotSent      int64 `json:"ugnot_sent"`
	GasUsed        int64 `json:"gas_used"`
	Fees           int64 `json:"fees"`
	// StorageBytes and StorageFee are net of refunds, and both can be negative:
	// a window in which more was freed than stored is a real answer, not a bug.
	StorageBytes int64 `json:"storage_bytes"`
	StorageFee   int64 `json:"storage_fee"`
}

// HotRealm is one realm ranked by calls inside the window.
type HotRealm struct {
	Network   string `json:"network"`
	Path      string `json:"path"`
	Calls     int    `json:"calls"`
	Callers   int    `json:"callers"`
	Failed    int    `json:"failed"`
	PrevCalls int    `json:"prev_calls"`
	LastTime  string `json:"last_time,omitempty"`
	LastBlock int    `json:"last_block"`
}

// HotToken is one asset ranked by transfers inside the window.
//
// Native is the one row not drawn from token_transfers: ugnot moves through
// BankMsgSend, not through a GRC20 Transfer event, and leaving the chain's own
// coin out of a list headed "hot assets" would be the most misleading thing on
// the page. It is marked rather than hidden so a reader knows the two rows were
// counted from different ledgers.
type HotToken struct {
	Network   string `json:"network"`
	Token     string `json:"token"`
	Symbol    string `json:"symbol"`
	PkgPath   string `json:"pkg_path,omitempty"`
	Native    bool   `json:"native,omitempty"`
	Transfers int    `json:"transfers"`
	Volume    int64  `json:"volume"`
	Senders   int    `json:"senders"`
	Receivers int    `json:"receivers"`
	Mints     int    `json:"mints"`
	Burns     int    `json:"burns"`
	// Fungible is false for GRC721, whose Transfer events carry no amount.
	// Volume and the two sums above mean nothing for those, and are zeroed
	// rather than shown as zero balances. Same rule as TokenSummaries.
	Fungible bool `json:"fungible"`
	// PrevTransfers is the same count over the preceding equal window.
	PrevTransfers int `json:"prev_transfers"`
}

// HotFlow is one notable transfer, with both ends left as raw addresses. The
// API layer is what resolves an address to the realm that owns it, because that
// derivation lives in pkg/gnoaddr and the store has no business importing it.
type HotFlow struct {
	Network     string `json:"network"`
	TxHash      string `json:"tx_hash"`
	Token       string `json:"token"`
	Symbol      string `json:"symbol"`
	PkgPath     string `json:"pkg_path,omitempty"`
	Native      bool   `json:"native,omitempty"`
	From        string `json:"from"`
	To          string `json:"to"`
	Value       int64  `json:"value"`
	BlockHeight int    `json:"block_height"`
	BlockTime   string `json:"block_time,omitempty"`
}

// HotDev is one address ranked by packages deployed inside the window.
type HotDev struct {
	Network string `json:"network"`
	Address string `json:"address"`
	Deploys int    `json:"deploys"`
	// Paths is how many distinct package paths those deploys touched, which is
	// smaller than Deploys whenever a path was resubmitted. Under the inert
	// submission policy that is routine, not exceptional.
	Paths     int      `json:"paths"`
	Realms    int      `json:"realms"`
	Failed    int      `json:"failed"`
	Recent    []string `json:"recent"`
	FirstTime string   `json:"first_time,omitempty"`
	LastTime  string   `json:"last_time,omitempty"`
	// FirstEver is this address's earliest deploy on this chain, at any time.
	// Equal to FirstTime exactly when the window contains their first deploy,
	// which is what New reports.
	FirstEver string `json:"first_ever,omitempty"`
	New       bool   `json:"new"`
}

// HotLib is one package ranked by how many of the window's *new* packages
// import it.
//
// "New" means first submitted inside the window, not last: a realm redeployed
// today is not a new package, and counting it as one would make every retry
// look like adoption.
type HotLib struct {
	Network string `json:"network"`
	Path    string `json:"path"`
	// NewImporters is how many packages first deployed in the window import
	// this one. TotalImporters is the all-time figure, and the pair is the
	// point: 3 of 4 is a library being picked up, 3 of 300 is noise.
	NewImporters   int      `json:"new_importers"`
	TotalImporters int      `json:"total_importers"`
	Importers      []string `json:"importers"`
}

// Pulse is the whole answer, in one response, because the home page paints it
// in one pass and five endpoints would be five round trips for one screen.
type Pulse struct {
	Window     PulseCounts `json:"window"`
	Prev       PulseCounts `json:"prev"`
	HasPrev    bool        `json:"has_prev"`
	HotRealms  []HotRealm  `json:"hot_realms"`
	HotTokens  []HotToken  `json:"hot_tokens"`
	HotFlows   []HotFlow   `json:"hot_flows"`
	HotDevs    []HotDev    `json:"hot_devs"`
	HotLibs    []HotLib    `json:"hot_libs"`
	TokenPaths []string    `json:"-"`
}

// pulseRange builds the time predicate for one column. An empty until is an
// open-ended window ending now, which is the common case; the bounded form is
// only used for the comparison window.
func pulseRange(col, since, until string) (string, []any) {
	if until == "" {
		return " AND " + col + " >= ?", []any{since}
	}
	return " AND " + col + " >= ? AND " + col + " < ?", []any{since, until}
}

// GetPulse reads every windowed figure the home page draws.
func (d *DB) GetPulse(p PulseParams) (*Pulse, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	limit := p.Limit
	if limit <= 0 {
		limit = 10
	}
	nf := d.networkFilter("network", p.Network)

	out := &Pulse{
		HotRealms: []HotRealm{},
		HotTokens: []HotToken{},
		HotFlows:  []HotFlow{},
		HotDevs:   []HotDev{},
		HotLibs:   []HotLib{},
	}

	var err error
	if out.Window, err = d.pulseCounts(nf, p.Since, ""); err != nil {
		return nil, err
	}
	if p.PrevSince != "" {
		if out.Prev, err = d.pulseCounts(nf, p.PrevSince, p.Since); err != nil {
			return nil, err
		}
		out.HasPrev = true
	}
	if out.HotRealms, err = d.pulseHotRealms(nf, p.Since, p.PrevSince, limit); err != nil {
		return nil, err
	}
	if out.HotTokens, err = d.pulseHotTokens(nf, p.Since, p.PrevSince, limit); err != nil {
		return nil, err
	}
	if out.HotFlows, err = d.pulseHotFlows(nf, p.Since, limit); err != nil {
		return nil, err
	}
	if out.HotDevs, err = d.pulseHotDevs(nf, p.Since, limit); err != nil {
		return nil, err
	}
	if out.HotLibs, err = d.pulseHotLibs(nf, p.Since, limit); err != nil {
		return nil, err
	}
	return out, nil
}

func (d *DB) pulseCounts(nf, since, until string) (PulseCounts, error) {
	var c PulseCounts

	one := func(query, col string, dest ...any) error {
		clause, args := pulseRange(col, since, until)
		return d.db.QueryRow(query+" WHERE "+nf+clause, args...).Scan(dest...)
	}

	if err := one(`SELECT COUNT(*), COALESCE(SUM(CASE WHEN success THEN 0 ELSE 1 END), 0) FROM calls`,
		"block_time", &c.Calls, &c.FailedCalls); err != nil {
		return c, err
	}
	if err := one(`SELECT COUNT(*) FROM package_submissions`, "block_time", &c.Deploys); err != nil {
		return c, err
	}
	if err := one(`SELECT COUNT(*) FROM msg_runs`, "block_time", &c.MsgRuns); err != nil {
		return c, err
	}
	// ugnot_amount is nullable: a send carrying only a non-ugnot denom parses to
	// NULL rather than to zero, and SUM over a column of them is NULL too.
	if err := one(`SELECT COUNT(*), COALESCE(SUM(ugnot_amount), 0) FROM bank_sends`,
		"block_time", &c.Sends, &c.UgnotSent); err != nil {
		return c, err
	}
	if err := one(`SELECT COALESCE(SUM(gas_used), 0), COALESCE(SUM(gas_fee), 0) FROM transactions`,
		"block_time", &c.GasUsed, &c.Fees); err != nil {
		return c, err
	}
	if err := one(`SELECT COALESCE(SUM(bytes_delta), 0), COALESCE(SUM(fee), 0) FROM storage_events`,
		"block_time", &c.StorageBytes, &c.StorageFee); err != nil {
		return c, err
	}
	if err := one(`SELECT COUNT(*) FROM token_transfers`, "block_time", &c.TokenTransfers); err != nil {
		return c, err
	}
	c.Txs = c.Calls + c.Deploys + c.MsgRuns + c.Sends

	// A package is new when its *first* submission lands in the window. Reading
	// packages.block_time instead would have counted a redeploy as a new
	// package, and under the inert submission policy a stuck deploy is retried
	// until it lands — so the table that keeps every submission is the only one
	// that can answer this.
	newClause, newArgs := pulseRange("first_time", since, until)
	if err := d.db.QueryRow(`
		SELECT COALESCE(SUM(CASE WHEN is_realm THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN is_realm THEN 0 ELSE 1 END), 0)
		  FROM (SELECT network, path, MIN(block_time) AS first_time, MAX(is_realm) AS is_realm
		          FROM package_submissions WHERE `+nf+`
		         GROUP BY network, path)
		 WHERE 1=1`+newClause, newArgs...).Scan(&c.NewRealms, &c.NewPackages); err != nil {
		return c, err
	}

	// One address active on two chains is two actors, which is why network is
	// part of the UNION's tuple. Collapsing it undercounts, the same way the
	// all-time unique-caller figure did before it was scoped.
	actClause, actArgs := pulseRange("block_time", since, until)
	args := []any{}
	for range 4 {
		args = append(args, actArgs...)
	}
	if err := d.db.QueryRow(`
		SELECT COUNT(*) FROM (
			      SELECT network, caller       AS a FROM calls               WHERE `+nf+actClause+`
			UNION SELECT network, caller       AS a FROM msg_runs            WHERE `+nf+actClause+`
			UNION SELECT network, from_address AS a FROM bank_sends          WHERE `+nf+actClause+`
			UNION SELECT network, creator      AS a FROM package_submissions WHERE `+nf+actClause+`
		)`, args...).Scan(&c.ActiveAddrs); err != nil {
		return c, err
	}
	return c, nil
}

func (d *DB) pulseHotRealms(nf, since, prevSince string, limit int) ([]HotRealm, error) {
	rows, err := d.db.Query(`
		SELECT network, pkg_path, COUNT(*) AS calls, COUNT(DISTINCT caller),
		       COALESCE(SUM(CASE WHEN success THEN 0 ELSE 1 END), 0),
		       COALESCE(MAX(block_time), ''), COALESCE(MAX(block_height), 0)
		  FROM calls WHERE `+nf+` AND block_time >= ?
		 GROUP BY network, pkg_path
		 ORDER BY calls DESC, pkg_path ASC
		 LIMIT ?`, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []HotRealm{}
	for rows.Next() {
		var r HotRealm
		if err := rows.Scan(&r.Network, &r.Path, &r.Calls, &r.Callers, &r.Failed,
			&r.LastTime, &r.LastBlock); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if prevSince == "" || len(out) == 0 {
		return out, nil
	}

	// The previous window in one grouped pass rather than a subquery per row:
	// the whole preceding window is a few hundred rows on any chain here, and
	// the alternative is N correlated scans of the same range.
	prev, err := d.db.Query(`
		SELECT network, pkg_path, COUNT(*) FROM calls
		 WHERE `+nf+` AND block_time >= ? AND block_time < ?
		 GROUP BY network, pkg_path`, prevSince, since)
	if err != nil {
		return nil, err
	}
	defer prev.Close()
	counts := map[string]int{}
	for prev.Next() {
		var net, path string
		var n int
		if err := prev.Scan(&net, &path, &n); err != nil {
			return nil, err
		}
		counts[net+"\x00"+path] = n
	}
	if err := prev.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		out[i].PrevCalls = counts[out[i].Network+"\x00"+out[i].Path]
	}
	return out, nil
}

func (d *DB) pulseHotTokens(nf, since, prevSince string, limit int) ([]HotToken, error) {
	rows, err := d.db.Query(`
		SELECT network, token, pkg_path, COUNT(*) AS transfers,
		       COALESCE(SUM(value), 0),
		       COUNT(DISTINCT CASE WHEN from_addr <> '' THEN from_addr END),
		       COUNT(DISTINCT CASE WHEN to_addr   <> '' THEN to_addr   END),
		       COALESCE(SUM(CASE WHEN from_addr = '' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN to_addr   = '' THEN 1 ELSE 0 END), 0),
		       COALESCE(MAX(CASE WHEN value > 0 THEN 1 ELSE 0 END), 0)
		  FROM token_transfers WHERE `+nf+` AND block_time >= ?
		 GROUP BY network, token
		 ORDER BY transfers DESC, token ASC
		 LIMIT ?`, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []HotToken{}
	for rows.Next() {
		var t HotToken
		var fungible int
		if err := rows.Scan(&t.Network, &t.Token, &t.PkgPath, &t.Transfers, &t.Volume,
			&t.Senders, &t.Receivers, &t.Mints, &t.Burns, &fungible); err != nil {
			return nil, err
		}
		t.Fungible = fungible == 1
		if !t.Fungible {
			// Zero would read as "nothing moved". It is "this arithmetic does
			// not apply": a GRC721 transfer carries no amount at all.
			t.Volume = 0
		}
		_, t.Symbol = TokenKeyParts(t.Token)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// ugnot last, then re-sorted in: the chain's own coin does not pass through
	// a GRC20 Transfer event, so a list built only from token_transfers leaves
	// out the asset that moves most.
	native, err := d.pulseNativeToken(nf, since)
	if err != nil {
		return nil, err
	}
	if native.Transfers > 0 {
		out = append(out, native)
		for i := len(out) - 1; i > 0 && out[i].Transfers > out[i-1].Transfers; i-- {
			out[i], out[i-1] = out[i-1], out[i]
		}
		if len(out) > limit {
			out = out[:limit]
		}
	}

	if prevSince == "" || len(out) == 0 {
		return out, nil
	}
	prev, err := d.db.Query(`
		SELECT network, token, COUNT(*) FROM token_transfers
		 WHERE `+nf+` AND block_time >= ? AND block_time < ?
		 GROUP BY network, token`, prevSince, since)
	if err != nil {
		return nil, err
	}
	defer prev.Close()
	counts := map[string]int{}
	for prev.Next() {
		var net, tok string
		var n int
		if err := prev.Scan(&net, &tok, &n); err != nil {
			return nil, err
		}
		counts[net+"\x00"+tok] = n
	}
	if err := prev.Err(); err != nil {
		return nil, err
	}
	var prevNative int
	if err := d.db.QueryRow(`SELECT COUNT(*) FROM bank_sends
		 WHERE `+nf+` AND block_time >= ? AND block_time < ? AND success = 1`,
		prevSince, since).Scan(&prevNative); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Native {
			out[i].PrevTransfers = prevNative
			continue
		}
		out[i].PrevTransfers = counts[out[i].Network+"\x00"+out[i].Token]
	}
	return out, nil
}

// pulseNativeToken folds ugnot's bank sends into the shape a GRC20 row has.
//
// Network is left empty on purpose: bank_sends is grouped across whatever the
// filter allows, so one row cannot claim a chain. Every other field is a direct
// analogue, except mints and burns, which ugnot has none of here.
func (d *DB) pulseNativeToken(nf, since string) (HotToken, error) {
	t := HotToken{Token: "ugnot", Symbol: "ugnot", Native: true, Fungible: true}
	err := d.db.QueryRow(`
		SELECT COUNT(*), COALESCE(SUM(ugnot_amount), 0),
		       COUNT(DISTINCT from_address), COUNT(DISTINCT to_address)
		  FROM bank_sends WHERE `+nf+` AND block_time >= ? AND success = 1`, since).
		Scan(&t.Transfers, &t.Volume, &t.Senders, &t.Receivers)
	return t, err
}

func (d *DB) pulseHotFlows(nf, since string, limit int) ([]HotFlow, error) {
	// The largest transfer of each asset, rather than the largest overall: a
	// value is only comparable inside one denom, and sorting 4,000 GNOT against
	// 4,000 units of a token with eighteen decimals would rank by nothing at
	// all.
	//
	// So each asset is ranked on its own, and the merged list is filled round
	// robin: every asset's biggest, then every asset's second biggest, and so
	// on until the list is full. A fixed slice per asset cannot work, because
	// how many assets moved is not known before the query runs — two would
	// leave the panel nearly empty on a chain where only ugnot moved, and ten
	// would let one busy token fill the table on a chain where forty did.
	rows, err := d.db.Query(`
		SELECT network, token, pkg_path, from_addr, to_addr, value, tx_hash, block_height,
		       COALESCE(block_time, ''), rn
		  FROM (SELECT *, ROW_NUMBER() OVER (PARTITION BY network, token ORDER BY value DESC, tx_hash ASC) AS rn
		          FROM token_transfers
		         WHERE `+nf+` AND block_time >= ? AND value > 0)
		 WHERE rn <= ?
		 ORDER BY rn ASC, value DESC, tx_hash ASC`, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// byRank[i] is every asset's (i+1)-th largest transfer, which is exactly
	// the round-robin order the merge wants.
	byRank := make([][]HotFlow, limit)
	for rows.Next() {
		var f HotFlow
		var rank int
		if err := rows.Scan(&f.Network, &f.Token, &f.PkgPath, &f.From, &f.To, &f.Value,
			&f.TxHash, &f.BlockHeight, &f.BlockTime, &rank); err != nil {
			return nil, err
		}
		_, f.Symbol = TokenKeyParts(f.Token)
		if rank >= 1 && rank <= limit {
			byRank[rank-1] = append(byRank[rank-1], f)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// ugnot is the asset that moves most on this chain and emits no Transfer
	// event, so its ranks are read from bank_sends and folded into the same
	// ladder rather than appended after it.
	native, err := d.db.Query(`
		SELECT network, from_address, to_address, COALESCE(ugnot_amount, 0), tx_hash, block_height,
		       COALESCE(block_time, '')
		  FROM bank_sends
		 WHERE `+nf+` AND block_time >= ? AND success = 1 AND COALESCE(ugnot_amount, 0) > 0
		 ORDER BY ugnot_amount DESC, tx_hash ASC
		 LIMIT ?`, since, limit)
	if err != nil {
		return nil, err
	}
	defer native.Close()
	rank := 0
	for native.Next() {
		f := HotFlow{Token: "ugnot", Symbol: "ugnot", Native: true}
		if err := native.Scan(&f.Network, &f.From, &f.To, &f.Value, &f.TxHash,
			&f.BlockHeight, &f.BlockTime); err != nil {
			return nil, err
		}
		if rank < limit {
			byRank[rank] = append(byRank[rank], f)
		}
		rank++
	}
	if err := native.Err(); err != nil {
		return nil, err
	}

	out := []HotFlow{}
	for _, tier := range byRank {
		for _, f := range tier {
			if len(out) >= limit {
				break
			}
			out = append(out, f)
		}
	}
	// Newest first, once the set is chosen. Rank picked *what* is worth showing;
	// recency is how a reader wants to read it.
	sortFlowsNewestFirst(out)
	return out, nil
}

// sortFlowsNewestFirst orders by height, then by value, then by hash. The last
// two are not decoration: a block can carry several of these, and without a
// total order the list reshuffles between two identical requests, which makes a
// page that refreshes every thirty seconds jump for no reason.
func sortFlowsNewestFirst(f []HotFlow) {
	sort.Slice(f, func(i, j int) bool {
		a, b := f[i], f[j]
		if a.BlockHeight != b.BlockHeight {
			return a.BlockHeight > b.BlockHeight
		}
		if a.Value != b.Value {
			return a.Value > b.Value
		}
		return a.TxHash < b.TxHash
	})
}

func (d *DB) pulseHotDevs(nf, since string, limit int) ([]HotDev, error) {
	rows, err := d.db.Query(`
		SELECT network, creator, COUNT(*) AS deploys, COUNT(DISTINCT path),
		       COUNT(DISTINCT CASE WHEN is_realm THEN path END),
		       COALESCE(SUM(CASE WHEN success THEN 0 ELSE 1 END), 0),
		       COALESCE(MIN(block_time), ''), COALESCE(MAX(block_time), '')
		  FROM package_submissions WHERE `+nf+` AND block_time >= ?
		 GROUP BY network, creator
		 ORDER BY deploys DESC, creator ASC
		 LIMIT ?`, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []HotDev{}
	index := map[string]int{}
	for rows.Next() {
		var v HotDev
		if err := rows.Scan(&v.Network, &v.Address, &v.Deploys, &v.Paths, &v.Realms,
			&v.Failed, &v.FirstTime, &v.LastTime); err != nil {
			return nil, err
		}
		v.Recent = []string{}
		index[v.Network+"\x00"+v.Address] = len(out)
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}

	// What they actually shipped. A deployer row without paths is a number
	// nobody can act on; with them it is a link to the thing that was built.
	paths, err := d.db.Query(`
		SELECT network, creator, path, MAX(block_time) AS t
		  FROM package_submissions WHERE `+nf+` AND block_time >= ?
		 GROUP BY network, creator, path
		 ORDER BY t DESC`, since)
	if err != nil {
		return nil, err
	}
	defer paths.Close()
	for paths.Next() {
		var net, creator, path, t string
		if err := paths.Scan(&net, &creator, &path, &t); err != nil {
			return nil, err
		}
		i, ok := index[net+"\x00"+creator]
		if !ok || len(out[i].Recent) >= pulseRecentPaths {
			continue
		}
		out[i].Recent = append(out[i].Recent, path)
	}
	if err := paths.Err(); err != nil {
		return nil, err
	}

	// First-time deployers are the single most interesting row in this list, so
	// the earliest deploy per address is read over all of history rather than
	// over the window. package_submissions is the smallest table here (448 rows
	// on mainnet, 2026-09-23), so one grouped pass is cheaper than a correlated
	// lookup per row.
	first, err := d.db.Query(`
		SELECT network, creator, MIN(block_time) FROM package_submissions
		 WHERE ` + nf + ` GROUP BY network, creator`)
	if err != nil {
		return nil, err
	}
	defer first.Close()
	for first.Next() {
		var net, creator string
		var t *string
		if err := first.Scan(&net, &creator, &t); err != nil {
			return nil, err
		}
		i, ok := index[net+"\x00"+creator]
		if !ok || t == nil {
			continue
		}
		out[i].FirstEver = *t
		out[i].New = *t >= since
	}
	return out, first.Err()
}

// pulseRecentPaths bounds how many package paths a deployer row names. Enough to
// see what they are building, short enough that one busy deployer cannot own the
// table.
const pulseRecentPaths = 4

func (d *DB) pulseHotLibs(nf, since string, limit int) ([]HotLib, error) {
	// Imports declared by packages whose *first* submission is inside the
	// window. dependencies holds the current state of each path, so the join is
	// "what the new packages import today", which is the question; a redeploy
	// that only changed a comment is excluded because the path is not new.
	//
	// Standard library imports are filtered out by requiring a slash. Without
	// it `std`, `strings` and `testing` take the top three places on every
	// chain on every window, and a list whose answer never changes tells a
	// reader nothing about what is being adopted.
	rows, err := d.db.Query(`
		SELECT d.network, d.import_path, COUNT(DISTINCT d.package_path) AS n
		  FROM dependencies d
		  JOIN (SELECT network, path, MIN(block_time) AS first_time
		          FROM package_submissions WHERE `+nf+`
		         GROUP BY network, path) p
		    ON p.network = d.network AND p.path = d.package_path
		 WHERE p.first_time >= ? AND d.import_path LIKE '%/%'
		 GROUP BY d.network, d.import_path
		 ORDER BY n DESC, d.import_path ASC
		 LIMIT ?`, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []HotLib{}
	index := map[string]int{}
	for rows.Next() {
		var l HotLib
		if err := rows.Scan(&l.Network, &l.Path, &l.NewImporters); err != nil {
			return nil, err
		}
		l.Importers = []string{}
		index[l.Network+"\x00"+l.Path] = len(out)
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}

	// Which new packages they are, and the all-time count to read the window's
	// figure against. Both are bound to the paths that survived the LIMIT, so
	// neither is a scan of the whole dependency table.
	holders := make([]string, 0, len(out))
	args := make([]any, 0, len(out)+1)
	for _, l := range out {
		holders = append(holders, "?")
		args = append(args, l.Path)
	}
	in := " AND import_path IN (" + strings.Join(holders, ",") + ")"

	totals, err := d.db.Query(`
		SELECT network, import_path, COUNT(DISTINCT package_path)
		  FROM dependencies WHERE `+nf+in+`
		 GROUP BY network, import_path`, args...)
	if err != nil {
		return nil, err
	}
	defer totals.Close()
	for totals.Next() {
		var net, path string
		var n int
		if err := totals.Scan(&net, &path, &n); err != nil {
			return nil, err
		}
		if i, ok := index[net+"\x00"+path]; ok {
			out[i].TotalImporters = n
		}
	}
	if err := totals.Err(); err != nil {
		return nil, err
	}

	args = append(args, since)
	importers, err := d.db.Query(`
		SELECT d.network, d.import_path, d.package_path, p.first_time
		  FROM dependencies d
		  JOIN (SELECT network, path, MIN(block_time) AS first_time
		          FROM package_submissions WHERE `+nf+`
		         GROUP BY network, path) p
		    ON p.network = d.network AND p.path = d.package_path
		 WHERE `+strings.TrimPrefix(in, " AND ")+` AND p.first_time >= ?
		 ORDER BY p.first_time DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer importers.Close()
	for importers.Next() {
		var net, imp, pkg, t string
		if err := importers.Scan(&net, &imp, &pkg, &t); err != nil {
			return nil, err
		}
		i, ok := index[net+"\x00"+imp]
		if !ok || len(out[i].Importers) >= pulseRecentPaths {
			continue
		}
		out[i].Importers = append(out[i].Importers, pkg)
	}
	return out, importers.Err()
}

// PackagePaths lists every package path known on a network.
//
// Its one caller derives each path's two accounts to resolve a transfer's ends
// (pkg/gnoaddr.Reverse). Reading paths rather than exposing the derivation here
// keeps the store free of the address format: this file knows about rows, and
// what a path hashes to is not one.
func (d *DB) PackagePaths(network string) ([]string, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	rows, err := d.db.Query(`SELECT DISTINCT path FROM packages WHERE ` +
		d.networkFilter("network", network))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []string{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

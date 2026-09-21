package store

import (
	"database/sql"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Cached account balances, and the account population.
//
// Balances are the one figure on this site that cannot be synced and cannot be
// derived. gno carries no balance in any indexed message, and computing one
// from bank_sends as received-minus-sent would be wrong in a way a reader could
// not see: it ignores gas fees, storage deposits, genesis allocations and every
// transfer a realm makes through a banker rather than a BankMsgSend. A rich
// list that is wrong at the top is worse than no rich list.
//
// So each row here is one live RPC read, swept in the background rather than on
// the read path. The read path used to fan out one request per row at 20 at a
// time, on every cold request, against a single node, which is the 7.8s that
// the perf pass measured and explicitly left unfixed.

// BalanceRow is one cached balance.
type BalanceRow struct {
	Address string `json:"address"`
	Network string `json:"network"`
	// Amount is the coin string the chain returned, kept verbatim. Ugnot is it
	// parsed, for ranking. Both, because a rich list has to sort on a number
	// and a page has to show what the chain actually said.
	Amount    string `json:"amount"`
	Ugnot     int64  `json:"ugnot"`
	Height    int    `json:"height"`
	FetchedAt string `json:"fetched_at"`
}

// ugnotAmount pulls the ugnot component out of a coin string.
//
// The chain answers with a comma-separated coin list ("100ugnot,5foo"), and
// only the native denom is rankable: two chains' non-native coins are not
// comparable and summing across denominations would invent a number.
var ugnotAmount = regexp.MustCompile(`(\d+)ugnot`)

func ParseUgnot(amount string) int64 {
	m := ugnotAmount.FindStringSubmatch(amount)
	if m == nil {
		return 0
	}
	v, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// UpsertBalances writes a sweep's results in one transaction.
//
// One transaction for the batch rather than per row: the sweeper holds the
// write lock once instead of thousands of times, the same reason the rollups
// are replaced wholesale.
func (d *DB) UpsertBalances(network string, rows []BalanceRow) error {
	if len(rows) == 0 {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		INSERT INTO balances (network, address, amount, ugnot, height, fetched_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(network, address) DO UPDATE SET
			amount = excluded.amount, ugnot = excluded.ugnot,
			height = excluded.height, fetched_at = excluded.fetched_at`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	now := time.Now().UTC().Format(time.RFC3339)
	for _, r := range rows {
		if _, err := stmt.Exec(network, r.Address, r.Amount, ParseUgnot(r.Amount), r.Height, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// BalancesFor returns the cached balances for a set of addresses on one
// network, keyed by address. Missing addresses are simply absent: the caller
// renders those as unknown rather than as zero, which is a different claim.
func (d *DB) BalancesFor(network string, addresses []string) (map[string]BalanceRow, error) {
	out := map[string]BalanceRow{}
	if len(addresses) == 0 {
		return out, nil
	}
	d.mu.RLock()
	defer d.mu.RUnlock()

	args := make([]any, 0, len(addresses)+1)
	args = append(args, network)
	q := `SELECT address, amount, ugnot, height, fetched_at FROM balances
	       WHERE network = ? AND address IN (` + strings.TrimSuffix(strings.Repeat("?,", len(addresses)), ",") + `)`
	for _, a := range addresses {
		args = append(args, a)
	}
	rows, err := d.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		r := BalanceRow{Network: network}
		if err := rows.Scan(&r.Address, &r.Amount, &r.Ugnot, &r.Height, &r.FetchedAt); err != nil {
			return nil, err
		}
		out[r.Address] = r
	}
	return out, rows.Err()
}

// RichList returns the highest balances on one network.
//
// Single network only, and the caller has to say which. A balance is
// denominated per chain, so a merged ranking would put two chains' ugnot in one
// column and the total would belong to neither.
func (d *DB) RichList(network string, limit, offset int) ([]BalanceRow, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	rows, err := d.db.Query(
		`SELECT address, amount, ugnot, height, fetched_at FROM balances
		  WHERE network = ? AND ugnot > 0
		  ORDER BY ugnot DESC LIMIT ? OFFSET ?`, network, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []BalanceRow{}
	for rows.Next() {
		r := BalanceRow{Network: network}
		if err := rows.Scan(&r.Address, &r.Amount, &r.Ugnot, &r.Height, &r.FetchedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// BalanceCoverage says how much of the ranking is actually ranked.
//
// The rich list can only rank what has been swept, and the page has to print
// that boundary rather than implying it ranked everyone. Mintscan can claim to
// rank a whole chain; mygnoscan cannot, because gno offers no way to enumerate
// the auth module.
type BalanceCoverage struct {
	// Swept is how many addresses have a cached balance, Known how many this
	// instance has ever seen on chain.
	Swept int `json:"swept"`
	Known int `json:"known"`
	// OldestFetch and NewestFetch bound the age of the figures.
	OldestFetch string `json:"oldest_fetch,omitempty"`
	NewestFetch string `json:"newest_fetch,omitempty"`
}

func (d *DB) BalanceCoverage(network string) (BalanceCoverage, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var cov BalanceCoverage
	var oldest, newest sql.NullString
	err := d.db.QueryRow(
		`SELECT COUNT(*), MIN(fetched_at), MAX(fetched_at) FROM balances WHERE network = ?`, network,
	).Scan(&cov.Swept, &oldest, &newest)
	if err != nil {
		return cov, err
	}
	cov.OldestFetch, cov.NewestFetch = oldest.String, newest.String

	known, err := d.countKnownAddresses(network)
	if err != nil {
		return cov, err
	}
	cov.Known = known
	return cov, nil
}

// knownAddressesQuery is every address this instance has seen on a chain, in
// any role.
//
// This is what "total accounts" has to mean here. gno offers no way to
// enumerate the auth module over RPC, so a true account count is not available
// at any price, and reporting this one under that name would be a different
// number wearing it. The page says "addresses seen on chain" for the same
// reason.
const knownAddressesQuery = `
	SELECT caller AS address FROM calls WHERE %s
	UNION SELECT creator FROM package_submissions WHERE %s
	UNION SELECT caller FROM msg_runs WHERE %s
	UNION SELECT from_address FROM bank_sends WHERE %s
	UNION SELECT to_address FROM bank_sends WHERE %s`

func (d *DB) knownAddressesSQL(network string) string {
	f := d.networkFilter("network", network)
	q := knownAddressesQuery
	for strings.Contains(q, "%s") {
		q = strings.Replace(q, "%s", f, 1)
	}
	return q
}

func (d *DB) countKnownAddresses(network string) (int, error) {
	var n int
	err := d.db.QueryRow(`SELECT COUNT(*) FROM (` + d.knownAddressesSQL(network) + `)`).Scan(&n)
	return n, err
}

// KnownAddresses lists the addresses a balance sweep should cover.
//
// Ordered so that the ones already cached and oldest come first, and the ones
// never fetched come first of all. A sweep bounded by `limit` therefore makes
// progress on a cold cache instead of refreshing the same head of the list
// forever.
func (d *DB) KnownAddresses(network string, limit int) ([]string, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	rows, err := d.db.Query(`
		SELECT a.address FROM (`+d.knownAddressesSQL(network)+`) a
		LEFT JOIN balances b ON b.network = ? AND b.address = a.address
		WHERE a.address <> ''
		ORDER BY (b.fetched_at IS NULL) DESC, b.fetched_at ASC
		LIMIT ?`, network, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []string{}
	for rows.Next() {
		var addr string
		if err := rows.Scan(&addr); err != nil {
			return nil, err
		}
		out = append(out, addr)
	}
	return out, rows.Err()
}

// AccountPopulation is the summary bar: how many addresses exist, and how many
// were active recently.
//
// The active counts re-deduplicate from active_addr_rollup rather than summing
// it, because counts cannot be re-aggregated: an address active on three days
// of a week is one weekly active address, not three.
type AccountPopulation struct {
	Known   int `json:"known"`
	Daily   int `json:"daily_active"`
	Weekly  int `json:"weekly_active"`
	Monthly int `json:"monthly_active"`
}

func (d *DB) AccountPopulation(network string) (AccountPopulation, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var p AccountPopulation
	known, err := d.countKnownAddresses(network)
	if err != nil {
		return p, err
	}
	p.Known = known

	// The rollup is rebuilt on a timer, so on a fresh instance it is empty. A
	// confident zero there would read as "nobody uses this chain", which is the
	// failure mode the rollups' own comment warns about, so an empty rollup
	// falls back to counting live instead.
	var rollupRows int
	if err := d.db.QueryRow(`SELECT COUNT(*) FROM active_addr_rollup WHERE ` +
		d.networkFilter("network", network)).Scan(&rollupRows); err != nil {
		return p, err
	}

	for _, w := range []struct {
		hours int
		into  *int
	}{{24, &p.Daily}, {24 * 7, &p.Weekly}, {24 * 30, &p.Monthly}} {
		cut := time.Now().UTC().Add(-time.Duration(w.hours) * time.Hour)
		n, err := d.countActiveSince(network, cut, rollupRows > 0)
		if err != nil {
			return p, err
		}
		*w.into = n
	}
	return p, nil
}

// countActiveSince counts distinct addresses active in a window.
//
// Distinct, not summed. An address active on three days of a week is one weekly
// active address, and that is exactly why active_addr_rollup stores tuples
// rather than counts: every granularity is an exact re-deduplication instead of
// an over-count.
func (d *DB) countActiveSince(network string, cut time.Time, useRollup bool) (int, error) {
	var n int
	if useRollup {
		// The rollup buckets by UTC hour as "YYYY-MM-DDTHH", which is
		// zero-padded and therefore orders lexicographically.
		err := d.db.QueryRow(`SELECT COUNT(DISTINCT addr) FROM active_addr_rollup
		                       WHERE `+d.networkFilter("network", network)+` AND bucket >= ?`,
			cut.Format("2006-01-02T15")).Scan(&n)
		return n, err
	}
	since := cut.Format(time.RFC3339)
	f := d.networkFilter("network", network)
	err := d.db.QueryRow(`
		SELECT COUNT(*) FROM (
			SELECT caller AS a FROM calls WHERE `+f+` AND block_time >= ?
			UNION SELECT caller FROM msg_runs WHERE `+f+` AND block_time >= ?
			UNION SELECT from_address FROM bank_sends WHERE `+f+` AND block_time >= ?
		)`, since, since, since).Scan(&n)
	return n, err
}

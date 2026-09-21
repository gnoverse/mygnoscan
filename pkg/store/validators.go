package store

import (
	"time"
)

// Validator activity, from the blocks this instance has synced.
//
// ⚠️ Two disjoint address spaces live under the word "validator" here, and
// conflating them is the bug that made loadValMonikers never match anything:
//
//   - The **consensus** address proposes blocks. It is what `blocks.proposer_id`
//     interns and what gnockpit reports, and the two agree exactly (verified
//     against mainnet 2026-09-20: all four addresses matched).
//   - The **operator** address registers in `r/gnops/valopers`. It is what
//     `valoper_registrations` holds, and nothing on chain maps it to the
//     consensus key.
//
// Everything in this file is keyed on the consensus address. The registration
// log is deliberately left alone rather than joined to it.

// ProposedBlock is one block a validator proposed.
type ProposedBlock struct {
	Height int    `json:"height"`
	Time   string `json:"time"`
	NumTxs int    `json:"num_txs"`
}

// ValidatorActivity is what the local block history says about one proposer.
type ValidatorActivity struct {
	Address string `json:"address"`
	Blocks  int    `json:"blocks"`
	Txs     int    `json:"txs"`
	First   string `json:"first_block_time,omitempty"`
	Last    string `json:"last_block_time,omitempty"`
	FirstH  int    `json:"first_height,omitempty"`
	LastH   int    `json:"last_height,omitempty"`
}

// ValidatorActivityAll returns per-proposer totals over the synced range.
func (d *DB) ValidatorActivityAll(network string) (map[string]ValidatorActivity, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	rows, err := d.db.Query(`
		SELECT p.address, COUNT(*), COALESCE(SUM(b.num_txs), 0),
		       MIN(b.time), MAX(b.time), MIN(b.height), MAX(b.height)
		  FROM blocks b JOIN proposers p ON p.id = b.proposer_id AND p.network = b.network
		 WHERE ` + d.networkFilter("b.network", network) + `
		 GROUP BY p.address`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]ValidatorActivity{}
	for rows.Next() {
		var a ValidatorActivity
		if err := rows.Scan(&a.Address, &a.Blocks, &a.Txs, &a.First, &a.Last, &a.FirstH, &a.LastH); err != nil {
			return nil, err
		}
		out[a.Address] = a
	}
	return out, rows.Err()
}

// ValidatorBlocks returns a validator's most recently proposed blocks.
func (d *DB) ValidatorBlocks(network, address string, limit int) ([]ProposedBlock, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	rows, err := d.db.Query(`
		SELECT b.height, b.time, b.num_txs
		  FROM blocks b JOIN proposers p ON p.id = b.proposer_id AND p.network = b.network
		 WHERE b.network = ? AND p.address = ?
		 ORDER BY b.height DESC LIMIT ?`, network, address, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []ProposedBlock{}
	for rows.Next() {
		var b ProposedBlock
		if err := rows.Scan(&b.Height, &b.Time, &b.NumTxs); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ValidatorShareSeries returns each validator's daily share of proposed blocks.
//
// This is the closest thing to a voting-power timeline that is actually
// derivable here. gno's valset changes through governance rather than through
// delegation, and no historical valset is stored anywhere: gnockpit reports the
// current one only. What the local block history *does* show is when a proposer
// started and stopped appearing, which is the same event seen from the other
// side, and it needs no data this instance does not already have.
//
// A share rather than a count, because a chain with variable block time makes
// raw counts per day incomparable.
type ValidatorSharePoint struct {
	Day    string             `json:"day"`
	Shares map[string]float64 `json:"shares"`
	Blocks int                `json:"blocks"`
}

func (d *DB) ValidatorShareSeries(network string, days int) ([]ValidatorSharePoint, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	since := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)
	rows, err := d.db.Query(`
		SELECT substr(b.time, 1, 10) AS day, p.address, COUNT(*)
		  FROM blocks b JOIN proposers p ON p.id = b.proposer_id AND p.network = b.network
		 WHERE b.network = ? AND b.time >= ?
		 GROUP BY day, p.address ORDER BY day ASC`, network, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	byDay := map[string]map[string]int{}
	order := []string{}
	for rows.Next() {
		var day, addr string
		var n int
		if err := rows.Scan(&day, &addr, &n); err != nil {
			return nil, err
		}
		if byDay[day] == nil {
			byDay[day] = map[string]int{}
			order = append(order, day)
		}
		byDay[day][addr] = n
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]ValidatorSharePoint, 0, len(order))
	for _, day := range order {
		counts := byDay[day]
		total := 0
		for _, n := range counts {
			total += n
		}
		pt := ValidatorSharePoint{Day: day, Blocks: total, Shares: map[string]float64{}}
		for addr, n := range counts {
			if total > 0 {
				pt.Shares[addr] = float64(n) / float64(total)
			}
		}
		out = append(out, pt)
	}
	return out, nil
}

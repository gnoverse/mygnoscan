package indexer

import (
	"context"
	"fmt"
	"strings"
)

// coinFlowMaxTransactions bounds one CoinFlows walk.
//
// The walk itself is unbounded by construction, so something has to say when a
// realm's history stops being a page and starts being a dataset. At 5 legs per
// transaction this is a quarter of a million rows, which no browser wants and
// no reader asked for; past it the answer is the local ledger, not a longer
// round trip to the indexer.
const coinFlowMaxTransactions = 50000

// CoinFlows fetches every successful transaction that moved native coins to or
// from one of the given addresses, newest first.
//
// Why this is possible at all: the bank keeper emits a TransferEvent{from, to,
// coins} on every sendCoins (tm2/pkg/sdk/bank/keeper.go), a realm's own banker
// moves included, and the tx-indexer exposes it as a queryable union member with
// all three fields. mygnoscan already walked past these events without reading
// them. Summing the legs touching a realm's address reproduces the chain's own
// answer to bank/balances exactly: verified on mainnet 2026-09-22, where
// r/.../bubblerumble2's 36 legs net to 80846853914 ugnot, to the ugnot.
//
// Two things do *not* emit one, both deliberately (SendCoinsUnrestricted): gas
// collection, and the storage-deposit charge and refund. Neither touches a
// realm's banker, which is why the reconstruction above is exact rather than
// approximate. For an address that *signs* transactions the same sum is short by
// exactly its gas spend, so do not reuse this as an account-page balance.
//
// ⚠️ This used to be one unpaginated query, which put a hard ceiling of
// ElementCap (10,000) transactions on any realm's whole history, and a realm
// that crossed it did not fail, it returned its newest 10,000 and reported
// `truncated`, at which point the derived total silently stopped matching
// bank/balances. r/…/bubblerumble3 reached 28% of that ceiling in its first day.
// So it walks the height cursor instead, the same way the syncer does, and
// `truncated` now means only that a realm is past coinFlowMaxTransactions.
func (c *Client) CoinFlows(ctx context.Context, addrs []string) (txs []Transaction, truncated bool, err error) {
	clauses := make([]string, 0, len(addrs)*2)
	for _, a := range addrs {
		if a == "" {
			continue
		}
		clauses = append(clauses,
			fmt.Sprintf(`{ TransferEvent: { to: { eq: "%s" } } }`, gqlEscape(a)),
			fmt.Sprintf(`{ TransferEvent: { from: { eq: "%s" } } }`, gqlEscape(a)))
	}
	if len(clauses) == 0 {
		return nil, false, nil
	}

	// success: true rather than filtering afterwards. A reverted transaction
	// still reports its events, and counting those would invent transfers that
	// never settled.
	where := fmt.Sprintf(`
		success: { eq: true }
		response: { events: { _or: [%s] } }`, strings.Join(clauses, " "))
	const fields = `
		hash
		block_height
		response { events { __typename ... on TransferEvent { from to coins } } }`

	// Ascending, because that is the only direction a height cursor can resume
	// in: the indexer's cap keeps the *first* rows it iterated, so ASC hands
	// back the contiguous next page above the cursor and DESC would hand back
	// the newest rows and orphan everything between. transactionsFromHeight
	// owns that detail, including dropping the trailing height a cap may have
	// cut in half.
	var cursor *int
	for {
		page, capped, err := c.transactionsFromHeight(ctx, cursor, where, fields)
		if err != nil {
			// Partial data plus an error beats nothing: the page can still draw
			// the history it has and say where it stops. The one case
			// transactionsFromHeight cannot resume from is a single block
			// holding more than ElementCap matching transactions, and it
			// reports that as an error rather than an empty page.
			if len(txs) > 0 {
				return reversed(txs), true, nil
			}
			return nil, false, err
		}
		txs = append(txs, page...)
		if !capped {
			break
		}
		if len(txs) >= coinFlowMaxTransactions {
			return reversed(txs), true, nil
		}
		if len(page) == 0 {
			break
		}
		next := page[len(page)-1].BlockHeight
		cursor = &next
	}
	// Newest first, which is what every caller renders and what the display
	// sort then assumes. Reversing an ascending walk keeps the within-block
	// ordering the old DESC query produced, where a later transaction in a
	// block sorts above an earlier one.
	return reversed(txs), false, nil
}

// reversed flips a slice in place and returns it. The walk collects ascending
// because the cursor demands it; every reader wants descending.
func reversed(txs []Transaction) []Transaction {
	for i, j := 0, len(txs)-1; i < j; i, j = i+1, j-1 {
		txs[i], txs[j] = txs[j], txs[i]
	}
	return txs
}

package indexer

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

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
// Returns `truncated` when the indexer capped the result set. The rows are the
// newest ones in that case, because the order is DESC: a caller anchoring on the
// live balance and walking backwards still gets a correct recent history, and
// only loses how far back it reaches.
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
	q := fmt.Sprintf(`{
		getTransactions(
			where: {
				success: { eq: true }
				response: { events: { _or: [%s] } }
			}
			order: { heightAndIndex: DESC }
		) {
			hash
			block_height
			response { events { __typename ... on TransferEvent { from to coins } } }
		}
	}`, strings.Join(clauses, " "))

	var result struct {
		GetTransactions []Transaction `json:"getTransactions"`
	}
	if err := c.query(ctx, q, nil, &result); err != nil {
		// A cap is partial data plus an error, not an empty answer. Reporting
		// the rows and saying they are short beats reporting nothing: the page
		// can still draw the recent history and say where it stops.
		if errors.Is(err, ErrQueryTooLarge) {
			return result.GetTransactions, true, nil
		}
		return nil, false, err
	}
	return result.GetTransactions, false, nil
}

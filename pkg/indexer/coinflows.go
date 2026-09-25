package indexer

import (
	"context"
	"errors"
	"fmt"
)

// Native coin movement, for the local ledger that stores it.
//
// The chain emits a TransferEvent{from, to, coins} on every sendCoins
// (tm2/pkg/sdk/bank/keeper.go), a realm's own banker moves included, which is
// what makes a package's balance reconstructable at all: summed over a realm's
// address the legs reproduce bank/balances exactly (ADR 0034).
//
// ⚠️ Two things do *not* emit one, both deliberately (SendCoinsUnrestricted):
// gas collection, and the storage-deposit charge and refund. Neither touches a
// realm's banker, which is why the realm case is exact; for an address that
// *signs*, the same sum is short by exactly its gas spend.
//
// This file used to also carry CoinFlows, a per-request walk of one package's
// whole history that the defi tab called on every cold read. It is gone: the
// syncer writes coin_transfers as it goes and the endpoint reads SQL, so the
// only walk left is the historical backfill below.

// ErrNoTransferEvents reports that this chain's indexer does not define the
// TransferEvent type, so native coin movement cannot be asked about at all.
// It describes the chain, not a failure: an empty answer and an unanswerable
// question are different facts and a page must not render them the same way.
var ErrNoTransferEvents = errors.New("this chain's indexer does not index transfer events")

// coinTransferFields is the selection set for the coin ledger's backfill.
//
// It selects `success` even though the filter always pairs with
// `success: { eq: true }`. Leaving the field out does not give you a field you
// ignore, it gives you one that is *false on every row*, and a consumer that
// checks it then silently stores nothing: the first live backfill run walked
// 10,000 real mainnet transactions and wrote 0 legs for exactly that reason.
// TestTransferQueriesSelectSuccessAndNotOnlyFilterOnIt is the guard.
const coinTransferFields = `
		hash
		block_height
		success
		response { events { __typename ... on TransferEvent { from to coins } } }`

// CoinTransferWindow fetches the successful transactions carrying any
// TransferEvent in the half-open height range (after, before), oldest first.
//
// The filter is `TransferEvent: {}` with no address inside it, which the indexer
// accepts as "carries one of these" rather than rejecting as an empty predicate.
// That is what makes backfilling this ledger cheap: mainnet's whole transfer
// history is ~11,700 legs, against ~250,000 transactions if you walked
// everything looking for them.
//
// ⚠️ **Bounded by height, not merely by cursor, and that is the point.**
// transactionsFromHeight asks for everything above a cursor and takes whatever
// the element cap returns, which here is a 10,000-row response every time. Run
// against `indexer.gno.land` that earns a **403** within a couple of minutes,
// from a residential IP and from val1 alike (both observed 2026-09-23), and
// because the breaker is per-client and the syncer has one, that 403 stops
// packages and calls syncing too. A window keeps each request small enough that
// the backfill is invisible next to the traffic the syncer already makes.
//
// Truncation inside a window is still possible and still handled: the trailing
// height is dropped, `truncated` is reported, and the caller resumes from the
// last complete height rather than skipping to the window's end.
func (c *Client) CoinTransferWindow(ctx context.Context, after, before int) ([]Transaction, bool, error) {
	if !c.SupportsTransferEvents(ctx) {
		return nil, false, ErrNoTransferEvents
	}
	q := fmt.Sprintf(`{
		getTransactions(
			where: {
				block_height: { gt: %d, lt: %d }
				success: { eq: true }
				response: { events: { _or: [{ TransferEvent: {} }] } }
			}
			order: { heightAndIndex: ASC }
		) { %s }
	}`, after, before, coinTransferFields)

	var result struct {
		GetTransactions []Transaction `json:"getTransactions"`
	}
	err := c.query(ctx, q, nil, &result)
	if err != nil && !errors.Is(err, ErrQueryTooLarge) {
		return nil, false, err
	}
	if err == nil {
		return result.GetTransactions, false, nil
	}
	txs := dropTrailingHeight(result.GetTransactions)
	if len(txs) == 0 {
		return nil, false, fmt.Errorf(
			"a single block holds more than %d transfer transactions: %w", ElementCap, err)
	}
	return txs, true, nil
}

package indexer

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrNoTransferEvents reports that this chain's indexer does not define the
// TransferEvent type, so native coin movement cannot be asked about at all.
// It describes the chain, not a failure: an empty answer and an unanswerable
// question are different facts and a page must not render them the same way.
var ErrNoTransferEvents = errors.New("this chain's indexer does not index transfer events")

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
	// Asked before the query rather than learned from its failure. An indexer
	// that does not define TransferEvent rejects the *filter* too, with a
	// GraphQL validation error the reader then sees verbatim: on 2026-09-23
	// `/api/realm/defi?network=pearl` answered `Field "TransferEvent" is not
	// defined by type "NestedFilterEvent"`, which reads as a broken site rather
	// than as a chain that cannot be asked.
	if !c.SupportsTransferEvents(ctx) {
		return nil, false, ErrNoTransferEvents
	}

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
	// coinTransferFields, shared with the backfill. It selects `success` even
	// though the filter above already guarantees it: leaving the field out does
	// not give you a field you ignore, it gives you one that is *false on every
	// row*, and a consumer that checks it then silently stores nothing. The
	// first live backfill run walked 10,000 real transactions and wrote 0 legs
	// for exactly that reason.
	const fields = coinTransferFields

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

// coinTransferFields is the backfill's selection set.
//
// success, for the reason CoinFlows spells out above: a set that omits it hands
// back `success: false` on every row rather than nothing, and the consumer then
// silently stores none of them.
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

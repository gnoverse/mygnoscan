package httpapi

import (
	"net/http"
	"strings"

	"github.com/moul/mygnoscan/pkg/gnoaddr"
	"github.com/moul/mygnoscan/pkg/store"
)

// What a contract holds, and how it came to hold it.
//
// The realm page could say who called a realm, what it emitted, what it costs to
// store and what imports it, and nothing at all about money. For anything
// defi-shaped that is the first question: r/.../bubblerumble2 is a prize pool
// holding 80,846 GNOT and its page never mentioned a coin.
//
// Two ledgers, because a package owns two accounts and they are filled by
// different mechanisms:
//
//   - the banker, from TransferEvent (see indexer.CoinFlows). Complete for a
//     realm, and checkable: the sum equals what bank/balances reports.
//   - the storage deposit account, from the storage events already synced. Not
//     included here, because the storage tab already draws it.
//
// GRC20 positions come from the local transfer ledger rather than from a
// balanceOf call: one query instead of one RPC round trip per token, and it
// carries history, which balanceOf does not. The cost is that the ledger only
// saw what the syncer saw, which the response says out loud rather than leaving
// a reader to assume a floor is a figure.

// coinFlowLimit is the default page of flows, kept at what the endpoint always
// returned so an existing caller sees no change. The balance figures are
// computed over everything the indexer returned; only the page is cut, and the
// response says so.
const coinFlowLimit = 500

// coinFlowMaxLimit is the largest page ?flows_limit may ask for.
//
// 500 was the whole answer, not a page: there was no parameter to lift it and
// no offset to walk past it, so a realm with 3,095 legs showed 500 of them and
// the curve drawn on top covered only the window those 500 spanned
// (r/…/bubblerumble3, 2026-09-22, one day old). Measured on that realm, a leg
// is ~257 bytes of JSON, so this ceiling is ~1.3 MB raw and ~190 KB gzipped:
// enough that every realm on mainnet today fits in one request, and bounded
// enough that a whale cannot hand a browser an unbounded document. Past it,
// page with ?flows_offset.
const coinFlowMaxLimit = 5000

// tokenFlowLimit is the default page of GRC20 transfers, and tokenFlowMaxLimit
// the largest ?token_flows_limit may ask for. The same pair as the native side,
// and the same reasoning: the default is what the endpoint always returned, the
// ceiling keeps a whale from handing a browser an unbounded document.
const (
	tokenFlowLimit    = 500
	tokenFlowMaxLimit = 5000
)

// counterpartyLimit bounds the collapsed view of the same legs.
//
// A graph is not a table and does not get more informative past a few dozen
// nodes; it gets unreadable. Ranked by gross volume and cut here, with the
// full count reported beside it so a reader knows a tail was left out rather
// than guessing from the picture.
const counterpartyLimit = 50

type tokenPositionRow struct {
	store.TokenPosition
	Verified      bool   `json:"verified"`
	DisplaySymbol string `json:"display_symbol,omitempty"`
	Decimals      int    `json:"decimals,omitempty"`
}

type realmDefiResponse struct {
	Network               string `json:"network"`
	Path                  string `json:"path"`
	Address               string `json:"address"`
	StorageDepositAddress string `json:"storage_deposit_address,omitempty"`

	// Balance and StorageBalance are live RPC reads, verbatim coin strings.
	// Empty is ambiguous on its own: it is both "the read failed" and "the
	// account holds nothing", and the second is the common case for a realm
	// whose money is all in GRC20. BalanceKnown and StorageBalanceKnown are
	// what separate them, so the frontend can render zero as zero and reserve
	// "unknown" for a read that genuinely did not happen.
	Balance             string `json:"balance"`
	BalanceKnown        bool   `json:"balance_known"`
	StorageBalance      string `json:"storage_balance"`
	StorageBalanceKnown bool   `json:"storage_balance_known"`

	// DerivedUgnot is the reconstruction: every transfer leg, summed. LiveUgnot
	// is what the chain says. Both are reported rather than one being chosen,
	// because the two agreeing is the evidence that the history below is
	// complete, and the two disagreeing is a fact worth seeing (an indexer gap,
	// or a genesis allocation, which emits no event).
	DerivedUgnot int64 `json:"derived_ugnot"`
	LiveUgnot    int64 `json:"live_ugnot"`
	// Truncated says the walk stopped before the realm's history did, so the
	// oldest flows are missing and DerivedUgnot is short by them.
	Truncated bool `json:"truncated"`
	// FlowsShown and FlowsTotal say whether the page is the whole story.
	// FlowsOffset is where the page starts, counting back from the newest leg,
	// so a caller can walk the rest without re-deriving the window.
	FlowsShown  int                   `json:"flows_shown"`
	FlowsTotal  int                   `json:"flows_total"`
	FlowsOffset int                   `json:"flows_offset"`
	Flows       []store.RealmCoinFlow `json:"flows"`

	// Counterparties is the same history collapsed by who was at the other
	// end, newest-heaviest first, and CounterpartiesTotal is how many there
	// were before the cut. Always over every leg, never over the page above.
	Counterparties      []store.RealmCoinParty `json:"counterparties"`
	CounterpartiesTotal int                    `json:"counterparties_total"`

	Tokens     []tokenPositionRow    `json:"tokens"`
	TokenFlows []store.TokenTransfer `json:"token_flows"`
	// The same three the native side reports, and for the same reason: a page
	// that is exactly as long as the limit is indistinguishable from a complete
	// history without a total beside it. r/gnoswap/pool and r/gnoswap/router
	// both sat on 500 with nothing saying so.
	TokenFlowsShown  int `json:"token_flows_shown"`
	TokenFlowsTotal  int `json:"token_flows_total"`
	TokenFlowsOffset int `json:"token_flows_offset"`
	// TokenLedgerFrom is the oldest transfer in the local GRC20 ledger on this
	// chain. Anything a package received before it is invisible here, so a
	// position is a floor rather than a figure whenever this is later than the
	// package's own deploy.
	TokenLedgerFrom string `json:"token_ledger_from,omitempty"`
}

// HandleRealmDefi answers the realm page's defi tab.
func (a *API) HandleRealmDefi(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	path := strings.TrimRight("gno.land/"+r.PathValue("path"), "/")

	// Denominated figures, so the all-networks blend is refused the same way
	// /api/storage refuses it: summing two chains' ugnot invents a number.
	if network == "" {
		jsonError(w, "balances are denominated per chain: select a network", 400)
		return
	}
	addr := gnoaddr.Derive(path)
	if addr == "" {
		jsonError(w, "no account for path: "+path, 404)
		return
	}
	depositAddr := gnoaddr.DeriveStorageDeposit(path)

	resp := realmDefiResponse{
		Network:               network,
		Path:                  path,
		Address:               addr,
		StorageDepositAddress: depositAddr,
		Flows:                 []store.RealmCoinFlow{},
		Tokens:                []tokenPositionRow{},
		TokenFlows:            []store.TokenTransfer{},
	}

	rpcURL := a.rpcURLFor(network)
	bal, balErr := fetchBalanceErr(r.Context(), addr, rpcURL)
	resp.Balance, resp.BalanceKnown = bal, balErr == nil
	resp.LiveUgnot = store.ParseUgnot(bal)
	if depositAddr != "" {
		sbal, sErr := fetchBalanceErr(r.Context(), depositAddr, rpcURL)
		resp.StorageBalance, resp.StorageBalanceKnown = sbal, sErr == nil
	}

	// Read from the local ledger, not walked from the indexer.
	//
	// This used to fetch every transaction carrying a TransferEvent that touched
	// either account, on every cold request, and do the arithmetic in Go. It cost
	// 2.0s and 2.6s on a realm with 3,104 legs and, more tellingly, **2.1 to 2.4
	// seconds on three realms with none at all**: the latency was the round trip
	// plus resolving a chain-wide event filter, so no response cache could reach
	// it (ADR 0043). coin_transfers now holds the same legs, filled forward by
	// the sync walk and backward by a one-off backfill.
	stats, err := a.db.RealmCoinStatsFor(network, addr, depositAddr)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	resp.DerivedUgnot = stats.DerivedUgnot
	resp.FlowsTotal = stats.Legs
	// Truncated changed meaning with the source and kept its name, because it
	// answers the same reader question: may the reconstruction below be short?
	// It used to mean "the indexer capped the query"; it now means the ledger's
	// historical backfill has not finished on this chain.
	resp.Truncated = !a.db.CoinBackfillDone(network)

	// The page is cut here and nowhere else. DerivedUgnot above is a SUM over
	// every row, because it is the figure compared against the chain's own
	// balance and a paged sum would report a gap that is an artefact of the
	// page size.
	q := r.URL.Query()
	flowOffset := intParam(q, "flows_offset", 0, 0)
	if flowOffset > stats.Legs {
		flowOffset = stats.Legs
	}
	page, err := a.db.RealmCoinFlowsFor(network, addr, depositAddr,
		intParam(q, "flows_limit", coinFlowLimit, coinFlowMaxLimit), flowOffset)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	resp.FlowsOffset = flowOffset
	resp.FlowsShown = len(page)
	resp.Flows = page

	parties, partyTotal, err := a.db.RealmCoinPartiesFor(network, addr, depositAddr, counterpartyLimit)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	resp.Counterparties = parties
	resp.CounterpartiesTotal = partyTotal

	positions, err := a.db.TokenPositions(network, addr)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	for _, p := range positions {
		row := tokenPositionRow{TokenPosition: p}
		if meta, ok := a.registry.Tokens[p.Token]; ok {
			row.Verified = meta.Verified
			row.DisplaySymbol = meta.Symbol
			row.Decimals = meta.Decimals
		}
		resp.Tokens = append(resp.Tokens, row)
	}
	if len(resp.Tokens) > 0 {
		q := r.URL.Query()
		limit := intParam(q, "token_flows_limit", tokenFlowLimit, tokenFlowMaxLimit)
		offset := intParam(q, "token_flows_offset", 0, 0)

		total, err := a.db.HolderTransferCount(network, addr)
		if err != nil {
			jsonError(w, err.Error(), 500)
			return
		}
		// Clamped before the query and reported back, so a caller paging by
		// adding what it holds to what it was given terminates instead of
		// walking forever asking for nothing.
		if offset > total {
			offset = total
		}
		flows, err := a.db.HolderTransfers(network, addr, limit, offset)
		if err != nil {
			jsonError(w, err.Error(), 500)
			return
		}
		resp.TokenFlows = flows
		resp.TokenFlowsTotal = total
		resp.TokenFlowsOffset = offset
		resp.TokenFlowsShown = len(flows)
		resp.TokenLedgerFrom = a.db.EarliestTokenTransfer(network)
	}

	JSONResponse(w, resp)
}

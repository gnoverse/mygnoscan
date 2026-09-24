package httpapi

import (
	"net/http"
	"sort"
	"strings"

	"github.com/moul/mygnoscan/pkg/gnoaddr"
	"github.com/moul/mygnoscan/pkg/indexer"
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

// counterparty is one other end of the realm's money, with the two directions
// kept apart.
//
// The same legs as Flows, collapsed. It exists because the table answers "what
// happened" and cannot answer "who", which on anything defi-shaped is the
// question: r/.../bubblerumble3 has 3,095 legs and twelve counterparties, and
// only the second number is a thing a person can hold in their head.
//
// Computed over every leg the walk returned, never over the page, for the same
// reason DerivedUgnot is: a per-counterparty total summed over 500 of 3,095
// legs is wrong for exactly the heaviest counterparties, which are the ones a
// reader is looking at.
type counterparty struct {
	// Address is the other end. Empty means the chain itself (a fee collector,
	// a genesis allocation), not a missing value.
	Address string `json:"address"`
	// Sent is what this account put into the realm; Received is what the realm
	// paid it. Kept apart rather than netted, because an account that moved
	// 400 GNOT each way is not the same actor as one that never moved any, and
	// a single net figure cannot tell them apart.
	Sent     int64 `json:"sent"`
	Received int64 `json:"received"`
	// Net is Received minus Sent: this account's own profit and loss, not the
	// realm's. The sign is the opposite of coinFlow.Amount's, which is written
	// from the realm's point of view, and getting the two confused turns a
	// player who is up into one who is down.
	Net       int64  `json:"net"`
	Legs      int    `json:"legs"`
	FirstSeen string `json:"first_seen,omitempty"`
	LastSeen  string `json:"last_seen,omitempty"`
}

type coinFlow struct {
	TxHash      string `json:"tx_hash"`
	BlockHeight int    `json:"block_height"`
	BlockTime   string `json:"block_time,omitempty"`
	// Account names which of the package's two accounts this leg touched.
	Account string `json:"account"`
	// Counterparty is the other end. Empty is possible and means the chain
	// itself (a fee collector, genesis), not a missing value.
	Counterparty string `json:"counterparty"`
	// Amount is signed ugnot: positive is received. Coins is the chain's own
	// string, kept because a transfer can carry a denom that is not ugnot and
	// Amount cannot represent it.
	Amount int64  `json:"amount"`
	Coins  string `json:"coins"`
}

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
	// Empty means the read failed, which is not the same as a zero balance and
	// must not be rendered as one.
	Balance        string `json:"balance"`
	StorageBalance string `json:"storage_balance"`

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
	FlowsShown  int        `json:"flows_shown"`
	FlowsTotal  int        `json:"flows_total"`
	FlowsOffset int        `json:"flows_offset"`
	Flows       []coinFlow `json:"flows"`

	// Counterparties is the same history collapsed by who was at the other
	// end, newest-heaviest first, and CounterpartiesTotal is how many there
	// were before the cut. Always over every leg, never over the page above.
	Counterparties      []counterparty `json:"counterparties"`
	CounterpartiesTotal int            `json:"counterparties_total"`

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
		Flows:                 []coinFlow{},
		Tokens:                []tokenPositionRow{},
		TokenFlows:            []store.TokenTransfer{},
	}

	rpcURL := a.rpcURLFor(network)
	resp.Balance = fetchBalance(r.Context(), addr, rpcURL)
	resp.LiveUgnot = store.ParseUgnot(resp.Balance)
	if depositAddr != "" {
		resp.StorageBalance = fetchBalance(r.Context(), depositAddr, rpcURL)
	}

	if client := a.clientFor(network); client != nil {
		txs, truncated, err := client.CoinFlows(r.Context(), []string{addr, depositAddr})
		if err != nil {
			jsonError(w, err.Error(), 500)
			return
		}
		resp.Truncated = truncated
		a.stampBlockTimes(r.Context(), network, client, txs)
		flows, derived := coinFlowsFor(txs, addr, depositAddr)
		resp.DerivedUgnot = derived
		resp.FlowsTotal = len(flows)

		// The page is cut here and nowhere else: DerivedUgnot above is the sum
		// over every leg, because it is the figure compared against the chain's
		// own balance and a paged sum would report a gap that is an artefact of
		// the page size.
		q := r.URL.Query()
		page, offset := pageFlows(flows,
			intParam(q, "flows_offset", 0, 0),
			intParam(q, "flows_limit", coinFlowLimit, coinFlowMaxLimit))
		resp.FlowsOffset = offset
		resp.FlowsShown = len(page)
		resp.Flows = page

		parties, total := counterpartiesFor(flows)
		resp.Counterparties = parties
		resp.CounterpartiesTotal = total
	}

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

// pageFlows cuts one page out of the newest-first flows, and reports where it
// actually started.
//
// Returns the clamped offset rather than the requested one, because the reader
// pages by adding the rows they hold to the offset they were given: handing back
// an offset past the end would have them walk forever asking for nothing. The
// slice is never nil, so the field marshals as [] rather than null.
func pageFlows(flows []coinFlow, offset, limit int) ([]coinFlow, int) {
	if offset > len(flows) {
		offset = len(flows)
	}
	page := flows[offset:]
	if len(page) > limit {
		page = page[:limit]
	}
	if page == nil {
		page = []coinFlow{}
	}
	return page, offset
}

// coinFlowsFor turns the transactions into one signed row per transfer leg,
// newest first, and returns the net ugnot across every leg.
//
// The net is computed over every leg the query returned, not over the rows that
// survive the display limit: it is the figure compared against the chain's own
// balance, and comparing a truncated sum would report a gap that is an artefact
// of the page size.
//
// Only the banker's legs count toward the net. The storage deposit account's
// legs are carried for the table (they are how a deposit was funded) but summing
// both into one figure would compare the sum of two accounts against the balance
// of one. In practice the deposit account has no legs at all: the charge and the
// refund go through SendCoinsUnrestricted, which emits nothing.
func coinFlowsFor(txs []indexer.Transaction, addr, depositAddr string) ([]coinFlow, int64) {
	flows := []coinFlow{}
	var net int64
	for _, tx := range txs {
		if tx.Response == nil {
			continue
		}
		for _, ev := range tx.Response.Events {
			if ev.Typename != "TransferEvent" {
				continue
			}
			var account, counterparty string
			var sign int64
			switch {
			case ev.To == addr:
				account, counterparty, sign = "banker", ev.From, 1
			case ev.From == addr:
				account, counterparty, sign = "banker", ev.To, -1
			case depositAddr != "" && ev.To == depositAddr:
				account, counterparty, sign = "storage deposit", ev.From, 1
			case depositAddr != "" && ev.From == depositAddr:
				account, counterparty, sign = "storage deposit", ev.To, -1
			default:
				// A leg of a transaction that touched this realm somewhere else.
				// One transaction can carry many transfers and only some of them
				// are ours.
				continue
			}
			amount := sign * store.ParseUgnot(ev.Coins)
			if account == "banker" {
				net += amount
			}
			flows = append(flows, coinFlow{
				TxHash:       tx.Hash,
				BlockHeight:  tx.BlockHeight,
				BlockTime:    tx.BlockTime,
				Account:      account,
				Counterparty: counterparty,
				Amount:       amount,
				Coins:        ev.Coins,
			})
		}
	}
	// The query already orders by height descending, and the legs within one
	// transaction arrive in execution order. A stable sort keeps both: it only
	// fixes the case where the indexer returned something else.
	sort.SliceStable(flows, func(i, j int) bool {
		return flows[i].BlockHeight > flows[j].BlockHeight
	})
	return flows, net
}

// counterpartiesFor collapses the legs by who was at the other end.
//
// Banker legs only, which is the same rule DerivedUgnot follows. The storage
// deposit account is a different account with a different story (the storage
// tab draws it), and folding its legs in here would attribute a deposit to the
// realm's treasury. In practice it contributes nothing at all: the charge and
// the refund both go through SendCoinsUnrestricted, which emits no event.
//
// Returns the top counterpartyLimit by gross volume, and the total count before
// the cut.
func counterpartiesFor(flows []coinFlow) ([]counterparty, int) {
	byAddr := map[string]*counterparty{}
	for _, f := range flows {
		if f.Account != "banker" {
			continue
		}
		c := byAddr[f.Counterparty]
		if c == nil {
			c = &counterparty{Address: f.Counterparty}
			byAddr[f.Counterparty] = c
		}
		// Amount is signed from the realm's point of view: positive is the
		// realm receiving, which is this account sending.
		if f.Amount >= 0 {
			c.Sent += f.Amount
		} else {
			c.Received += -f.Amount
		}
		c.Legs++
		// The flows are newest first, so the first time an address is seen is
		// its last activity and the last time is its first.
		if c.LastSeen == "" {
			c.LastSeen = f.BlockTime
		}
		if f.BlockTime != "" {
			c.FirstSeen = f.BlockTime
		}
	}

	out := make([]counterparty, 0, len(byAddr))
	for _, c := range byAddr {
		c.Net = c.Received - c.Sent
		out = append(out, *c)
	}
	// Gross, not net: an account that moved a lot in both directions is a major
	// counterparty even when it comes out level, and ranking by net would bury
	// it under someone who moved a thousandth as much one way.
	gross := func(c counterparty) int64 { return c.Sent + c.Received }
	sort.SliceStable(out, func(i, j int) bool {
		if gross(out[i]) != gross(out[j]) {
			return gross(out[i]) > gross(out[j])
		}
		return out[i].Address < out[j].Address
	})
	total := len(out)
	if len(out) > counterpartyLimit {
		out = out[:counterpartyLimit]
	}
	return out, total
}

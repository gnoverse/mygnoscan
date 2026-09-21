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

// coinFlowLimit bounds the flows returned to the page. The balance figures are
// computed over everything the indexer returned; only the table and the curve
// are cut, and the response says when they were.
const coinFlowLimit = 500

// tokenFlowLimit is the same bound for the GRC20 side.
const tokenFlowLimit = 500

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
	// Truncated says the indexer capped the transfer query, so the history
	// reaches back only as far as the oldest flow below.
	Truncated bool `json:"truncated"`
	// FlowsShown and FlowsTotal say whether the table is the whole story.
	FlowsShown int        `json:"flows_shown"`
	FlowsTotal int        `json:"flows_total"`
	Flows      []coinFlow `json:"flows"`

	Tokens     []tokenPositionRow    `json:"tokens"`
	TokenFlows []store.TokenTransfer `json:"token_flows"`
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
		if len(flows) > coinFlowLimit {
			flows = flows[:coinFlowLimit]
		}
		resp.FlowsShown = len(flows)
		resp.Flows = flows
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
		flows, err := a.db.HolderTransfers(network, addr, tokenFlowLimit)
		if err != nil {
			jsonError(w, err.Error(), 500)
			return
		}
		resp.TokenFlows = flows
		resp.TokenLedgerFrom = a.db.EarliestTokenTransfer(network)
	}

	JSONResponse(w, resp)
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

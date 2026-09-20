package httpapi

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/moul/mygnoscan/pkg/store"
)

// The assets views, over the GRC20 transfer ledger.
//
// Deliberately without any of Mintscan's money columns. GNOT is not listed
// anywhere and there is no oracle on chain, so price, market cap and "total
// value" would each be a number this explorer invented. What is left is the
// part that is actually knowable and actually wanted: supply, who holds it, and
// how much of it moves.

// assetRow is one token, with whatever the registry knows about it merged in.
type assetRow struct {
	store.TokenSummary
	// Verified and DisplaySymbol come from the curated registry. Anyone can
	// deploy a realm called `gns`, so this is the only thing separating the
	// real one from a lookalike, and it is a human's claim rather than
	// anything the chain says.
	Verified      bool   `json:"verified"`
	DisplaySymbol string `json:"display_symbol,omitempty"`
	DisplayName   string `json:"display_name,omitempty"`
	Decimals      int    `json:"decimals,omitempty"`
}

type assetsResponse struct {
	Network string     `json:"network"`
	Assets  []assetRow `json:"assets"`
}

func (a *API) decorate(t store.TokenSummary) assetRow {
	row := assetRow{TokenSummary: t}
	if meta, ok := a.registry.Tokens[t.Token]; ok {
		row.Verified = meta.Verified
		row.DisplaySymbol = meta.Symbol
		row.DisplayName = meta.Name
		row.Decimals = meta.Decimals
	}
	return row
}

func (a *API) HandleAssets(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	summaries, err := a.db.TokenSummaries(network)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	rows := make([]assetRow, len(summaries))
	for i, t := range summaries {
		rows[i] = a.decorate(t)
	}
	JSONResponse(w, assetsResponse{Network: network, Assets: rows})
}

type assetDetailResponse struct {
	Network   string                 `json:"network"`
	Asset     assetRow               `json:"asset"`
	Holders   []store.TokenHolder    `json:"holders"`
	Transfers []store.TokenTransfer  `json:"transfers"`
	Supply    []store.BlockTimePoint `json:"supply_series"`
}

// HandleAsset serves one token.
//
// Single network, because a balance reconstruction is per chain: the same token
// path can exist on two chains and its holders there are different people.
func (a *API) HandleAsset(w http.ResponseWriter, r *http.Request) {
	network := a.singleNetwork(r)
	if network == "" {
		jsonError(w, "no network configured", 404)
		return
	}
	token, err := url.PathUnescape(r.PathValue("token"))
	if err != nil || strings.TrimSpace(token) == "" {
		jsonError(w, "no token given", 400)
		return
	}

	summaries, err := a.db.TokenSummaries(network)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	var found *store.TokenSummary
	for i := range summaries {
		if summaries[i].Token == token {
			found = &summaries[i]
			break
		}
	}
	if found == nil {
		jsonError(w, "token not found on this network", 404)
		return
	}

	holders, err := a.db.TopHolders(network, token, 50)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	transfers, err := a.db.TokenTransfers(network, token, 50)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	series, err := a.db.TokenSupplyOverTime(network, token, 90)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}

	JSONResponse(w, assetDetailResponse{
		Network:   network,
		Asset:     a.decorate(*found),
		Holders:   holders,
		Transfers: transfers,
		Supply:    series,
	})
}

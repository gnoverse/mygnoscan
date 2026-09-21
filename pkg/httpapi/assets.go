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

// HandleAssets lists every asset, optionally narrowed to one realm.
//
// `?realm=<package path>` is what the realm page asks for, and the reason it
// has to ask at all: a realm is a *container* of tokens, not a token. Six of
// the twelve assets on mainnet (measured 2026-09-22) are issued by one realm,
// `.../gnomi/padv3`, and grc20factory exists to have many. So the realm page
// cannot show "the token"; it shows the list, and each row routes to its own
// page keyed on the full event key.
func (a *API) HandleAssets(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	summaries, err := a.db.TokenSummaries(network)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	realm := strings.TrimSpace(r.URL.Query().Get("realm"))
	rows := make([]assetRow, 0, len(summaries))
	for _, t := range summaries {
		if realm != "" && t.PkgPath != realm {
			continue
		}
		rows = append(rows, a.decorate(t))
	}
	JSONResponse(w, assetsResponse{Network: network, Assets: rows})
}

// HandleAssetSearch matches assets by their event key, for the search box.
//
// Separate from /api/search rather than folded into it: that endpoint answers
// with a flat array of packages and every caller reads it as one, and a token
// is not a package. A realm holding six tokens would also collapse to a single
// package row, which is exactly the confusion this whole surface exists to
// undo.
func (a *API) HandleAssetSearch(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		jsonError(w, "missing q parameter", 400)
		return
	}
	found, err := a.db.SearchTokens(network, q, 8)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	rows := make([]assetRow, len(found))
	for i, t := range found {
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
	// Siblings are the other assets issued by the same realm. Empty for the
	// common one-token realm; six rows for `.../gnomi/padv3`. Carried on the
	// detail response rather than fetched separately so the page can say
	// "this realm issues N of these" without a second round trip, which is
	// the fact a reader arriving from the realm most needs.
	Siblings []assetRow `json:"siblings"`
	// Ledger is the span of transfer history this network actually has, so
	// the page can state the window it computed over. See TokenLedgerWindow.
	Ledger store.TokenLedgerWindow `json:"ledger"`
	// Realm is the issuing package, when the event key carried a path and the
	// package is known to this index. Nil for a token that emits a bare
	// symbol, and for one whose realm was deployed outside the synced range.
	Realm *assetRealm `json:"realm,omitempty"`
}

// assetRealm is the issuing package, reduced to what the token page shows.
type assetRealm struct {
	Path        string `json:"path"`
	Creator     string `json:"creator"`
	BlockHeight int    `json:"block_height"`
	BlockTime   string `json:"block_time,omitempty"`
	NumFiles    int    `json:"num_files"`
	Calls       int    `json:"call_count"`
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
	ledger, err := a.db.TokenLedgerWindow(network)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}

	siblings := []assetRow{}
	if found.PkgPath != "" {
		for i := range summaries {
			if summaries[i].PkgPath == found.PkgPath && summaries[i].Token != token {
				siblings = append(siblings, a.decorate(summaries[i]))
			}
		}
	}

	// Best-effort: a token whose key is a bare symbol has no realm to look up,
	// and a realm deployed before this index's range is not in `packages`.
	// Neither is an error on a page about the token.
	var realm *assetRealm
	if found.PkgPath != "" {
		if detail, err := a.db.GetPackageDetail(network, found.PkgPath); err == nil && detail != nil {
			realm = &assetRealm{
				Path:        detail.Path,
				Creator:     detail.Creator,
				BlockHeight: detail.BlockHeight,
				BlockTime:   detail.BlockTime,
				NumFiles:    detail.NumFiles,
				Calls:       detail.Calls,
			}
		}
	}

	JSONResponse(w, assetDetailResponse{
		Network:   network,
		Asset:     a.decorate(*found),
		Holders:   holders,
		Transfers: transfers,
		Supply:    series,
		Siblings:  siblings,
		Ledger:    ledger,
		Realm:     realm,
	})
}
